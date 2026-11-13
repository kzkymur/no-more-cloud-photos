# Ubuntu production deployment

## Quick start

On an Ubuntu 26.04 LTS (resolute) amd64 host, from a clean checkout of the
commit to deploy:

1. **Before the first run** (deploy checks these and never does them for you):
   - Mount the data HDD and bind-mount `/var/lib/nmcp` and
     `/var/lib/nmcp-backup` from it (see `infra/ubuntu/deploy.conf.example`).
     deploy never formats or mounts disks.
   - Install UFW, keep your management access allowed (for example
     `ufw allow 22/tcp`), then `ufw default deny incoming` and `ufw enable`
     from a console. deploy only adds the two Tailnet-interface rules.
   - Join the Tailnet, or put an auth key in a root-only 0600 file and name it
     in the config.
2. Create the config:

   ```sh
   sudo install -m 0600 infra/ubuntu/deploy.conf.example /etc/nmcp/deploy.conf
   sudoedit /etc/nmcp/deploy.conf   # Tailnet DNS name, HDD mount point, ports
   ```

3. Dry run. This prints the preflight and the plan and changes nothing:

   ```sh
   sudo ./deploy --check
   ```

   Fix every line marked `!` and rerun until it reports `preflight passed`.
4. Deploy:

   ```sh
   sudo ./deploy
   ```

   The first run builds the release on the host (about 40 minutes; capped at
   2 CPUs / 4 GiB with low priority). Later runs of the same commit skip the
   build, and steps whose result already exists are skipped.
5. Apply the Tailscale ACL snippet that deploy prints, in the admin console.

What `./deploy` does, in order:

- Adds the PGDG repository (fingerprint-checked) and installs
  `infra/ubuntu/packages.txt`.
- Creates the PostgreSQL 17 cluster (`postgresql@17-main.service`) and the
  `nmcp` database, then runs the role bootstrap. Every login gets a generated
  password, stored only under root-only `/etc/nmcp/deploy`; passwords reach
  PostgreSQL only as SCRAM verifiers.
- Builds the release with `scripts/build-ubuntu-release`.
- Gets the TLS certificate with `tailscale cert` and adds the UFW rules.
- Runs `install-ubuntu-host` and `install-ubuntu-release` stage/activate, then
  installs the backup configuration and its timer.
- Checks services, readiness and the firewall.

The default backup failure hook (`infra/ubuntu/backup-notify`) only writes to
the journal at priority `err`. Replace it if nothing monitors the journal.

What deploy never touches: port 443, the existing `tailscale serve` and Funnel
configuration, the UFW default policy, and disks. It stops with an error if a
configured port is already in use or claimed by `tailscale serve`/Funnel.

The rest of this document is the reference for what deploy automates: the
contracts, and the manual procedure it replaces.

## Reference

This runbook installs the repository's production contract on Ubuntu 26.04 LTS (resolute) amd64.
CI validates these assets on Ubuntu 24.04 only and does not exercise a 26.04
install; record real-host evidence with `docs/real-host-handoff.md`.
It intentionally requires real host values; example addresses, credentials, and
certificates are never promoted to production defaults.

## Supported and blocked scope

The shipped units run PostgreSQL, migration, Core API, the Core Worker, and
Nginx. Before it reports ready or claims transform work, the Worker verifies
the configured pinned ICC and the still, animation, and video helper capability
contracts. It then compares the retained results with all active profiles and
retired profiles pinned by queued/running transform work and registers the real
transform executor only when every exact MIME/recipe/output branch is supported.
Claimed work reads a size/hash-verified pinned Original descriptor, preserves
successful targets across retry, and atomically publishes each durable rendition
under the live lease. The release artifact must provide those trusted paths;
full Ubuntu native-library closure and rollback evidence remains an issue #21
release gate.

No application maintenance, backup, restore, or cleanup service/timer is installed. Issue
#19 has not yet defined the scheduled due/no-op command, exit codes, environment
names, or database role. A timer that always runs the manual command would
duplicate database policy and is unsafe. When #19 lands, a timer may only wake
the application: due-time, last-success, retention, locking, and no-op decisions
must remain database-backed application behavior. This is a fail-closed blocked
boundary, not a claim that scheduled maintenance works.
The independent `nmcp-tls-expiry.timer` only checks certificate lifetime; it
does not encode or wake any database-backed application schedule.

The repository does not contain a host's Tailnet IP, DNS name, certificate,
ACL, firewall policy, mount device, or measured per-process resource split.
Those are mandatory operator inputs. The one resource value fixed by design is
the **aggregate** `nmcp.slice` ceiling: two CPUs and 4 GiB, shared by all Core,
Nginx, and PostgreSQL instance units. No child unit receives a duplicate 2
CPU/4 GiB allocation. A single worker unit gives concurrency one at the process
boundary.

## Files and privilege boundaries

| Path | Owner/mode | Purpose |
| --- | --- | --- |
| `/opt/nmcp/releases/<id>` | `root:root`; directories and `bin/*` 0755, other files 0644 | immutable runtime-readable release |
| `/opt/nmcp/current` | root-owned atomic symlink | active release |
| `/etc/nmcp/*.env` | `root:root 0600` | per-process environment; no CLI secrets |
| `/etc/nmcp/tls-releases/<digest>/*.pem` | `root:root 0600` below 0700 parents | immutable Nginx credential generation |
| `/etc/nmcp/tls-current` | root-owned atomic symlink | active certificate/key pair |
| `/etc/nmcp-nginx/nginx.conf` | `root:root 0644` below 0755 parent | non-secret config readable by Nginx |
| `/var/lib/nmcp/media` | `nmcp:nmcp 0700` | Core writable storage |
| `/var/lib/nmcp/media/.quarantine` | `nmcp:nmcp 0700` | canonical same-filesystem repair quarantine; never served |
| `/var/lib/nmcp/backups` | `nmcp:nmcp 0700` | reserved, never mounted into Nginx |
| `/var/lib/nmcp/deployment` | `root:root 0700` | package and activation evidence |

API, Worker, migration, and Nginx use the non-login `nmcp` identity because
stored media is deliberately mode 0600/0700. Nginx is nevertheless prevented
from writing by its private mount namespace: `/var/lib/nmcp` is inaccessible,
and only `originals` and `renditions` are bind-mounted read-only at `/srv/nmcp`.
It cannot see backup, temporary, quarantine, database, environment, or source
TLS-key paths. systemd exposes the TLS files only in the Nginx service's private
credential directory. Nginx receives only `CAP_NET_BIND_SERVICE` for low ports.

`.quarantine` is a fixed child of `NMCP_STORAGE_ROOT`; there is no separate
quarantine environment variable, caller-selected path, or mount. Operators must
verify it has the same device ID as `/var/lib/nmcp/media` before repair is wired.
The API and Worker currently share the `nmcp` Unix identity and a writable media
root, so this layout prevents accidental serving but does not claim OS-level
quarantine isolation from either Core process. Issue #20 owns any future
administrative unit, credential, and sandbox boundary; none is installed here.

Upgrades from a revision that created the legacy dotless
`/var/lib/nmcp/media/quarantine` fail before host installation, whether that
path is empty or populated. Stop NMCP, inspect the path without following
symlinks, and preserve any unexpected contents before changing it. If it is
empty, remove it with `sudo rmdir /var/lib/nmcp/media/quarantine`. If populated,
record its owner/mode/content inventory, then either move the entire directory
to reviewed operator-controlled storage outside `/var/lib/nmcp/media` (so the
source path is absent), or move each reviewed entry through a controlled
operator procedure and use `rmdir` after the source is empty. Do not recursively
delete or blindly merge contents. Re-run host installation to create canonical `.quarantine`,
then adjudicate preserved entries under the reconciliation manifest procedure
when that application workflow lands. Installation also rejects a symlink,
unsafe owner/mode, or separate filesystem mounted at canonical `.quarantine`.

## Host prerequisites

1. Install Ubuntu 26.04 runtime packages listed in
   `infra/ubuntu/packages.txt`. Do not use `latest` as evidence: preserve
   `dpkg-query` package/version output with the deployment record.
2. Provision PostgreSQL with the documented role split and an `nmcp` database.
   Keep PostgreSQL on literal loopback; do not expose port 5432. Record the
   concrete Ubuntu cluster unit (for example `postgresql@17-main.service`); the
   `postgresql.service` meta-unit is not an acceptable dependency.
3. Mount the durable HDD at the operator-approved location before creating or
   bind-mounting `/var/lib/nmcp`. Confirm filesystem, capacity, boot-time mount
   ordering, and ownership. This repository cannot infer the block device.
4. Enroll the host in the intended Tailnet and apply the approved Tailscale ACL.
   Obtain a certificate for the concrete Tailnet hostname. Do **not** enable
   Funnel. Do not copy validation self-signed certificates.
5. Build or obtain a reviewed release directory with `MANIFEST.sha256`, the
   three Go binaries (`core-api`, `core-worker`, `nmcp-admin`), all three media
   helpers, their pinned native closure, and `share/sRGB2014.icc`. Issue #21 owns
   the production artifact composition and rollback evidence; do not infer that
   closure from the validation image alone.

## Configuration and install

Before the first migration, run the reviewed role bootstrap as the local
PostgreSQL cluster administrator while connected to the empty `nmcp` database:

```sh
sudo -u postgres psql --dbname nmcp --file infra/ubuntu/postgresql-roles.sql
sudo -u postgres psql --dbname nmcp
# In the interactive psql session; passwords never enter shell history/argv:
\password nmcp_migrator
\password nmcp_api
\password nmcp_worker
\password nmcp_check
\password nmcp_repair
```

The script creates credential-free `nmcp_runtime`, `nmcp_worker_runtime`,
`nmcp_purge_function_owner`, `nmcp_check_runtime`, `nmcp_repair_runtime`,
`nmcp_check_function_owner`, and `nmcp_repair_function_owner` roles. The two
reconciliation capability roles and two function owners are NOINHERIT and
isolated from the common runtime graph. API and Worker receive only their
reviewed existing memberships. The operational logins `nmcp_check` and
`nmcp_repair` (LOGIN INHERIT, no superuser, CREATEDB, CREATEROLE, replication
or BYPASSRLS) each receive exactly one edge, their capability role
(`nmcp_check_runtime` / `nmcp_repair_runtime`, INHERIT TRUE, SET FALSE, no
ADMIN), plus CONNECT on the database, and nothing else. They are used only by
`nmcp-admin check` and `nmcp-admin repair`. The migrator is a member of the three function-owner roles only so
forward migrations can transfer narrowly scoped SECURITY DEFINER functions.
The bootstrap treats every direct incoming or outgoing membership edge touching
these stable roles or the five managed logins as a closed allowlist; this also
blocks an external login from reaching the migrator or an owner transitively.
Membership normalization and the final graph check run in one transaction, so
an unsafe extra edge cannot leave a partially rewritten graph.

Old embedded migrations transfer a function to its stable owner and then harden
it in the same migration session. The production migration wrapper therefore
uses a two-phase role transition while API and Worker are stopped. Its root-only
pinned bootstrap first verifies the complete safe graph and role attributes,
then transactionally changes only the three function-owner-to-`nmcp_migrator`
edges from `INHERIT FALSE` to `INHERIT TRUE`; their `ADMIN FALSE` and `SET TRUE`
options and every other edge remain exact. The three inheritance bits are one
state: only all-FALSE hardened or all-TRUE interrupted compatibility is valid;
every mixed combination fails without normalization. An EXIT/INT/TERM handler always runs
the default bootstrap and an independent graph verifier to restore `INHERIT
FALSE`, whether migration succeeds or fails. Temporary inheritance benefits
only the isolated migration login: the exact bilateral graph forbids API,
Worker, or a rogue role from reaching the migrator or function owners.

The wrapper runs the checksum-verified SQL installed under
`/usr/local/libexec/nmcp/postgresql` through local `postgres` peer
authentication on the reviewed `/var/run/postgresql` socket, port 5432, and
database/user identity. It starts libpq from an empty environment and pins those
values on the command line, so caller `PGHOST`, service, password, and session
variables cannot redirect or alter the privileged bootstrap. It does not use or pass the admin DSN to an application
process. Release activation first invalidates any prior `nmcp-migrate.service`
success. API and Worker require a fresh successful migration unit, so a failed
hardening/verifier, normal signal, or interrupted compatibility phase prevents
service start. A power loss or SIGKILL can leave the database in compatibility
mode, but the next mandatory wrapper attempt accepts only that exact interrupted
state, reruns migration idempotently, and hardens before reporting readiness.
`nmcp-admin check`, `repair` and `maintenance resume` never reuse the root
migration credential for reconciliation; they connect as `nmcp_check` /
`nmcp_repair`. Create `/etc/nmcp/credentials` as root:root 0700, and
`check.dsn` / `repair.dsn` from `infra/ubuntu/credentials/*.dsn.example` as
root:root 0600: one libpq connection string for `nmcp_check` / `nmcp_repair`,
with the password set above. Restrict both logins to the `nmcp` database on
loopback in `pg_hba.conf`, like the other credentials. The capability
invocation in [Running nmcp-admin](#running-nmcp-admin) hands both files to the
process as systemd credentials, so they are never readable by the `nmcp` user
at rest. (`NMCP_CHECK_DATABASE_URL_FILE`, `NMCP_REPAIR_DATABASE_URL_FILE` and
`NMCP_ADMIN_DATABASE_URL_FILE` are honoured only when no systemd credential
directory is present, as in the validation harness; production uses
`LoadCredential=`.) nmcp-admin refuses a credential that is not a regular, non-symlink file with the
expected owner and no group/other access (systemd's own credential shape is
accepted), and refuses any connection whose session user is not exactly the
expected login. Both DSNs must name the
same database as the owner credential (`admin.dsn`); after a restore cutover, update them to the
active candidate database (see `docs/backup-operations.md`). If you change the
shape of these two logins (for example add a connection limit), restore's role
graph comparison will refuse until the change is reviewed there too. The
bootstrap transfers database/schema ownership to `nmcp_migrator` and removes
PUBLIC schema creation. Embedded migrations own all application object grants;
the bootstrap deliberately does not grant table or function access after the
fact. Run it before migration 0017, which owns those future grants and must fail
closed if stable roles are absent or unsafe.

Create separate files from `infra/ubuntu/env/*.example`. Replace every marker,
use a random HMAC value of at least 32 bytes, and keep the API address exactly
`127.0.0.1:8080`. `NMCP_FILE_BASE_URL` must name the separate File Server TLS
listener and end in `/files/`.
The three files and TLS inputs must be regular root-owned files below normalized,
root-owned paths with no group/world-writable parent. The installer captures
them through `O_NOFOLLOW` file descriptors before validation/copy, rejects
duplicates or extra/missing environment keys, requires the exact
`nmcp_migrator`/`nmcp_api`/`nmcp_worker` identities with distinct passwords and
the same literal-loopback port 5432 `nmcp` database, permits only `sslmode=disable`
as a DSN option, and rejects query routing overrides, fragments, wildcard API
binds, or a mismatched files-root URL. Environment values use a deliberately
literal subset: quotes, backslashes, and whitespace are rejected so validation
and systemd `EnvironmentFile=` cannot interpret different effective values.
Only LF is accepted as a physical line delimiter; all other control characters
are rejected. PostgreSQL authority permits exactly one raw `@` and no host list.

Render Nginx only with real Tailnet values:

```sh
scripts/render-ubuntu-nginx \
  --template infra/ubuntu/nginx.conf.in \
  --output /tmp/nmcp-nginx.conf \
  --api-listen "<TAILNET_IP>:<API_TLS_PORT>" \
  --files-listen "<TAILNET_IP>:<FILES_TLS_PORT>" \
  --tailnet-hostname "<HOSTNAME>"
```

IPv6 listens must use `[address]:port`. The renderer rejects wildcard,
loopback, LAN, and non-Tailscale ranges, unresolved tokens, malformed hostnames,
and a shared API/File endpoint.

Complete the reviewed UFW procedure in the next section before installation.
Then, as root, install host assets and the first release using the scripts
documented by `--help`:

```sh
sudo scripts/install-ubuntu-host \
  --config-dir /root/nmcp-config \
  --api-listen "<TAILNET_IP>:<API_TLS_PORT>" \
  --files-listen "<TAILNET_IP>:<FILES_TLS_PORT>" \
  --tailnet-hostname "<HOSTNAME>" \
  --postgresql-unit "postgresql@<VERSION>-<CLUSTER>.service" \
  --tls-cert /root/cert.pem --tls-key /root/key.pem
sudo scripts/install-ubuntu-release stage /root/nmcp-release <RELEASE_ID>
sudo scripts/install-ubuntu-release activate <RELEASE_ID>
```

Host-asset installation/reinstallation is accepted only while
`nmcp-files.service` is exactly `inactive/dead`, checked after acquiring the
deployment lock; active, activating, deactivating, reloading, and failed states
are refused before any asset changes.

All stage/activate/rollback/host/TLS mutations and every direct or
dependency-triggered `nmcp-migrate.service` execution share the deployment
serializer. Both stable lock inodes live below root:root mode-0700
`/run/nmcp-deploy`, not the world-writable `/run/lock`. The opener first proves
`/run` itself is root-owned and has no group/world write bits, then anchors all
creation through that validated dirfd. A no-follow dirfd opener
creates them mode 0600 and rejects symlinks, non-regular files, foreign
ownership, extra hard links, mode drift, or pathname/inode replacement before
locking. The installer process itself opens and retains the primary lock FD
across link, exact-release migration,
service-state, and evidence mutations. It then publishes root-only completion
evidence; the systemd singleton consumes that evidence without doing database
work while the installer owns the lock. Ordinary direct/dependency-triggered
migrations acquire and retain the lock themselves and snapshot the exact release
path before execution. The installer also acquires a separate root-only
execution lease before dispatch and atomically transfers that same locked open
file description to the transient migration over a root-only Unix socket. The
tracked wrapper is the sole deadline/cancellation authority: after the receiver
announces readiness, it rechecks the child, installer identity, and deadline,
then sends an FD offer with its monotonic deadline. The receiver rejects an
expired offer and verifies the descriptor before declaring execution readiness.
The wrapper monitors the same absolute deadline and the installer's pinned pidfd
through that declaration, rechecks all authority, and only then sends the final
`EXEC` grant. The receiver rechecks expiry immediately before exec. Thus
installer death before the final grant prevents execution, while death after
the grant leaves the child holding the lease and blocks every new
host/release/TLS/direct-migration mutation until the exact binary exits.
Every sequenced-packet protocol record is received with an expected-plus-one
buffer and is rejected on `MSG_TRUNC`, `MSG_CTRUNC`, or any non-exact length;
valid-prefix extensions cannot be interpreted as READY, offer, or EXEC records.
The concrete database unit must expose a finite `TimeoutStartUSec`. Before it
can dispatch anything, a tracked wrapper writes its own PID/start-time record
and launches and reaps the exact `systemd-run` process. The installer supplies
its previously captured PID/start time; the wrapper pins that production owner
with a pidfd, revalidates identity after opening it, and aborts the exact unit if
the owner dies before the final execution grant.
The lifecycle wrapper imposes a 20-minute total operation deadline that includes
queued dependency jobs and running admin work; on expiry it makes cancellation
irrevocable, stops the exact transient unit, and does not return until that unit
has no job, no main PID, and is inactive/failed or gone. The
transient additionally has a 15-minute runtime deadline and a 30-second stop
deadline. A failed or delayed stop therefore retains both deployment locks and
blocks rollback, link, and service mutation instead of racing live database
work. The wrapper owns socket and runner-record cleanup through terminal exit.
Cancellation also reaps the exact `systemd-run` launcher before a second
mandatory terminal check. The reap closes the late unit-registration window;
only the subsequent no-job/no-PID terminal result permits lock and socket
cleanup.
Successful oneshot migration remains active so production
API/Worker dependency starts cannot invoke it a second time. Staging requires an exact manifest and fixed runtime-safe modes,
then proves the `nmcp` identity can traverse/read/execute the installed release.
Activation stops the target, moves the active symlink atomically, starts the
concrete PostgreSQL dependency and migration, and records success only after API
readiness plus API, Worker, File Server, target, and concrete PostgreSQL remain
active for five consecutive checks. A failure before a completed migration
restores the old link and records actual active and attempted release identities
separately. A failure after migration does **not** automatically run old code
against a potentially new schema, but it stops the target and every individually
started service so the rejected release cannot remain exposed.

Rollback is fail-closed:

```sh
sudo scripts/install-ubuntu-release rollback <PREVIOUS_RELEASE_ID>
```

Both this status transient and every migration transient declare `Requires=`
and `After=` on the reviewed concrete PostgreSQL unit. The target release's
`nmcp-admin migrate status --json` must report fully
compatible (exit 0) against the current database before the link changes. Any
pending/drift/error status refuses rollback; database restoration is a separate,
explicit disaster-recovery operation. Never delete the last known-good release
during activation. If compatible rollback activation fails, the installer stops
all partial services, atomically restores the prior link, and restores its prior
active/inactive service state; a failed restoration is left fully stopped.

## Running nmcp-admin

No process running as the `nmcp` user ever receives the migrator credential.
Operator `nmcp-admin` commands therefore run in one of exactly two documented
modes, each a transient unit. nmcp-admin refuses a command in the wrong mode
(capability commands refuse to run as root or with any migrator credential
present; owner commands refuse to use the migrator login unless the process
is root).

**Capability commands** (`check`, `repair` including `--dry-run`,
`maintenance resume`) run as `nmcp` with only the reconciliation logins:

```sh
sudo systemd-run --quiet --pipe --wait --collect \
  -p User=nmcp -p Group=nmcp \
  -p EnvironmentFile=/etc/nmcp/capability.env \
  -p LoadCredential=check.dsn:/etc/nmcp/credentials/check.dsn \
  -p LoadCredential=repair.dsn:/etc/nmcp/credentials/repair.dsn \
  /opt/nmcp/current/bin/nmcp-admin check|repair|maintenance resume ...
```

They run as `nmcp` because repair creates, renames and removes entries under
the nmcp:nmcp 0700 media root; as root it would leave root-owned entries.
Their database access is limited to the `nmcp_check` / `nmcp_repair` SECURITY
DEFINER API (migration readiness, maintenance state, audit, and the
reconciliation functions).

**Owner commands** (everything else: `config *`, `profile *`, `regenerate`,
`jobs retry`, `cleanup`, other `maintenance` subcommands) run as root with the
migrator credential, and never write under the media root;
`cleanup --kind backups` deletes expired backup artifacts as root:

```sh
sudo systemd-run --quiet --pipe --wait --collect \
  -p User=root \
  -p EnvironmentFile=/etc/nmcp/capability.env \
  -p LoadCredential=admin.dsn:/etc/nmcp/credentials/admin.dsn \
  /opt/nmcp/current/bin/nmcp-admin <command> ...
```

- **Cleanup:** `cleanup --kind renditions` reports what the Worker's automatic
  sweep will delete and waits for it; `--kind media` only enqueues purge jobs
  that the Worker executes; `--kind backups` deletes artifacts only inside
  `NMCP_BACKUP_ROOT`.
- **Secrets stay root-only at rest:** systemd (PID 1) reads
  `/etc/nmcp/capability.env` (non-secret, root:root 0644 or 0600, from
  `infra/ubuntu/env/capability.env.example`) and copies each named DSN file
  into the unit's private `$CREDENTIALS_DIRECTORY`. `/etc/nmcp/admin.env`
  (the migrator DSN) is used only by `nmcp-migrate.service`, never by these
  invocations.
- **Exit status and I/O:** `--wait` returns nmcp-admin's exit code, and `--pipe`
  connects stdin/stdout/stderr, so `--file -` works.
- **Exceptions:** the root-only restore and cutover commands keep their own
  documented procedure in `docs/backup-operations.md`, and `migrate` runs only
  through the release installer's migration unit.

Create the three credential files under `/etc/nmcp/credentials` (root:root
0700) as root:root 0600, from `infra/ubuntu/credentials/*.dsn.example`:
`check.dsn` (`nmcp_check`), `repair.dsn` (`nmcp_repair`), and `admin.dsn`
(`nmcp_migrator`, the same password as `admin.env`).

## Profile certification before activation

Migrations seed the bundled `standard/v1` (`60000000-0000-4000-8000-000000000001`)
and `thumbnail/v1` (`60000000-0000-4000-8000-000000000002`) profiles as
**drafts**. `nmcp-admin profile activate` refuses a draft until append-only
processor certification rows cover every exact input MIME, source mode, output
kind, maximum edge, and setting its recipes need. On each real host, after the
release is active and before activating a profile, certify it with the exact
installed helpers:

1. Collect operator-owned fixtures: at least one per input MIME of the profile
   (all 17 candidate MIMEs for the bundled profiles, including every RAW alias,
   HEIC/HEIF, BMP, MP4, and QuickTime). Every fixture's long edge must be at
   least the recipe edge (1920 for `standard`, 640 for `thumbnail`) because each
   output must reach exactly that edge. `image/webp` under `standard` needs a
   **static** WebP (the Worker's still path) and an **animated** WebP; GIF and
   video fixtures must be animated/real streams. Fixtures are evidence inputs,
   not uploads; keep them outside the storage root (for example under
   `/root/nmcp-fixtures`). Certification is an owner command: root nmcp-admin
   copies each fixture into its private workspace and hands it to the helpers,
   which run as `nmcp`, as inherited file descriptors, so fixtures need not be
   readable by `nmcp`.
2. Write `SPEC.json` with absolute paths and the SHA-256 of each file, measured
   by the operator on this host (for example `sha256sum`), never copied from
   another host:

   ```json
   {
     "profile_id": "60000000-0000-4000-8000-000000000002",
     "helpers": {
       "still_helper": {"path": "/opt/nmcp/current/bin/nmcp-still-helper", "sha256": "<64 hex>"},
       "animation_helper": {"path": "/opt/nmcp/current/bin/nmcp-animation-helper", "sha256": "<64 hex>"},
       "video_helper": {"path": "/opt/nmcp/current/bin/nmcp-video-helper", "sha256": "<64 hex>"},
       "prlimit": {"path": "/usr/bin/prlimit", "sha256": "<64 hex>"},
       "ffprobe": {"path": "/opt/nmcp/current/bin/ffprobe", "sha256": "<64 hex>"},
       "srgb_icc": {"path": "/opt/nmcp/current/share/sRGB2014.icc", "sha256": "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a"}
     },
     "fixtures": [
       {"mime_type": "image/jpeg", "path": "/root/nmcp-fixtures/landscape.jpg", "sha256": "<64 hex>"}
     ]
   }
   ```

   Use the same helper, `prlimit`, and ICC paths as the Worker configuration.
   `ffprobe` is the independent output probe: use the pinned
   `/opt/nmcp/current/bin/ffprobe` (the version CI certifies with). The API's
   upload metadata probe still uses the distribution `/usr/bin/ffprobe`; that
   version skew is a recorded known limit (release-gate deviation table).
   Running as root, certify starts every helper and probe as uid/gid `nmcp`
   (no supplementary groups) with the Worker's `prlimit` limits, so it never
   decodes untrusted fixtures as root. It does not reproduce the Worker unit's
   systemd sandbox (ProtectSystem, PrivateTmp, RestrictAddressFamilies,
   `nmcp.slice`, …), but the helper processes run with `no_new_privs` set.
   While a helper runs, its output file is group-writable by `nmcp`, so
   another uid-`nmcp` process could alter it before certify probes and
   digests it. This is a recorded known limit (release-gate deviation table).
   Use only fixtures you trust.
3. Run, as owner commands (see [Running nmcp-admin](#running-nmcp-admin)),
   with the database in normal mode. The spec is passed on stdin:

   ```sh
   run_owner() {
     sudo systemd-run --quiet --pipe --wait --collect \
       -p User=root \
       -p EnvironmentFile=/etc/nmcp/capability.env \
       -p LoadCredential=admin.dsn:/etc/nmcp/credentials/admin.dsn \
       /opt/nmcp/current/bin/nmcp-admin "$@"
   }
   sudo cat /root/nmcp-certify-thumbnail.json | run_owner profile certify --file - --json
   run_owner profile activate --id 60000000-0000-4000-8000-000000000002 --json
   ```

   Certification does not activate;
   activation stays a separate audited step and still enforces version order.
   Then restart the Worker (`systemctl restart nmcp-worker.service`). The
   Worker validates its capability envelope against the active profiles once at
   startup and claims only those profiles' jobs; until it restarts, jobs for a
   newly activated profile stay `queued` (they are not lost or failed).
4. Preserve the JSON result and the `profile_processor_certifications` evidence
   receipts with the deployment record. Exit `2` means the spec, a helper or
   fixture digest/owner/mode, a fixture MIME, or an output check was rejected;
   `4` means the profile is not a draft, an identical certification already
   exists (same requirement and byte-identical helper/ICC digests), or another
   profile operation holds the lock; `1` is an operational helper or database
   failure. Every non-zero exit inserts nothing. A refused requirement reports a
   fixed `reason` (`requirement_not_covered`) and the exact
   requirement in the JSON result. A refused helper, ICC, fixture, processor
   setup or capability probe reports `reason` `input_evidence_rejected` with the
   failing `input` (for example `still_helper` or `fixture[3]:image/heic`) and
   `check` (`open`, `owner_mode`, `ancestor`, `digest`, `type_size`,
   `processor_config`, `capabilities`, `requirements`, `mime_not_in_profile`,
   `mime_without_fixture`, `inspect`, `routing`, `decoder_not_advertised`),
   never a path.

A certification proves that **this host's exact helper binaries** transformed
**the given fixtures** with the profile's recipes and that independently probed
outputs matched. It is not evidence that every possible input of those MIME
types decodes, and it does not replace the Worker's startup capability probes.
The registry is append-only history: rows are never updated or deleted. A
requirement is refused as a duplicate only when an existing row for the same
processor, schema version, MIME, source mode, output kind, edge, and setting
range carries a receipt whose recorded still/animation/video helper, `ffprobe`,
`prlimit`, and ICC digests are byte-identical to this run. After any helper or
ICC change, re-certify: new rows are appended next to the old ones. An existing
row whose evidence is not a well-formed receipt of this schema counts as not
identical, so new evidence is appended beside it.

Requirements follow the routes the Worker can execute
(`profile.WorkerExecutableOutputs`, the same rule as migration 0024's
activation validator). Under an all-frames recipe a GIF always produces
animated WebP, so bundled `standard` needs only an `animation-webp` row for
GIF, while `image/webp` needs both `still-avif` (static fixture) and
`animation-webp` (animated fixture). Bundled `standard` and `thumbnail` are
therefore certifiable once the full fixture matrix above is supplied.

## Firewall and Tailnet checks

Binding to the concrete Tailnet address is mandatory but does not replace ACLs
or the host firewall. The installer is deliberately fail-closed on UFW: UFW must
already be active, its default incoming policy must be deny, and its persisted
rules must contain exactly the two interface+destination rules below with no
all-port, range, service-profile, shorthand, or other rule that can cover either
port. Other management allows are accepted only when a single disjoint numeric
port and protocol can be proved. Perform policy changes only from an approved
console/change window. First inventory and preserve an explicit management/SSH
allow; do not enable default deny from the only unprotected remote session.

```sh
tailscale status
tailscale ip -4
tailscale ip -6
tailscale funnel status                 # must show no Funnel listener
sudo ss -lntup                          # API direct only 127.0.0.1:8080
sudo ufw status verbose                 # record policy and management allows
# Add/verify the approved management allow before changing the default.
sudo ufw default deny incoming
sudo ufw allow in on tailscale0 to <TAILNET_IP> port <API_TLS_PORT> proto tcp
sudo ufw allow in on tailscale0 to <TAILNET_IP> port <FILES_TLS_PORT> proto tcp
sudo ufw enable                         # only after console/management proof
sudo scripts/verify-ubuntu-firewall --tailnet-ip <TAILNET_IP> \
  --api-port <API_TLS_PORT> --files-port <FILES_TLS_PORT>
```

If UFW is not the approved manager, do not bypass the installer check. Extend
and review the verifier/install contract for the managed nftables/firewalld
policy in a separate change before deployment.
Do not add wildcard `0.0.0.0`, `::`, LAN-interface, WAN-interface, or Funnel
rules. From an authorized Tailnet client, both TLS listeners must work. From a
LAN client with Tailscale disabled, both must fail. Run the latter from another
machine: a local request is not evidence of a non-Tailnet denial.

## TLS renewal and expiry

`nmcp-tls-expiry.timer` checks daily and fails if the active certificate is not
currently valid, lacks a DNS SAN or a
trusted server chain for the recorded hostname, or expires within seven days; monitor failed
units/journal through the host's normal alerting. Obtain a new Tailnet
certificate/key using the approved mechanism into trusted root-only paths, then:

```sh
sudo /usr/local/libexec/nmcp/renew-ubuntu-tls /root/new-cert.pem /root/new-key.pem
sudo systemctl status nmcp-tls-expiry.service nmcp-files.service
```

Renewal preserves an intentionally inactive File Server; it restarts only a
stable running service and rejects transition states. It validates current validity, SAN/hostname, trusted chain, key pair,
ownership/path, and seven-day lifetime; writes
one immutable generation; atomically switches `tls-current`; and **restarts**
Nginx so systemd `LoadCredential` is refreshed. If restart fails, it restores
the previous generation and attempts to restart it; failure of that recovery
restart is reported without claiming availability. The expiry check also compares
the active systemd credential snapshot fingerprint with `tls-current`, detecting
an interrupted publish-before-restart window. Host reinstall refuses while the
File Server is active. A config reload alone does
not refresh systemd credentials. Retain the prior generation until verification
and remove old generations only in a separately reviewed housekeeping step.

## Required verification and evidence

Run repository-static checks before install:

```sh
scripts/verify-ubuntu-deployment
```

On the host record, without printing secret values:

```sh
# nmcp-files.service ExecStartPre runs this exact nginx -t inside the
# service credential namespace; preserve its successful journal line.
sudo systemctl restart nmcp-files.service
sudo journalctl -u nmcp-files.service --since '<ACTIVATION_TIME>'
sudo systemd-analyze verify /etc/systemd/system/nmcp*.service \
  /etc/systemd/system/nmcp.target /etc/systemd/system/nmcp.slice
sudo systemctl is-enabled nmcp.target
sudo systemctl --failed
sudo systemctl show nmcp.slice -p CPUQuotaPerSecUSec -p MemoryMax
sudo systemctl show "<CONCRETE_POSTGRESQL_UNIT>" nmcp-api.service nmcp-worker.service \
  nmcp-files.service -p Slice -p User -p Group
sudo ss -lntup
sudo stat -c '%U:%G %a %n' /etc/nmcp /etc/nmcp/*.env /etc/nmcp-nginx \
  /etc/nmcp-nginx/nginx.conf /etc/nmcp/tls-current \
  /etc/nmcp/tls-current/*.pem /var/lib/nmcp/media \
  /var/lib/nmcp/media/.quarantine /var/lib/nmcp/backups
test "$(stat -c %d /var/lib/nmcp/media)" = \
  "$(stat -c %d /var/lib/nmcp/media/.quarantine)"
curl --fail --show-error http://127.0.0.1:8080/readyz
```

Prepare one small, dedicated Original and Rendition and a canonical-looking
symlink to each in their respective storage trees. The symlinks must target the
known objects; their IDs must differ from the target IDs, and the paths must
otherwise match the production URL grammar. Preserve the fixture files used for
byte comparison, then run the repository assertion client from an authorized
Tailnet client:

```sh
scripts/assert-ubuntu-files-endpoint \
  --base-url "https://<HOSTNAME>:<FILES_TLS_PORT>" \
  --original-path "/files/originals/<SHARD>/<ORIGINAL_ID>/original.<EXT>" \
  --original-file "<LOCAL_ORIGINAL_FIXTURE>" --original-mime "<ORIGINAL_MIME>" \
  --original-symlink-path "/files/originals/<SHARD>/<SYMLINK_ORIGINAL_ID>/original.<EXT>" \
  --rendition-path "/files/renditions/<SHARD>/<ORIGINAL_ID>/<RENDITION_ID>/<PROFILE_ID>.<EXT>" \
  --rendition-file "<LOCAL_RENDITION_FIXTURE>" --rendition-mime "<RENDITION_MIME>" \
  --rendition-symlink-path "/files/renditions/<SHARD>/<ORIGINAL_ID>/<SYMLINK_RENDITION_ID>/<PROFILE_ID>.<EXT>"
```

Use the DNS hostname covered by the certificate, not the Tailnet IP. Normal
system trust is the default. For an approved private CA, append
`--ca-file /path/to/ca-bundle.pem`. Never use `--insecure` against a real host;
that switch exists only for the disposable self-signed launcher.

The client independently checks Original and Rendition exact GET bytes;
raw-socket body-free HEAD; exact, open-ended, suffix, and multipart ranges
(including framing, per-part Content-Range, and payload); unsatisfiable 416;
body-free ETag 304; MIME, cache, Accept-Ranges, and nosniff headers; POST denial;
strict 403 symlink denial; and listing, arbitrary, non-public, noncanonical-case,
wrong-shard, malformed-escape, encoded-separator, and raw/encoded normalized
aliases to the existing object. Remove the dedicated symlinks after collecting
evidence. Separately verify Nginx cannot create, rename, or unlink in either bind
mount. `scripts/verify-ubuntu-nginx-http` runs this same client against a
disposable self-signed instance; it is CI evidence for the template, not a
substitute for running the command above against the real trusted endpoint.

Stop/start each unit and the recorded concrete PostgreSQL instance, and verify readiness becomes unavailable
then recovers. Send SIGTERM and confirm graceful shutdown within unit timeouts.
Reboot only with explicit host approval, then confirm mount, PostgreSQL,
migration, API, Worker, Nginx, and readiness ordering. If the real host,
authorized/negative clients, firewall authority, or reboot permission are not
available, mark those checks **not executed**; static or container checks must
not be reported as substitutes.
