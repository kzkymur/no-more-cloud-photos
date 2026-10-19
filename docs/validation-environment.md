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
| Nginx | `https://127.0.0.1:18443` | worker UID/GID 10001; only Original/Rendition volume subpaths are mounted read-only; isolated network |

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
Validation migrations run as the non-superuser `nmcp_migrator`. A dedicated
bootstrap-only container first verifies the closed role graph and enables
inheritance on exactly the three function-owner memberships. A second
bootstrap-only container always restores the hardened `INHERIT FALSE` graph and
runs the independent graph verifier before Compose may start API or Worker; the
validation driver invokes that hardener explicitly after a failed startup as
well. The bootstrap password is present only in those disposable administrator
containers and is not inherited by application services.

Storage initialization creates the sole canonical quarantine directory at
`/var/lib/nmcp/media/.quarantine` as UID/GID 10001 mode 0700 and verifies that it
has the same device ID as the storage root. There is no dotless `quarantine`
fixture and no quarantine-path environment override. A private sentinel proves
the File Server cannot address the directory, while mount inspection continues
to inspect every live mount and require exactly the two read-only volume
subpaths `originals` and `renditions`. Retained-volume upgrade evidence rejects
both empty and populated legacy dotless directories without deleting them.
Because
the disposable API and Worker deliberately share UID/GID 10001 and the media
volume, this is serving/layout evidence rather than OS-level process isolation.

## Commands

Docker Engine, Docker Compose v2, `curl`, `python3`, and `sha256sum` are required
on a Linux host.

```text
scripts/validation-env up       # build, migrate, start, and install a known file fixture
scripts/validation-env check    # probes the already-running environment
scripts/validation-env logs     # diagnostics without credentials printed by the apps
scripts/validation-env down     # stop; retain named database/media volumes
scripts/validation-env reset    # stop and remove only this checkout's owned volumes
scripts/validation-env verify   # clean start, checks, version evidence, guaranteed cleanup
```

The default Compose project name includes a hash of the checkout's canonical
path. `NMCP_VALIDATION_PROJECT` may explicitly override it, but every container,
network, and volume also carries the full checkout-owner hash. Every command
fails closed before operating on a same-named resource with a missing or
different owner label. Thus another checkout cannot be stopped or reset even if
an operator gives both the same override. Cleanup does not traverse or delete
host paths. A failed standalone `up` and every `verify` exit remove owned
containers and named volumes; CI also has an independent `if: always()` reset.
Docker's ordinary image/build cache is retained.

`verify` proves clean and retained-volume lifecycles (`reset; up; down; up;
check; reset`) using unique media-file and PostgreSQL markers that startup does
not recreate, two simultaneously running project identities, continued
operation of one after resetting the other, and fail-closed foreign-owner
rejection. `check` proves API/Worker process state and graceful exit/restart,
Nginx configuration validity, exact Original and Rendition bodies/MIME,
body-free HEAD, exact closed/open-ended/suffix Range bodies and Content-Range,
multipart Range framing, unsatisfiable Range (`416`), Accept-Ranges, private
immutable cache, and nosniff independently for both Nginx storage locations.
It also proves non-GET rejection (`405`), directory-listing denial, raw dot
segment and encoded-separator rejection against existing control objects,
canonical shard equality, canonical-name Original and Rendition symlink denial,
and existing non-public backup and `.quarantine` sentinels. It asserts that
Nginx has only its isolated network, shares no network with PostgreSQL, and
receives exactly two read-only Original/Rendition subpath volume mounts and
three read-write tmpfs mounts at `/var/cache/nginx`, `/var/run`, and `/tmp`,
with the exact configured size/mode/UID/GID option map. The inspection treats
only equivalent Engine renderings as normalization: size suffixes are converted
to bytes, and a leading-zero octal mode or Engine's decimal mode are compared as
the same numeric value; Alpine's `/var/run` symlink appears as `/run` in live
`mountinfo`. Missing, extra, wrong-type, or wrong-RW live mounts fail.
It also
records the live PostgreSQL, Nginx,
ExifTool, FFprobe, and `prlimit`
versions. It builds the merged issue #11 source-pinned native still toolchain
through its single canonical build script, runs `nmcp-still-helper capabilities`,
checks the exact library versions and sRGB ICC digest, rejects an unresolved
dynamic dependency, and records the closure, installed size, runtime package
versions, and final Core image size. Environment
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
Dockerfile-specific allowlists limit the build contexts to Go/native-helper
sources or Nginx configuration; repository secrets, dumps, media, documentation,
and unrelated untracked files are not sent to the builder.

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
