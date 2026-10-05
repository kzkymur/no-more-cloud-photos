# Core validation environment

This Docker Compose environment is the reproducible integration substrate for
issue #36. It starts PostgreSQL 17, applies the embedded migrations, and starts
the Core API, Core Worker, and an HTTPS Nginx File Server. It is for local and CI
validation only; it does not replace the Ubuntu systemd/Tailscale deployment
owned by issue #20.

## Boundary and layout

| Process | Validation endpoint | Identity/access |
|---|---|---|
| Core API | `127.0.0.1:18080` | container UID/GID 10001; read/write media and database |
| Core Worker | not published | container UID/GID 10001; read/write media and database |
| PostgreSQL | Compose network only | validation-only database/password; database volume is not shared |
| Nginx | `https://127.0.0.1:18443` | worker UID/GID 10001; media volume is mounted read-only |

Only `media/originals` and `media/renditions` are mapped below `/files`.
PostgreSQL data, TLS keys, temporary files, backup, and quarantine roots are not
mounted into Nginx. The API is deliberately bound to loopback on the host. The
File Server uses a fresh two-day self-signed certificate, so validation clients
must opt into trusting it. Production certificates, Tailnet ACLs, firewall,
restart behavior, and Funnel prohibition remain #20/#22 evidence.

The validation-only password and cursor key in `compose.yaml` are intentionally
non-secret and must never be copied into deployment configuration. Production
secrets belong in root-owned systemd credentials/environment files readable by
the service manager and service identity, not in the repository or arguments.

## Commands

Docker Engine, Docker Compose v2, and `curl` are required on a Linux host.

```text
scripts/validation-env up       # build, migrate, start, and install a known file fixture
scripts/validation-env check    # probes the already-running environment
scripts/validation-env logs     # diagnostics without credentials printed by the apps
scripts/validation-env down     # stop; retain named database/media volumes
scripts/validation-env reset    # stop and remove only nmcp-validation named volumes
scripts/validation-env verify   # clean start, checks, version evidence, guaranteed cleanup
```

`reset` and `verify` use the fixed Compose project name `nmcp-validation` and
remove only that project's named volumes. They do not traverse or delete host
paths. A failed build keeps Docker's ordinary image/build cache but removes the
validation containers and named data volumes when `verify` exits.

`check` proves API health/readiness, Nginx configuration validity, GET/HEAD,
closed/open-ended/suffix Range (`206`), unsatisfiable Range (`416`), non-GET
rejection (`405`), private immutable cache and nosniff headers, directory-listing
denial, traversal denial, missing objects, and non-public backup namespace. It
also records the live PostgreSQL, Nginx, ExifTool, FFprobe, and `prlimit`
versions. It builds the merged issue #11 source-pinned native still toolchain
through its single canonical build script, runs `nmcp-still-helper capabilities`,
checks the exact library versions and sRGB ICC digest, and records its dynamic
dependency closure, installed size, and final Core image size. Environment
startup is not a claim that upload/transform/lifecycle,
codec quality, backup/restore, failure injection, or complete issue #21 E2E has
passed.

## Image and package policy

The validation inputs are Go `1.27.1`, Ubuntu `24.04`, PostgreSQL
`17-alpine`, and Nginx `1.28-alpine`; every base image is pinned by its
multi-platform manifest digest. Ubuntu/APK package patch versions are resolved
at image build time and printed by `verify` for diagnosis. This is not a
production package lock: issue #20 must pin production package versions/digests
and absolute tool paths. The native still-image source SHAs, build flags, and
versions remain owned by issue #11's
`internal/stillprocessor/helper/build-pinned-toolchain.sh`; the validation image
calls that script rather than duplicating or changing its codec contract.

## Production contract handed to issue #20

- Separate API, Worker, PostgreSQL, Nginx, and administrative processes. API is
  loopback-only behind the approved routing boundary; Worker/database are not
  published.
- A dedicated unprivileged Core identity owns the local POSIX media root. API
  and Worker write it; Nginx receives only a read-only view. Storage files are
  mode `0600`, so Nginx must use the deliberately shared read identity or an
  equivalently narrow read mechanism—never grant a broad world-readable mode.
- Serve exactly `originals/` and `renditions/` under the configured HTTPS
  files-root. Never expose media-root siblings, database, backup, temporary,
  quarantine, credential, or service configuration paths.
- Configuration ownership must prevent the service identity from changing its
  own secrets or unit definition. DSNs and HMAC keys must not appear in command
  arguments or logs.
- Divide the total 2 CPU / 4 GiB cgroup budget across services; do not assign
  that full budget independently to every unit. Worker concurrency starts at
  one. Timer wakeups invoke the application; database configuration determines
  backup/cleanup eligibility.
