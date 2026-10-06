# Ubuntu production deployment

This runbook installs the repository's production contract on Ubuntu 24.04.
It intentionally requires real host values; example addresses, credentials, and
certificates are never promoted to production defaults.

## Supported and blocked scope

The shipped units run PostgreSQL, migration, Core API, the current Core Worker,
and Nginx. The worker currently performs lease reclamation only. It does not
run transform jobs or `nmcp-still-helper` until issue #14 connects that runtime
interface.

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
| `/var/lib/nmcp/backups` | `nmcp:nmcp 0700` | reserved, never mounted into Nginx |
| `/var/lib/nmcp/deployment` | `root:root 0700` | package and activation evidence |

API, Worker, migration, and Nginx use the non-login `nmcp` identity because
stored media is deliberately mode 0600/0700. Nginx is nevertheless prevented
from writing by its private mount namespace: `/var/lib/nmcp` is inaccessible,
and only `originals` and `renditions` are bind-mounted read-only at `/srv/nmcp`.
It cannot see backup, temporary, quarantine, database, environment, or source
TLS-key paths. systemd exposes the TLS files only in the Nginx service's private
credential directory. Nginx receives only `CAP_NET_BIND_SERVICE` for low ports.

## Host prerequisites

1. Install Ubuntu 24.04 runtime packages listed in
   `infra/ubuntu/packages.txt`. Do not use `latest` as evidence: preserve
   `dpkg-query` package/version output with the deployment record.
2. Provision PostgreSQL with a least-privilege application role and a database.
   Keep PostgreSQL on literal loopback; do not expose port 5432. Record the
   concrete Ubuntu cluster unit (for example `postgresql@16-main.service`); the
   `postgresql.service` meta-unit is not an acceptable dependency.
3. Mount the durable HDD at the operator-approved location before creating or
   bind-mounting `/var/lib/nmcp`. Confirm filesystem, capacity, boot-time mount
   ordering, and ownership. This repository cannot infer the block device.
4. Enroll the host in the intended Tailnet and apply the approved Tailscale ACL.
   Obtain a certificate for the concrete Tailnet hostname. Do **not** enable
   Funnel. Do not copy validation self-signed certificates.
5. Build or obtain a reviewed release directory with `MANIFEST.sha256` and the
   three currently wired Go binaries (`core-api`, `core-worker`, `nmcp-admin`).

## Configuration and install

Create separate files from `infra/ubuntu/env/*.example`. Replace every marker,
use a random HMAC value of at least 32 bytes, and keep the API address exactly
`127.0.0.1:8080`. `NMCP_FILE_BASE_URL` must name the separate File Server TLS
listener and end in `/files/`.
The three files and TLS inputs must be regular root-owned files below normalized,
root-owned paths with no group/world-writable parent. The installer captures
them through `O_NOFOLLOW` file descriptors before validation/copy, rejects
duplicates or extra/missing environment keys, requires all DSNs to match and use
literal loopback port 5432 and database `nmcp`, permits only `sslmode=disable`
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
  /etc/nmcp/tls-current/*.pem /var/lib/nmcp/media /var/lib/nmcp/backups
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
