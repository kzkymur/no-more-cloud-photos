# Core development and configuration

## Pinned dependencies

- Go `1.27.1`. Official download metadata: <https://go.dev/dl/?mode=json>.
  The Linux amd64 archive SHA-256 is
  `63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445`.
- `github.com/jackc/pgx/v5` `v5.11.0`, locked with all transitive module
  checksums in `go.sum`.
- `golang.org/x/sys` `v0.36.0` supplies Linux descriptor-relative filesystem
  and no-replace rename operations used by the storage durability layer.
- CI uses immutable action commits: `actions/checkout` v7.0.1 at
  `3d3c42e5aac5ba805825da76410c181273ba90b1` and `actions/setup-go` v7.0.0
  at `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`.
- PostgreSQL integration tests run against the `postgres:17-alpine` service.
  This is a test entry point, not a claim that later backup/restore and
  deployment compatibility have already passed.
- Metadata acceptance uses ExifTool 13.36 for still/RAW inputs, FFprobe/FFmpeg
  6.0.1 for video evidence, and util-linux `prlimit` for the inherited
  address-space limit. CI downloads the immutable named archives, verifies
  their documented SHA-256 digests before extraction, and records all tool
  versions in each run. The Ubuntu 24.04 CI image supplies `prlimit`; issue #20
  pins the production platform contract and verifies these absolute executable paths:
  `/usr/bin/exiftool`, `/usr/bin/ffprobe`, and `/usr/bin/prlimit`.

The isolated validation image builds the still, animation, and video helpers in
that order into one pinned `/opt/nmcp` prefix. This proves the shared codec
closure used by the Worker validation lane; issue #21 separately owns packaging
and rollback proof for the production release artifact.

The repository sets `go 1.27.0` as its language/toolchain floor while CI pins
the security patch release `1.27.1`. Set `GOTOOLCHAIN=local` so an unexpected
toolchain is never downloaded implicitly.

## Commands

```text
go mod verify
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./...
TEST_DATABASE_URL='postgres://...' go test -v -count=1 -run '^TestMigratorIntegration$' ./internal/database
TEST_DATABASE_URL='postgres://...' go test -v -count=1 ./internal/job ./internal/worker ./internal/processrunner
TEST_DATABASE_URL='postgres://...' go test -v -count=1 -run '^TestReadServiceIntegration$' ./internal/readapi
TEST_DATABASE_URL='postgres://...' go test -v -count=1 -run '^TestReadHTTPPostgreSQLIntegration$' ./internal/httpapi
```

`nmcp-admin migrate status` is read-only. It exits `4` when migrations are
pending and `1` for connection failure or migration-history drift.
`nmcp-admin migrate up` uses the database advisory lock and applies embedded
forward migrations. Issue #4 owns the initial Core schema, so issue #3 embeds
no schema SQL and only establishes the runner/history contract.

Migrations are transactional by default and are applied one at a time with
their history row in the same transaction. A PostgreSQL operation that cannot
run in a transaction must begin with the exact first line
`-- nmcp:transaction=off idempotent=true`. The explicit idempotency promise is
required, and such a file contains exactly one executable statement so the
PostgreSQL simple-query protocol cannot create an implicit multi-statement
transaction. Every migration rejects explicit transaction-control statements;
only the runner owns transaction boundaries. The runner writes a
dirty/in-progress row before executing outside a transaction, and after a
process crash reruns only that known, checksum-matched,
dirty tail migration. Unknown, changed, unsafe, or non-tail dirty history is
drift and blocks readiness. One session-level advisory lock covers validation,
transactional migrations, non-transactional execution, and recovery.

## Environment

| Variable | API | Worker | Admin | Rule/default |
|---|---:|---:|---:|---|
| `NMCP_DATABASE_URL` | required | required | required | PostgreSQL DSN; secret, never logged. |
| `NMCP_STORAGE_ROOT` | required | required | - | Absolute clean path; never logged. |
| `NMCP_FILE_BASE_URL` | required | required | - | Absolute HTTPS File Server files root with a non-root path, e.g. `https://photos.example.ts.net/files`; trailing slash is normalized. Core appends the canonical storage key directly and never inserts `/files`. No credentials, query, fragment, dot/empty segments, or encoded path ambiguity. |
| `NMCP_STILL_HELPER_PATH` | - | required | - | Clean absolute path to the trusted still/RAW protocol-v1 helper. |
| `NMCP_ANIMATION_HELPER_PATH` | - | required | - | Clean absolute path to the trusted animation protocol-v1 helper. |
| `NMCP_VIDEO_HELPER_PATH` | - | required | - | Clean absolute path to the trusted video protocol-v1 helper. |
| `NMCP_PRLIMIT_PATH` | - | required | - | Clean absolute path to the trusted `prlimit` executable used by the process supervisor. |
| `NMCP_SRGB_ICC_PATH` | - | required | - | Clean absolute path to the pinned sRGB2014 ICC profile. |
| `NMCP_SRGB_ICC_SHA256` | - | required | - | Must equal the pinned sRGB2014 digest `384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a`. |
| `NMCP_CURSOR_HMAC_KEY` | required | - | - | At least 32 bytes; secret, never logged. |
| `NMCP_API_ADDR` | optional | - | - | `127.0.0.1:8080`; explicit host and valid port. |
| `NMCP_LOG_LEVEL` | optional | optional | optional | `info`; one of `debug`, `info`, `warn`, `error`. |
| `NMCP_SHUTDOWN_TIMEOUT` | optional | optional | optional | `30s`; range `1s` through `5m`. |
| `TEST_DATABASE_URL` | tests | tests | tests | Enables isolated-schema real PostgreSQL migration tests. |

API `GET /healthz` checks only the process handler. `GET /readyz` checks the
database, migration currency/checksums, and the shared storage probe. The probe
uses exclusive create, write, file sync, no-replace rename, directory sync,
unlink, and deletion-directory sync beneath the pinned non-symlink root. Worker
startup uses the same probe, verifies the pinned ICC, and completes all three
processor capability handshakes before it can report ready or claim a transform
job. Dependency failures return only the stable
`unavailable` error and do not expose DSNs, paths, or SQL details.

For example, with `NMCP_FILE_BASE_URL=https://photos.example.ts.net/files`,
the stored key `originals/ab/<original-id>/original.jpg` is returned as
`https://photos.example.ts.net/files/originals/ab/<original-id>/original.jpg`.
The corresponding Nginx URI namespaces map to the storage namespaces without
another `files` component:

```nginx
# URI-to-storage-root mapping only; issue #20 owns the complete hardened config.
location /files/originals/  { alias /var/lib/nmcp/media/originals/; }
location /files/renditions/ { alias /var/lib/nmcp/media/renditions/; }
```

The production configuration must additionally implement the method, Range,
cache, directory, traversal, malformed-escape, and symlink rules in the File
Server contract; the abbreviated mapping above is not a deployable substitute.

## Storage durability boundary

`internal/storage` is Linux-specific because the approved deployment target is
Ubuntu Server. It pins `NMCP_STORAGE_ROOT` as a directory descriptor, walks and
creates key components with `openat`/`mkdirat` plus `O_NOFOLLOW`, creates the
attempt temp in the final directory, syncs every new child and parent directory,
syncs and validates file bytes, publishes with `renameat2(RENAME_NOREPLACE)`, and
syncs the final directory. The configured root must be on a local POSIX
filesystem whose atomic rename and file/directory `fsync` semantics are trusted;
NFS/FUSE behavior is not claimed. The storage tree is owned by the Core service
identity and must not be writable by other users or processes. Descriptor-
relative operations prevent traversal and symlink following; higher-level
Media/lease locks in #14/#16 serialize publish versus delete and prevent a
trusted writer from replacing an entry across the delete type-check/unlink
window.

Only canonical typed Original/Rendition keys enter filesystem methods. Temp
files remain before rename after a crash; a final file after rename but before a
database commit is an orphan for explicit reconciliation. A post-rename sync
failure is reported as a published but uncertain outcome, never as an absent
file. Deletion is exact-key and idempotent, syncs its directory, and deliberately
does not prune directories. Deterministic before/after fault injection models
recovery-visible namespace states; process termination and real physical
power-loss persistence remain explicit system evidence for issue #21.
`Store.Close` prevents new work but existing Temp descriptors remain independent;
every owner must finish `Publish` or call `Abort`. API draining and Worker
shutdown complete owned operations before closing the Store.

API and Worker handle SIGINT/SIGTERM. The API stops intake and drains HTTP
requests within the configured timeout. The Worker stops claiming, cancels and
reaps the active process tree, and uses an independent bounded 10 second
database context to requeue only a still-live lease with `worker_shutdown`
before closing dependencies. A lost or exactly expired token never writes an
error or state.

## Job lease and process boundary

`internal/job` claims only registered transform work in a short PostgreSQL
transaction using `FOR UPDATE SKIP LOCKED`, database-clock availability, a new
UUIDv4 token, a two-minute lease, and an incremented attempt. Tool execution
holds no database transaction. Heartbeats run every 30 seconds and every state
mutation compares Job ID, running state, token, and an expiry strictly greater
than `clock_timestamp()`; equality is expired. Reclaim processes at most 50
expired rows per transaction and records only the fixed safe `lease_expired`
summary. Recoverable failures use full jitter over an exponential five-second
base capped at 15 minutes. An exhausted row becomes failed, and the database
constraint prohibits `queued` when `attempts >= max_attempts`.

Ordinary retry keeps successful targets terminal and requeues only failed
targets. Failed-job administrative retry preserves attempts and successful
targets, atomically raises the ceiling by a positive additional budget, makes
the row immediately claimable, and records both successful and rejected
concurrent commands in immutable audit history. Error messages stored on Jobs
are fixed summaries; child stderr, paths, DSNs, and secrets are never stored.

Generic #10 claim deliberately rejects purge. Purge first start must lock Media,
recheck deletion/cancellation, and atomically set `started_at` with its lease;
issue #16 adds that operation with the restore lock order. Until #11-#14 register
a capability-checked transform executor and atomic publication path, the
production Worker has an empty executor registry: it may reclaim expired leases
but cannot claim or silently no-op-complete a job.

`internal/processrunner` is the shared Linux containment layer for metadata and
Worker tools. It accepts only a clean absolute executable plus an argument
array, replaces the environment, applies the inherited address-space limit,
and caps stdout/stderr independently. A short-lived `/proc/self/exe` subreaper
kills the initial process group and adopted `setsid`/double-fork descendants,
then reaps to `ECHILD` on success, timeout, cancellation, or output overflow.
The initial Worker contract exposes one required processor thread and safety
timeout ceilings of 30 minutes for still/RAW, two hours for animation, and 24
hours for video. Registered processors must translate the thread value into
their tool-specific trusted argument array and may only shorten the family
ceiling. Exact recipes and runtime capability checks remain #11-#13 work.

## Profile recipe boundary

`internal/profile` and migration `0002` define `nmcp-media` parameter schema v1.
Recipes exactly cover their normalized input MIME allowlist and encode source
probing, frame/loop behavior, primary-stream selection, resize/even-dimension,
orientation, color/tone-map, metadata, alpha, audio, and output settings.
Unknown fields, wildcards, unregistered candidate MIMEs, missing/extra recipes,
and invalid output combinations fail in both Go and PostgreSQL.

Bundled `standard/v1` and `thumbnail/v1` are drafts. Their AVIF Q60/Q50 at
8-bit, animated-WebP Q80 at 8-bit, and AV1 CRF32 at 10-bit 4:2:0 with AAC
128kbit/s are provisional starting points, not verified quality or compatibility
claims. Q80 is an initial lossy animation balance consistent with the other
draft quality targets; #12 and #21 must replace it through a new immutable
profile version if fixture/device evidence rejects it. The exact candidate MIME
aliases are likewise versioned inputs for #7 detector evidence, never a promise
that every vendor variant works.

Candidate registry rows are immutable and carry pending evidence only. A profile
can activate only when immutable certification rows cover every required exact
MIME/source/output kind, maximum edge, and quality/CRF range. A database
certification is deployment compatibility metadata, not proof that a particular
Worker process has its binary/plugin/codec; #11–#13 add startup/runtime probes
and must refuse execution on mismatch. Migration application alone never
certifies a capability.

## Metadata probe boundary

`internal/mediaformat` is the shared closed registry used by profile recipes,
content detection, and original storage-extension mapping. `internal/metadata`
accepts a complete regular temporary file. Issue #8 owns wiring it into the
upload acceptance transaction and late selection of the detected extension;
issue #7 does not claim that HTTP upload is implemented.

Detection never consumes a multipart MIME declaration or filename. It performs
at most 1 MiB of cumulative structural reads of JPEG/PNG/GIF/WebP/BMP, RAF, TIFF-derived RAW, and
ISO-BMFF/legacy QuickTime headers. Generic TIFF, vendor-name-only TIFF, invalid
offsets/lengths/loops, and contradictory BMFF brands are not coerced into a
registered MIME. HEIF/HEIC sequence brands are rejected, including files that
mix still and sequence compatibility, because the closed registry contains no
sequence MIME. Header recognition proves only a candidate container/family;
ExifTool/FFprobe must still extract valid dimensions and video duration, and
issues #11–#13 remain responsible for codec/transform runtime capability.

The complete regular file is opened once with `O_NOFOLLOW|O_NONBLOCK`, detected
through that read-only descriptor, and exposed to the tool as
`/proc/self/fd/3`; the pathname is not reopened after detection. External tools
are invoked without a shell through trusted absolute paths. Their environment
is replaced with `LC_ALL=C`, `LANG=C`, and `TZ=UTC`; ExifTool is additionally
started with configuration loading disabled.
Each probe inherits a 1 GiB `RLIMIT_AS` through `prlimit`, has a 60 second wall
deadline, and caps stdout and stderr independently at 1 MiB. A short-lived copy
of the current Core executable supervises each probe as a Linux child
subreaper. It kills the initial process group, repeatedly kills session/group
escapees adopted from the probe tree, and reaps until `ECHILD` on timeout,
cancellation, output overflow, and normal direct-child exit. A bounded cleanup
failure is reported as a probe failure. This containment is designed for the
trusted, same-UID pinned tools; it is not a defense against uninterruptible
kernel sleep, privilege/namespace escape, or a hostile fork bomb. The address
space limit is neither a sandbox nor an RSS guarantee. Tool output,
absolute input paths, stderr, and environment data are never copied into API or
stored metadata.

The configurable acceptance policy defaults to dimensions no greater than
100,000 on either axis and a checked product no greater than 1,000,000,000
pixels. Rejection reports whether the dimension or pixel limit failed. These
are metadata acceptance limits, not a statement that such an image can be
decoded in 4 GiB; decoder/frame/transform limits belong to #11–#13 and real
resource evidence belongs to #21.

Capture candidates remain ordered: EXIF DateTimeOriginal, DateTimeDigitized,
DateTime; or primary video stream creation time then container creation time.
A malformed matching offset invalidates that candidate rather than being
guessed as local time. Valid offsetless candidates use the acceptance
transaction's timezone snapshot; DST folds select the earlier UTC instant and
gaps fall through. Filesystem times are never candidates.

`originals.exif_json` is a typed allowlist (camera/lens, orientation, exposure,
aperture, ISO, focal length, GPS, and selected raw capture strings), not a full
copy of every EXIF tag and not an instruction to modify the original. Derived
detector evidence, capture decision/fold, selected video stream/codec, and
probe tool version are kept separately in source metadata. Missing EXIF
serializes as `{}`. Fixture provenance and SHA-256 values are recorded in
`internal/metadata/testdata/README.md`; synthetic 17-format detector evidence is
reported separately from real ExifTool/FFprobe executions.

FFprobe excludes attached pictures, still-image thumbnails, and unusable
zero-dimension video streams. It selects a default usable stream first, then
the largest pixel area and lowest stream index. Duration uses the positive
container duration first with the selected-stream duration as fallback and is
converted to milliseconds with checked decimal arithmetic. Issue #13 must use
the persisted primary stream index rather than independently choosing another
stream.
