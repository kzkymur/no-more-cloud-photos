# Core HTTP API and CLI contract

Status: normative implementation contract for issue #2. The live architecture and Worker designs remain the product requirements; this document fixes details that those designs intentionally left open. Storage and concurrency invariants are in [State, storage, workers, and operations](state-and-storage.md).

## 1. Protocol conventions

- The API is JSON over HTTPS inside the Tailnet, except `POST /media`, which is `multipart/form-data`. File bytes are served by the separate File Server, never by these API routes.
- JSON request and response media type is `application/json; charset=utf-8`. Unknown JSON fields are rejected with `400 invalid_request`; omitted optional fields and explicit `null` are not interchangeable unless stated.
- IDs are lowercase UUIDv4 strings. Timestamps are UTC RFC 3339 strings with available fractional precision. Durations are integer milliseconds; sizes are integer bytes; SHA-256 values are 64 lowercase hexadecimal characters.
- Every non-empty response has `X-Request-ID`. Clients may supply a valid 1-64 character ASCII `X-Request-ID`; otherwise Core creates one. Secrets, file-system absolute paths, lease tokens, and storage credentials never appear in responses or logs.
- Collection `limit` values must be decimal integers. The common default is 50 and maximum is 200, except `/changes`, whose default is 100 and maximum is 1,000.
- A route not supporting the method returns `405` with `Allow`. An unacceptable response media type returns `406`; a non-JSON body on a JSON route returns `415`.

### 1.1 Error schema

Every API error uses this shape:

```json
{
  "error": {
    "code": "invalid_cursor",
    "message": "cursor is invalid for this query",
    "request_id": "01J...",
    "details": {}
  }
}
```

`code` is stable and machine-readable. `message` is diagnostic and not stable. `details` is always an object and may be empty. Validation errors use `details.fields`, an object from field name to a stable reason string. Unhandled failures return `500 internal_error`; dependency unavailability returns `503 unavailable` and may include integer `Retry-After` seconds. Internal errors and SQL/tool output are not exposed.

### 1.2 Opaque cursors

Cursors are versioned, base64url-encoded, authenticated tokens. They contain the last ordering tuple and a canonical fingerprint of all filters, but clients must not decode or construct them. A cursor may be used only on the route and filters that created it. Invalid encoding, signature, version, route, or filter binding returns `400 invalid_cursor`.

- Media order is `taken_at DESC NULLS LAST, id DESC`; the token carries the null class, `taken_at`, and `id`. A null `taken_at` is never replaced by `created_at`.
- Job order is `created_at DESC, id DESC`; the token also binds `status` and `media_id`.
- Changes order is the durable feed position described in [section 7](#7-change-feed). It does not use a PostgreSQL sequence value allocated outside the mutation's serialization lock.
- `next_cursor` is `null` at a known end for media/jobs. Changes return the input position when no event is available because their end is open-ended.

Cursor pagination is keyset pagination. It provides no historical snapshot across pages: concurrent state changes may affect later pages, while a stable dataset has neither gaps nor duplicates.

## 2. Resource schemas

The following tables define complete response fields. `T?` means JSON `null` is allowed. Fields are not omitted unless explicitly stated.

### 2.1 Rendition

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Immutable rendition ID. |
| `media_id` | UUID | Owning media. |
| `job_target_id` | UUID | Target that produced it. |
| `profile` | object | `{id, key, version}` where `id` is UUID, `key` is a profile key, and `version` is integer >= 1. |
| `mime_type` | string | Normalized output MIME. |
| `size_bytes` | integer | Non-negative persisted file size. |
| `width`, `height` | integer? | Positive pixel dimensions when applicable. |
| `duration_ms` | integer? | Non-negative duration for moving output. |
| `sha256` | string | Persisted output digest. |
| `file_url` | string | Absolute immutable HTTPS File Server URL. |
| `created_at` | timestamp | Publication time. |

Historical `purge_after` and processor parameters are internal/history fields, not part of this read model. `GET /renditions/{id}` returns this same object while the row/file is retained.

### 2.2 Media

`MediaSummary` has:

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Stable logical media ID. |
| `mime_type` | string | Content-detected normalized original MIME. |
| `original_filename` | string? | Normalized client basename as defined by the upload contract; metadata only and never used as a path. |
| `size_bytes` | integer | Original size. |
| `width`, `height` | integer? | Probed source dimensions. |
| `duration_ms` | integer? | Probed source duration. |
| `taken_at` | timestamp? | Normalized capture instant. |
| `taken_at_source` | string | `embedded_offset`, `default_timezone`, or `unknown`. |
| `taken_at_timezone` | string? | IANA zone used only for offsetless source time. |
| `created_at` | timestamp | Acceptance time. |
| `deleted_at` | timestamp? | Logical deletion time. |
| `purge_after` | timestamp? | Snapshotted media retention deadline. |
| `rendition` | Rendition? | Current rendition for the requested profile key, or `null` while unavailable. |

`MediaDetail` has every summary field except singular `rendition`, plus:

| Field | Type | Meaning |
|---|---|---|
| `current_renditions` | Rendition[] | Current outputs for all profile keys, ordered by key. Missing keys have no entry. |
| `jobs` | Job[] | Newest first; capped at 100. Use `/jobs?media_id=` for complete history. |

Deleted media remains addressable by `GET /media/{id}` and the original/display/thumbnail metadata routes until physical purge. Its `deleted_at` is non-null. This is deliberate: logical deletion removes it from the default list but does not pretend the retained bytes are gone. After purge, media and rendition routes return `404`; durable jobs and change tombstones remain.

### 2.3 Original

| Field | Type | Meaning |
|---|---|---|
| `id`, `media_id` | UUID | Original and media IDs. |
| `mime_type`, `size_bytes`, `sha256` | string, integer, string | Content metadata. |
| `original_filename` | string? | Normalized client basename as defined by the upload contract. |
| `file_url` | string | Immutable File Server URL. |
| `created_at` | timestamp | Persistence time. |

Original EXIF is intentionally not returned by v1; it is retained in storage for future use.

### 2.4 Job and target

`Job` has:

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Job ID. |
| `type` | string | `transform` or `purge`. |
| `status` | string | `queued`, `running`, `succeeded`, `failed`, or `cancelled`. |
| `media_id` | UUID | Live media ID, or the immutable snapshot after purge. |
| `original_id` | UUID? | Null after original removal. |
| `attempts`, `max_attempts` | integer | Claim count and configured positive limit. |
| `available_at` | timestamp | Earliest next claim. |
| `started_at` | timestamp? | First execution start; for purge it is never cleared. |
| `finished_at` | timestamp? | Terminal transition time. |
| `error` | object? | `{code, message}` for the latest terminal/attempt error. |
| `cancelled_at` | timestamp? | Purge cancellation time. |
| `cancel_reason` | string? | Currently `media_restored`. |
| `created_at`, `updated_at` | timestamp | Audit times. |
| `targets` | JobTarget[] | Empty for purge; profile-key order for transform. |

`JobTarget` has `id`, `profile: {id,key,version}`, `status` (`pending`, `succeeded`, or `failed`), `attempts`, nullable `error: {code,message}`, nullable `rendition_id`, and `updated_at`. Lease token/expiry are deliberately private.

### 2.5 Profile

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Identifies one immutable version. |
| `key` | string | Stable use name such as `standard`, `thumbnail`, or a custom key. |
| `version` | integer | Positive, unique within key. |
| `status` | string | `draft`, `active`, or `retired`. |
| `input_mime_types` | string[] | Non-empty normalized exact MIME values; no wildcards. |
| `parameters_schema_version` | integer | Positive processor contract version. |
| `processor` | string | Registered processor capability name. |
| `parameters` | object | Validated MIME-specific recipes and recorded encoder settings. |
| `created_at`, `activated_at`, `retired_at` | timestamp, timestamp?, timestamp? | Lifecycle audit times. |

## 3. Upload

### `POST /media`

Required headers are `Content-Type: multipart/form-data; boundary=...` and `Idempotency-Key`. The key is 1-128 visible ASCII characters, excluding whitespace and control characters. A request has exactly one part named `file`; other parts, nested multipart, `Content-Transfer-Encoding`, empty files, and more than one `file` are `400 invalid_multipart`. The part's declared content type is advisory and never drives MIME selection.

The optional client filename is normalized once and the resulting value is used both for `original_filename` and the idempotency request hash. RFC 8187 `filename*` takes precedence over `filename` and must declare UTF-8; invalid percent encoding, invalid UTF-8, NUL, DEL, or Unicode control characters return `400 invalid_filename`. Core converts backslashes to slashes, takes only the final non-empty path component (removing browser-supplied paths such as `C:\\fakepath\\`), and normalizes that basename to Unicode NFC. An absent or empty filename becomes JSON/database `null`; `.` and `..` are also treated as null. The normalized basename must be at most 255 UTF-8 bytes. No trimming, case folding, or character replacement is performed. It is retained only as display metadata and never enters a storage key or process argument.

Core streams to a same-filesystem temporary file while computing SHA-256. It never buffers the complete upload in memory. Limits are:

- Original file maximum: 20 GiB (`21,474,836,480` bytes).
- Multipart framing/header allowance: 1 MiB beyond the file limit; excess returns `413 upload_too_large` and the connection body is not reused.
- Request headers: 64 KiB and 10 seconds.
- Body idle timeout: 120 seconds with no bytes read. Hard upload duration: 24 hours. Either returns `408 upload_timeout` if a response remains possible.
- After the complete body is read, Core has 30 seconds to write the JSON response. If the connection disappears after commit, the stored idempotency result remains authoritative for retry.
- Metadata probe after the complete temporary upload, but before final acceptance publication: 60 seconds and 1 GiB address-space limit per probe process. Decode/probe failure, malformed input, decompression-bomb policy violation, or unsupported codec returns `422 invalid_media`; unknown/unregistered content MIME returns `415 unsupported_media_type`. Malformed optional EXIF alone does not reject otherwise decodable media.

The canonical idempotency request hash is SHA-256 over a versioned, length-prefixed encoding of the file SHA-256, exact byte size, and normalized filename, with a distinct marker for null. Multipart boundaries, `filename` spelling before the normalization above, and advisory MIME headers are excluded, so a correctly reconstructed retry matches.

On first success, the response is `201`:

```json
{
  "media": { "id": "...", "mime_type": "image/jpeg", "original_filename": "a.jpg", "size_bytes": 1, "width": 1, "height": 1, "duration_ms": null, "taken_at": null, "taken_at_source": "unknown", "taken_at_timezone": null, "created_at": "...", "deleted_at": null, "purge_after": null, "rendition": null },
  "job": null
}
```

`job` is `null` when no active profile accepts the detected MIME. Otherwise one transform job and one target for each matching active profile are committed with Media and Original. The upload response does not wait for conversion.

Idempotency behavior is persistent across restart and has no v1 expiry:

- After streaming and fingerprinting, concurrent requests for the same scope/key are serialized by a PostgreSQL advisory lock. The winner's acceptance transaction inserts the completed idempotency row; a waiter then reads that row. A process death releases the lock and leaves either a complete result or no result, never a committed placeholder.
- The first terminal acceptance result (`201` or SHA duplicate `409`) and its exact response body are committed with the idempotency record.
- A retry with the same key and canonical request hash returns the stored status/body and `Idempotency-Replayed: true`, even if the media has since been deleted. It creates no file, row, job, or event.
- The same key with a different canonical hash returns `409 idempotency_conflict` with `details.original_request_hash`; concurrent requests are serialized and obey the same rule.
- Transient `408`, `413`, `415`, `422`, `500`, and `503` responses are not stored. Published-but-unreferenced files after a crash are reconciled as described in the storage contract.

Original SHA uniqueness is independent of idempotency. A new key whose file SHA already exists, including for logically deleted media, returns:

```json
{
  "error": {
    "code": "duplicate_media",
    "message": "original content already exists",
    "request_id": "...",
    "details": { "existing_media_id": "...", "deleted": false }
  }
}
```

Other status codes are `400 missing_idempotency_key|invalid_idempotency_key|invalid_multipart`, `408 upload_timeout`, `413 upload_too_large`, `415 unsupported_media_type`, `422 invalid_media`, `500 internal_error`, and `503 unavailable`.

## 4. Media and file metadata routes

### `GET /media`

Query parameters: `profile` defaults to `standard`; it accepts any existing profile key. `deleted` is `exclude` (default), `only`, or `include`. `cursor` and `limit` follow section 1.2. Unknown parameters are rejected.

`200` response:

```json
{ "items": [], "next_cursor": null }
```

Each item is `MediaSummary`; `rendition` is the current successful output for the requested key or `null`. Switching the active profile version does not hide the old current output. Invalid filters/cursor/limit return `400`; an unknown profile key returns `400 invalid_profile`.

### `GET /media/{id}`

Returns `200` with `MediaDetail`, including logically deleted media, or `404 media_not_found`. Malformed UUID is `400 invalid_id`.

### `GET /media/{id}/display`

Returns `200` with a Rendition for current key `standard`. It returns `409 rendition_not_ready` when media exists but no successful current standard output exists, and `404 media_not_found` after purge. Logically deleted media is still readable until purge.

### `GET /media/{id}/thumbnail`

Identical to `/display` for key `thumbnail`.

### `GET /media/{id}/original`

Returns `200` with Original, including for logically deleted media, or `404 media_not_found`/`404 original_not_found`. This explicit route is the only API response that exposes the original URL.

### `GET /renditions/{id}`

Returns `200` with Rendition or `404 rendition_not_found`. A rendition removed by historical cleanup or media purge is `404`, while its target remains visible in job history.

## 5. Job and profile routes

### `GET /jobs`

Optional filters are one exact `status` and `media_id`; `cursor` and `limit` follow section 1.2. `200` is `{ "items": Job[], "next_cursor": string|null }`. Invalid status, UUID, cursor, limit, or unknown parameter is `400`. Purge jobs remain queryable after media purge through `media_id` snapshot.

### `GET /jobs/{id}`

Returns `200` with Job, `400 invalid_id`, or `404 job_not_found`.

### `GET /profiles`

Optional `status` is one of `draft`, `active`, or `retired`. Results are ordered by `key ASC, version DESC`. `200` is `{ "items": Profile[] }`; invalid filters are `400`.

## 6. Delete, restore, and purge

All three operations lock the Media row. Their database state transition and change event are one transaction.

### `DELETE /media/{id}`

First deletion sets `deleted_at` and snapshots `media_retention_days` into `purge_after`; `null` means no automatic purge and `0` means eligible on the next scanner run. Repetition is idempotent and does not recalculate either field. Returns `200` with MediaDetail, `400 invalid_id`, or `404 media_not_found`.

### `POST /media/{id}/restore`

The body must be absent or `{}`. Restore clears `deleted_at` and `purge_after` and changes every not-started queued purge job for that media to `cancelled` in the same transaction. It returns `200` with MediaDetail.

It returns `409 media_not_deleted` if active, and `409 purge_already_started` if any purge job has non-null `started_at`, including while that job is queued for retry or failed. It returns `404 media_not_found` after purge.

### `DELETE /media/{id}/purge`

Only logically deleted media is accepted. It creates a targetless purge job and returns `202` with Job. If a queued or running purge already exists, it returns the same job with `202` and creates nothing. A failed purge returns `409 purge_failed_use_retry` with its job ID and must be retried through the administrative CLI; a cancelled purge does not block a new purge after a later delete. It returns `409 media_not_deleted` for active media and `404 media_not_found` after purge. Actual deletion is asynchronous.

Automatic retention uses this exact same enqueue operation and duplicate prevention. Restore, explicit/automatic enqueue, and the Worker's first purge start are serialized by the same Media lock.

## 7. Change feed

### `GET /changes`

Query parameters are `cursor` and `limit` only. A missing cursor means position zero. `200` is:

```json
{
  "items": [
    {
      "id": "...",
      "position": "42",
      "type": "media_upsert",
      "media_id": "...",
      "occurred_at": "...",
      "media": {},
      "reason": "upload"
    }
  ],
  "next_cursor": "..."
}
```

`position` is a decimal string representing a positive signed 64-bit logical position. Event `type` and payload are:

| Type | `media` | `reason` |
|---|---|---|
| `media_upsert` | MediaDetail projection without `jobs`; current rendition URLs included. | `upload`, `rendition_current`, or `restore`. |
| `media_deleted` | `null` | `logical_delete`. |
| `media_purged` | `null` | `physical_purge`. |

Every event also retains `media_id` and `occurred_at`, so purge remains actionable after all Media/Rendition rows are gone. Events are immutable, may be delivered more than once when a consumer retries a cursor, and are not automatically deleted in v1. Consumers must process a page durably before storing `next_cursor`. Invalid cursor/limit is `400`; there is no `410 cursor_expired` while v1 retention remains disabled.

The mutation transaction allocates the position only while holding the singleton feed-state row lock and inserts the event before commit. Therefore another mutation cannot allocate a later visible position and commit ahead of it. See the proof and rejected sequence design in the [storage contract](state-and-storage.md#7-durable-change-feed).

## 8. Service probes

- `GET /healthz` returns `200 {"status":"ok"}` whenever the process event loop is healthy; it does not query dependencies.
- `GET /readyz` returns `200 {"status":"ready"}` only when configuration is valid, migrations are current, PostgreSQL is reachable, and the storage root passes the required access check. Otherwise it returns `503` using the common error schema.

These routes expose no build secrets or dependency addresses.

## 9. File Server contract

`file_url` is `https://<configured-file-host>/files/<file-key>`. The host is configured, never inferred from an untrusted request header. File keys are exactly:

```text
originals/{shard}/{original_id}/original.{ext}
renditions/{shard}/{original_id}/{job_target_id}/{rendition_id}.{ext}
```

IDs and extensions are lowercase canonical values; `shard` is the first two hex digits of `original_id` after removing hyphens. A URL never contains a filename, profile key, absolute path, query token, or mutable alias. Regeneration creates a new rendition ID, file, and URL.

Nginx exposes only the `originals/` and `renditions/` namespaces from a read-only mount. It permits `GET` and `HEAD`, including one or more valid byte ranges, and returns normal `200`, `206`, `304`, `404`, or `416` semantics. Other methods are `405`; traversal, encoded separators/dot segments, malformed percent encoding, symlink escape, and directory requests are rejected without revealing paths. Directory listing is off. Responses use the stored MIME, `Accept-Ranges: bytes`, `X-Content-Type-Options: nosniff`, and `Cache-Control: private, max-age=31536000, immutable`.

Tailnet access is the initial authorization boundary: no signed URL, application cookie, CDN, or per-file ACL is promised. Known URLs can work after logical deletion until physical deletion. Core API and File Server binding, routing, and firewall rules expose only loopback/Tailnet and never use Funnel.

## 10. Administrative CLI

The binary is `nmcp-admin` using Go `flag`. All commands accept `--json`; human-readable output is default. `--json` writes one JSON value to stdout and diagnostics to stderr. Exit codes are `0` success, `2` usage/validation, `3` confirmation required, `4` conflict or nothing eligible, `5` partial batch failure, and `1` operational/internal failure. Secrets are never accepted directly on the command line.

| Command | Contract |
|---|---|
| `migrate status [--json]` | Compare embedded migration checksums and versions with the database without changing it. Exit `4` when pending migrations exist and `1` on checksum drift or an unreachable database. |
| `migrate up [--json]` | Under the migration advisory lock, apply every pending embedded forward migration in version order. Refuse checksum drift, unknown applied versions, or downgrade. This non-interactive command is the deployment migration entry point. |
| `config show [--json]` | Show typed non-secret system configuration. |
| `config set --name NAME --value VALUE [--json]` | Set one of `deleted_media_retention_days`, `superseded_rendition_retention_days`, `default_timezone`, `db_backup_interval_hours`, `db_backup_retention_days`; literal `null` is allowed only for the two retention values. Validate before transaction. |
| `profile create --file FILE [--json]` | Create immutable draft profile from JSON; validate processor/schema/MIME recipes. |
| `profile activate --id UUID [--json]` | Under a key-scoped advisory lock, activate a draft only when its version is greater than every version ever activated for that key, atomically retiring the prior active version. A lower/equal stale draft returns conflict and remains draft. Never backfills media. |
| `profile retire --id UUID [--json]` | Retire an active profile; historical targets remain valid. |
| `regenerate --profile KEY (--media-id UUID OR --all) [--batch-size N] [--resume TOKEN] [--dry-run] [--yes] [--json]` | Create paged transform jobs pinned to the active profile version. `--all` requires `--yes` unless dry-run. Persisted batch state makes `--resume` safe and prevents duplicate target work. |
| `jobs retry (--job-id UUID OR --status failed) [--type TYPE] [--additional-attempts N] [--batch-size N] [--resume TOKEN] [--dry-run] [--yes] [--json]` | Requeue the same failed job and only failed/pending targets; succeeded targets are immutable. `--additional-attempts` is a positive integer, defaults to 3, and atomically sets `max_attempts = max(max_attempts, attempts + N)` before requeue so an exhausted job is claimable without erasing its attempt history. Bulk retry requires confirmation. |
| `check [--scope VALUE] [--json]` | `--scope` accepts `files`, `db`, or `all`. Read-only reconciliation classifies temporary, orphan, missing, checksum mismatch, and invariant violations. Never deletes. |
| `repair --report UUID [--quarantine-dir PATH] [--dry-run] --yes [--json]` | Apply an explicit saved check report under maintenance lock. Ambiguous/orphan files are quarantined, not immediately deleted. |
| `cleanup [--kind VALUE] [--dry-run] --yes [--json]` | `--kind` accepts `renditions`, `media`, `backups`, or `all`. Run due cleanup using the same locks/rechecks as automatic cleanup. |
| `backup run [--json]` / `backup list [--json]` | Run one database backup or list persistent history. |
| `restore --backup-id UUID --yes [--json]` | Enter maintenance, restore and validate into the configured database, run DB/file reconciliation, and remain in maintenance until `maintenance resume`. |
| `maintenance status [--json]` / `maintenance resume --yes [--json]` | Inspect or explicitly leave maintenance after successful checks. |

Mutating commands take a named PostgreSQL advisory lock appropriate to their operation. A conflicting operation fails rather than running concurrently. Every mutation writes an admin audit record containing command, actor/host, sanitized arguments, timestamps, result, and affected IDs. Batch order is stable by Media ID; checkpoint and active profile ID are persisted, so activation during a batch cannot change its pinned version.

## 11. Deferred measurements

This contract fixes formats and behavioral boundaries, not unmeasured codec quality. AVIF/AV1 quality values, bit depth, metadata retention, animated WebP compatibility, tone-map tuning, representative processing time, and peak memory are selected and recorded by issues #11-#13 and verified by #21 (with real-device follow-up in #22). Implementations must not silently substitute H.264 or reduce the required formats while those measurements are pending.
