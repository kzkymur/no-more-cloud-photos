# State, storage, workers, and operations contract

Status: normative implementation contract for issue #2. HTTP representations and CLI syntax are in [Core HTTP API and CLI](core-api.md).

## 1. Component and transaction boundaries

Core API, Core Worker, and the Nginx File Server are separate processes in one Core responsibility. PostgreSQL owns metadata and coordination. The storage root owns originals and renditions. Nginx mounts that root read-only; API and Worker write through the storage library. Viewer/Uploader consume Core JSON and fetch bytes directly from Nginx. Searcher consumes `/changes`; Share API, Photo App, Search Index, and Index Worker implementation are outside this repository.

An upload is accepted only after the durable original, Media/Original rows, idempotency result, applicable transform job/targets, and change event exist. Conversion is asynchronous. PostgreSQL and the filesystem cannot be one atomic transaction, so all file mutations use a recoverable publish protocol and reconciliation rather than claiming distributed atomicity.

## 2. Conceptual persistent model

All IDs are UUIDv4. Migrations are forward-only, embedded in the administrative binary with immutable version/checksum history, transactional where PostgreSQL permits, versioned, and protected by a migration advisory lock. `nmcp-admin migrate status` is read-only and `nmcp-admin migrate up` is the only deployment mutation entry point; API and Worker refuse readiness when migrations are pending or checksums drift. Rollback means restore a tested backup or deploy a forward repair migration; destructive down migrations are not shipped.

### 2.1 Core entities

- `media`: `id`, normalized source metadata, nullable `taken_at`, capture-source/timezone fields, `created_at`, nullable `deleted_at`, nullable `purge_after`. Index list order exactly as the API contract.
- `originals`: one row per Media (`media_id UNIQUE`), globally unique `sha256` across active and deleted media, byte/probe metadata, original EXIF JSON (`{}` when absent), and relative file key. `ON DELETE CASCADE` from Media.
- `profiles`: immutable `(key, version)` recipe, processor/schema capability, lifecycle timestamps/status. `UNIQUE(key, version)` and partial unique active row per key.
- `jobs`: `transform` or `purge`, status/attempt/availability/lease/error/audit columns, nullable `original_id ON DELETE SET NULL`, and non-null immutable `media_id_snapshot`. A transform has at least one target; purge has none. A partial unique constraint prevents more than one queued, running, or failed purge job per Media snapshot; failed purge is retried in place rather than replaced.
- `job_targets`: profile version pinned by `profile_id`, state/error/attempt audit, `UNIQUE(job_id, profile_id)`. Target and its job must resolve to the same Media as any published rendition.
- `renditions`: successful files only, linked to Media and `job_target_id`, with required digest/file metadata, `is_current`, and nullable `purge_after`. Index `media_id`. Historical rows may be removed by due cleanup; job/target history remains.
- `system_config`: singleton `id=1` with typed columns. Initial values are `deleted_media_retention_days=null`, `superseded_rendition_retention_days=null`, `default_timezone='Asia/Tokyo'`, `db_backup_interval_hours=24`, and `db_backup_retention_days=30`. Retention is nullable/non-negative; interval/backup retention are positive; timezone is a validated IANA name.

### 2.2 Required additions

| Table/state | Required durable content and constraint |
|---|---|
| `idempotency_requests` | Scope (`POST /media`) and key unique, canonical request hash, stored terminal HTTP status/body/media ID, and creation timestamp. Rows are immutable and have no automatic v1 expiry. A PostgreSQL advisory scope/key lock serializes contenders; the row is inserted only with the acceptance/duplicate transaction, so no stale placeholder state exists. |
| `change_feed_state` | Singleton `id=1`, transactional `last_position bigint >= 0`. |
| `change_events` | UUID, unique positive `position`, event type/reason, Media ID, immutable JSON payload/tombstone, occurrence timestamp. FK to Media is deliberately absent so purge cannot erase the event. No automatic v1 retention. |
| `backup_runs` | ID, status (`running`, `succeeded`, or `failed`), start/end, config snapshot, final relative path, byte size/digest, manifest JSON, PostgreSQL/tool versions, error, notification result. Only succeeded rows/files enter retention. |
| `maintenance_state` | Singleton mode (`normal` or `maintenance`), reason, owner, entered timestamp, nullable successful check report. Automatic deletion and normal writes are blocked in maintenance. |
| `admin_audit` | Command, actor/host, sanitized arguments, outcome, affected IDs, timestamps. |
| `admin_batches` | Operation, pinned profile/config, stable high-water/checkpoint, status (`running`, `succeeded`, `failed`, or `cancelled`), counts/error. Enables regeneration/retry resume without duplicates. |
| `reconciliation_reports` | Immutable classified findings and repair disposition, including quarantine paths relative to configured quarantine root. |

Purge cancellation is persistent in `jobs.status='cancelled'` with `cancelled_at` and `cancel_reason`; it is not represented by deleting a queued job. Purge history therefore survives restore and later re-delete.

## 3. File durability and paths

The only valid relative keys are:

```text
originals/{shard}/{original_id}/original.{ext}
renditions/{shard}/{original_id}/{job_target_id}/{rendition_id}.{ext}
```

`shard` derives from the original UUID. Extensions derive from normalized detected/encoded formats, never user names. Path construction accepts typed IDs and a closed extension enum, not arbitrary strings. Every operation opens beneath a configured root, rejects symlinks and root escape, and does not follow user-controlled path components.

Durable publish is: securely create each missing directory component beneath the storage root; after every new directory entry, `fsync` the new directory and its parent before proceeding; create an attempt-specific temporary file on the same filesystem with exclusive creation; stream/write; `fsync` file; validate size/digest/decode as applicable; rename without overwrite to the final unique key; `fsync` the containing directory; then commit the referencing database row. Thus a newly created shard/ID directory and the final rename both survive a crash. A collision is an invariant failure, never overwrite. Deletion is idempotent: missing is success after metadata/reconciliation checks.

If a crash occurs before rename, the aged temp is reported. If it occurs after rename but before DB commit or lease CAS, the final file is an orphan and is quarantined only by explicit repair. If DB commit exists but a file is missing, reconciliation reports a missing-file fault and does not fabricate success. Fault-injection hooks cover directory creation, new-directory fsync, parent-directory fsync, write, file fsync, rename, final-directory fsync, DB commit, and delete boundaries.

Original bytes are never modified. Probe/transform reads from the durable original and writes a new file.

## 4. Ingestion metadata

MIME comes from bounded content inspection, normalized to an exact registered MIME; multipart declarations and filename extensions are not trusted. Initial capability covers representative JPEG, PNG, GIF, HEIC/HEIF, supported RAW variants, WebP, BMP, MP4, and MOV/QuickTime. A container/family name is not a promise that every embedded codec or RAW variant decodes; unsupported capability is a stable error.

Probes collect dimensions, duration, EXIF/container metadata, and capture time under the limits in the API contract. Original metadata is retained even when no active profile matches, in which case no transform job is created.

Capture candidates are deterministic:

1. Still images: EXIF `DateTimeOriginal`, then `DateTimeDigitized`, then `DateTime`; each uses its corresponding offset tag when valid.
2. Video/QuickTime: selected primary video-stream creation time, then format/container creation time. Stream selection follows the processor's recorded primary-stream rule.
3. The first valid candidate with an explicit numeric offset is converted to UTC and recorded as `embedded_offset`; `default_timezone` is ignored.
4. The first valid offsetless local candidate is interpreted with the acceptance transaction's snapshotted `default_timezone`. A DST fold chooses the earlier UTC instant and records the fold decision in original metadata. A nonexistent DST-gap local time is rejected as a candidate rather than shifted; the next candidate is tried.
5. If no candidate is valid, `taken_at=null`, source is `unknown`, and timezone is null. Malformed metadata does not reject otherwise decodable media.

Changing the default timezone never rewrites existing Media. Original EXIF is `{}` when absent; source value, raw candidate, offset/fold decision, and applied timezone remain auditable.

## 5. State machines

### 5.1 Media and purge eligibility

```text
active --DELETE--> deleted --purge Worker first start--> purge_started --success--> absent
  ^                    |
  +------ restore -----+  (only before purge_started)
```

`active` means `deleted_at IS NULL`. Delete snapshots the then-current media retention; config changes are not retroactive. `deleted` can have a queued purge job. Restore clears deletion/deadline and cancels every queued, never-started purge. `purge_started` is derived from a purge job's non-null `started_at`; retries may put the job back in `queued`, but Media can never be restored after this point. Physical success deletes Media/Original/Rendition rows and files but retains jobs/targets, backup history, admin audit, and change tombstones.

### 5.2 Job

```text
queued -> running -> succeeded
   ^         |  \-> failed
   |         +----> queued     (expired lease/backoff, attempts remain)
   +-- failed              (explicit admin retry of same row)
queued -> cancelled        (never-started purge restored)
```

- Claim is a short transaction using `FOR UPDATE SKIP LOCKED`, `available_at <= now()`, `attempts < max_attempts`, and a fresh random lease token/expiry. External processes run with no DB transaction open.
- Heartbeat and every publication/state update compare job ID, `running`, token, and unexpired lease. The exact-expiry boundary is expired (`lease_expires_at <= database clock`). A stale Worker can finish a tool but cannot publish metadata/current state.
- Claim increments `attempts`. Recoverable failure/lease expiry sets `queued` with bounded exponential backoff plus jitter while attempts remain; otherwise `failed`. Admin retry requeues the same row without resetting attempts/history and atomically raises `max_attempts` to at least `attempts + additional_attempts` (positive, CLI default 3), so the claim predicate is immediately true. Concurrent retry is serialized by the Job row lock and recorded in admin audit.
- `started_at` is set once. For purge, it is set under the Media lock only after rechecking logical deletion and cancellation, before any physical delete, and is never cleared.
- Transform succeeds only when all targets succeed. It fails when no more retries remain for any failed target. Successfully published targets survive partial failure and are never rerun by ordinary retry.
- Purge `cancelled` and all `succeeded` states are terminal. Transform jobs have no public cancellation in v1.

### 5.3 Target

```text
pending -> succeeded
pending -> failed -> pending  (same job retry)
```

Targets do not need `running`; the parent lease owns execution. `succeeded` is terminal and has exactly one rendition. A failed attempt records a bounded/sanitized error. Profile ID never changes.

### 5.4 Profile

```text
draft -> active -> retired
```

There is at most one active version per key. Activation validates registered processor name, schema version, every exact input MIME, and a recipe/output for every input MIME. Unknown capability cannot activate. Under a transaction-scoped advisory lock derived from the normalized profile key, activation requires the draft version to be strictly greater than the maximum version with non-null `activated_at` for that key, then retires the old active row and activates the draft atomically. Thus activation versions are monotonic even across retirement; a stale lower-version draft remains draft and cannot become active. Activation does not regenerate existing media. Existing current rendition remains displayed until a pinned target for a newer version successfully replaces it. Retired versions remain executable for already-pinned targets.

### 5.5 Backup

```text
running -> succeeded
running -> failed
```

Only one run holds the backup advisory lock. `succeeded` requires completed custom-format dump, fsync/atomic final publish, digest, manifest, and validation. A failed or abandoned run is never considered a backup and its temp is reported/cleaned safely.

## 6. Transform publication and cleanup

Worker starts only registered tools, using argument arrays rather than a shell. Each tool runs in its own process group with bounded threads, input/output sizes, profile-specific timeout, and descendant termination on timeout/SIGTERM. Worker concurrency starts at one. systemd divides, rather than duplicates, the total initial CPU 2 / RAM 4 GiB budget across services.

Safety timeout ceilings start at 30 minutes for still/RAW, 2 hours for animation, and 24 hours for video. A profile may set a lower timeout. These ceilings are not codec performance claims and must be reviewed with the measurements in #11-#13/#21.

Required recipes are:

- `standard/v1` still: AVIF, aspect preserved, no crop, long edge at most 1920 px, no upscaling, orientation applied to pixels, SDR normalized to sRGB, HDR tone-mapped to SDR.
- `thumbnail/v1`: AVIF still, first frame for video/animation, aspect preserved, no crop, long edge at most 640 px, no upscaling.
- `standard/v1` animation: animated WebP preserving animation, alpha, frame timing, and loop behavior; orientation/color are normalized, aspect is preserved, there is no crop/upscale, and the long edge is at most 1920 px.
- `standard/v1` video: MP4 with AV1 video and AAC when audio exists, aspect preserved, no crop, long edge at most 1920 px, no upscaling, odd dimensions rounded down as needed, rotation applied, SDR BT.709, HDR tone-mapped to SDR. There is no automatic H.264 fallback.

Processor/tool versions, selected stream, recipes, encoder settings, color/tone-map choices, quality, bit depth, and metadata policy are saved in profile parameters/result audit. Exact quality/bit-depth/metadata values and measured resource limits are intentionally deferred to #11-#13 and #21, not guessed here.

Publication uses an attempt-specific rendition UUID. After durable file rename, one short transaction locks Media and validates the live lease, target/media/original relationship, and non-deleted media. It inserts Rendition, marks the target successful, and updates aggregate job state. It promotes the new row to current only when there is no current row for the key or the target profile version is greater than or equal to the current row's profile version. Promotion clears the old current, snapshots current `superseded_rendition_retention_days` into only that old row's `purge_after`, gives the new current a null deadline, and writes one `media_upsert` change event. An older-version result that finishes late succeeds as non-current history, receives the current superseded-retention deadline at insertion, and emits no indexing event; it can never roll current back. Direct current updates outside this operation are prohibited and checked by reconciliation.

If a newer profile fails, old current remains. Concurrent versions of one key serialize on Media; the greatest successfully published profile version wins regardless of commit order. For the same version, the last valid publication becomes current and the displaced row gets its retention snapshot.

Historical rendition cleanup selects due non-current rows, locks Media, and rechecks `is_current=false` before deleting. It removes file and row recoverably but retains target/job. A zero-day deadline means next scan, not synchronous delete. A null deadline means no automatic delete.

## 7. Durable change feed

Every externally indexable state mutation performs this operation inside its existing short database transaction:

1. Lock singleton `change_feed_state` with `SELECT ... FOR UPDATE`.
2. Increment its `last_position` transactionally.
3. Insert the immutable `change_events` row at that position, including an upsert snapshot or self-contained tombstone.
4. Commit the entity mutation, counter, and event together.

This intentionally serializes only short publication transactions, acceptable for the initial low-resource single-Core workload.

**Commit-order proof.** Transaction A holds the singleton row until commit or rollback. Transaction B cannot allocate the next position until A releases it. If A commits, A's event is visible before B allocates/commits. If A rolls back, its counter increment and event roll back and B reuses the next valid value. Therefore a reader that advances past position `p` cannot later discover a newly committed event with position `<= p`.

A plain `nextval()` is rejected: A can allocate 10 and pause, B can allocate 11 and commit, a consumer can advance to 11, and then A can commit 10 permanently behind the cursor. Ordering by event UUID, entity timestamp, or transaction start ID has the same visibility flaw. A post-commit dispatcher could safely assign positions to committed outbox rows, but adds lag and recovery machinery without benefit at the expected write rate.

The feed is durable, pull-based, and at-least-once. It does not mark events consumed globally. Logical delete and physical purge use tombstones independent of FK-deleted rows. V1 never expires events or idempotency records. Any future retention design must add a persisted consumer checkpoint/lease and a `410` full-resync contract before deleting history.

## 8. Purge, reconciliation, and maintenance

The retention scanner reads config each run, not a compiled timer interval. For due deleted Media it uses the same locked enqueue operation as the API. A purge Worker locks Media at first start, verifies it is still deleted and its job is not cancelled, sets `started_at` once, then idempotently removes all historical/current rendition files and the original. It records progress sufficient to retry each crash point. The final transaction deletes Media-owned rows, preserves job snapshot/history, and emits `media_purged`.

Transform publication locks Media and refuses deleted media; purge start uses the same lock, so transform versus purge cannot publish across the boundary. Missing files during purge are recorded but do not prevent converging database deletion when identity/path checks prove the expected object. Unexpected files are never deleted by wildcard traversal.

`check` classifies same-filesystem temp, final orphan, DB-referenced missing file, checksum/size mismatch, invalid current relation, and expired candidates. It is always dry/read-only. `repair` consumes an immutable report under maintenance lock; ambiguous orphans go to a configured same-filesystem quarantine with a manifest. Restored files and newly found orphans are not immediately deleted. During restore/reconciliation, all automatic media/rendition/backup deletion and mutating API operations are stopped until explicit resume.

## 9. Database backup and restore

Backup scheduling compares the last successful run to the current typed `db_backup_interval_hours`; changing the value affects the next eligibility calculation. systemd timers may wake the command but do not encode the interval. A PostgreSQL advisory lock rejects overlapping runs.

A run writes `pg_dump --format=custom` to a temp under the configured backup root, captures tool/server versions and config snapshot, fsyncs the file, verifies it can be listed/read, computes size/SHA-256, writes and fsyncs a manifest, and atomically renames both into a final run directory before marking `succeeded`. Paths and credentials come from environment/service credentials, not `system_config` or CLI arguments. Failure produces structured logs, nonzero exit, persistent failed history, and invokes the configured notification hook with sanitized metadata; missing/failing hooks are themselves recorded failures.

Cleanup uses the current `db_backup_retention_days`, considers only successful validated backups, and always retains at least the newest successful backup even when expired. Failed/temp artifacts follow separate safe cleanup and never cause deletion of the last success.

Restore requires explicit confirmation and maintenance mode. It verifies manifest/digest, restores with `pg_restore` into a fresh database, checks migration/config/invariants and API-readable rows, then reconciles DB references with originals/renditions. Cutover is explicit and the system remains in maintenance until `maintenance resume`; failure never resumes automatically. Recovery duration and effective RPO are measured in #19/#21.

Normal PostgreSQL WAL and `fsync` remain enabled. WAL archive/PITR and original-file cloud backup are out of v1 scope. A DB dump on the same physical HDD does not protect either metadata or originals from that HDD's failure, and operations documentation must say so.

## 10. Operational boundary

Ubuntu Server and systemd run `cmd/core-api`, `cmd/core-worker`, `nmcp-admin`, PostgreSQL, and Nginx. Core uses Go `net/http`, `pgx`, `os/exec`, and `flag`. Configuration is validated on startup; structured logs exclude secrets. API/Worker support graceful shutdown: stop intake/claims, terminate or release owned work safely, and close dependencies.

Nginx behavior and immutable URLs are normative in the [API contract](core-api.md#9-file-server-contract). Storage is read-only to Nginx. Backup, temp, quarantine, PostgreSQL, and credentials are outside the served roots. Tailnet TLS/ACL, host binding, routes, and firewall must all prevent non-Tailnet LAN/Internet bypass; Funnel is prohibited.

## 11. ADR decisions and alternatives

### ADR-001: Serialize feed positions with a transactional singleton lock

Chosen because it gives a short, provable commit-order cursor and atomic entity/event recording. Rejected: raw sequences (commit inversion loses events), timestamp/UUID ordering (same flaw), and dispatcher-assigned order (safe but needless moving parts for v1).

### ADR-002: Keep idempotency and feed history indefinitely in v1

Chosen because a silent expiry changes retry correctness and can strand an offline Indexer. Rejected: in-memory/TTL cache. Retention can be added only with an explicit expiry/resync contract.

### ADR-003: Publish files before referencing DB rows and reconcile orphans

Chosen because same-filesystem rename plus fsync makes bytes durable before success is visible. Rejected: DB-before-file (visible missing bytes) and distributed-transaction claims unsupported by PostgreSQL/filesystems.

### ADR-004: Persist purge cancellation as a job terminal state

Chosen for race safety and auditability. Rejected: deleting queued purge rows, which loses history and makes Worker/cancellation races ambiguous.

### ADR-005: Expose immutable storage-derived URLs

Chosen for direct Nginx Range/cache service without API/DB per byte request. Rejected for v1: signed URLs/cookies (Tailnet is the approved trust boundary) and mutable profile aliases (cache/current races).

### ADR-006: Forward-only migrations and custom-format logical backups

Chosen for inspectable PostgreSQL evolution and portable restore. Rejected: destructive down migrations and copying a live PostgreSQL data directory. PITR is explicitly deferred.

### ADR-007: Deterministic offsetless capture-time rules

Chosen to avoid host-dependent timezone results: explicit offsets win, fold chooses earlier instant, gap candidates are ignored. Rejected: changing old timestamps after config edits and silently shifting nonexistent times.

### ADR-008: Defer codec quality numbers, not required behavior

Formats, dimensions, color families, animation behavior, and no-H.264-fallback are fixed. Numeric quality, bit depth, detailed metadata retention, and resource limits require fixture/device evidence and belong to #11-#13/#21; guessing here would masquerade as validation.

### ADR-009: Bound uploads at 20 GiB and stream them

Chosen as a conservative Core API safety ceiling that still admits long consumer videos without buffering them in memory. The ceiling is configuration-independent so concurrent nodes enforce one contract; changing it requires a versioned contract update and boundary tests. Rejected: an unbounded request body, which lets one client exhaust disk or hold a handler forever, and a memory-buffered upload. The 24-hour hard deadline and 120-second idle deadline separately handle slow valid uploads and stalled peers.

### ADR-010: Authenticate route- and filter-bound cursors

Chosen so clients cannot construct inconsistent keyset positions or reuse a cursor with different visibility filters. Rejected: raw tuples, offsets that become increasingly expensive and unstable, and encryption presented as secrecy. Cursor contents are not sensitive; authenticity and versioning are the requirements.

### ADR-011: Keep logically deleted metadata readable until purge

Chosen because restore, progress display, and explicit original access operate on retained data, while default listing still hides deleted Media. Rejected: returning `404` immediately after logical deletion, which falsely implies bytes are gone and prevents a coherent restore UI. The trust boundary remains Tailnet-wide; purge is the operation that removes bytes and metadata.

### ADR-012: Make profile activation versions monotonic per key

Chosen so numeric version ordering is also activation-generation ordering, allowing publication to reject a delayed older target without a second generation identifier. A key-scoped advisory lock makes concurrent activation decisions deterministic. Rejected: allowing an already superseded lower version to become active, which can never safely replace a newer current rendition, and a separate activation-generation column, which adds another ordering identity without a use case for version rollback. Rollback is performed by creating a new higher version with the desired older recipe, preserving audit history.
