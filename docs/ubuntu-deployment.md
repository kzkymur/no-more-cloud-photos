# Ubuntu production deployment

This runbook installs the repository's production contract on Ubuntu 24.04.
It intentionally requires real host values; example addresses, credentials, and
certificates are never promoted to production defaults.

## Supported and blocked scope

The shipped units run PostgreSQL, migration, Core API, the current Core Worker,
and Nginx. The worker currently performs lease reclamation only. It does not
run transform jobs or `nmcp-still-helper` until issue #14 connects that runtime
interface.

No maintenance, backup, restore, cleanup service, or timer is installed. Issue
#19 has not yet defined the scheduled due/no-op command, exit codes, environment
names, or database role. A timer that always runs the manual command would
duplicate database policy and is unsafe. When #19 lands, a timer may only wake
the application: due-time, last-success, retention, locking, and no-op decisions
must remain database-backed application behavior. This is a fail-closed blocked
boundary, not a claim that scheduled maintenance works.

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
| `/opt/nmcp/releases/<id>` | `root:root`, not group/world writable | immutable release |
| `/opt/nmcp/current` | root-owned atomic symlink | active release |
| `/etc/nmcp/*.env` | `root:root 0600` | per-process environment; no CLI secrets |
| `/etc/nmcp/tls-*.pem` | `root:root 0600` | Nginx systemd credentials |
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
   Keep PostgreSQL on a Unix socket or loopback; do not expose port 5432.
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

As root, install host assets and the first release using the scripts documented
by `--help`:

```sh
sudo scripts/install-ubuntu-host \
  --config-dir /root/nmcp-config \
  --api-listen "<TAILNET_IP>:<API_TLS_PORT>" \
  --files-listen "<TAILNET_IP>:<FILES_TLS_PORT>" \
  --tailnet-hostname "<HOSTNAME>" \
  --tls-cert /root/cert.pem --tls-key /root/key.pem
sudo scripts/install-ubuntu-release stage /root/nmcp-release <RELEASE_ID>
sudo scripts/install-ubuntu-release activate <RELEASE_ID>
```

Activation verifies hashes and permissions, moves the active symlink atomically,
runs migration, starts/restarts the units, waits for `/readyz`, and records the
release. A failure before a completed migration restores the old link. A failure
after migration does **not** automatically run old code against a potentially
new schema.

Rollback is fail-closed:

```sh
sudo scripts/install-ubuntu-release rollback <PREVIOUS_RELEASE_ID>
```

The target release's `nmcp-admin migrate status --json` must report fully
compatible (exit 0) against the current database before the link changes. Any
pending/drift/error status refuses rollback; database restoration is a separate,
explicit disaster-recovery operation. Never delete the last known-good release
during activation.

## Firewall and Tailnet checks

Binding to the concrete Tailnet address is mandatory but does not replace ACLs
or the host firewall. Adapt these commands to the approved firewall manager;
do not flush an existing ruleset or risk the management session.

```sh
tailscale status
tailscale ip -4
tailscale ip -6
tailscale funnel status                 # must show no Funnel listener
sudo ss -lntup                          # API direct only 127.0.0.1:8080
sudo ufw status verbose                 # record existing policy first
sudo ufw allow in on tailscale0 to <TAILNET_IP> port <API_TLS_PORT> proto tcp
sudo ufw allow in on tailscale0 to <TAILNET_IP> port <FILES_TLS_PORT> proto tcp
```

If UFW is not the approved manager, encode the equivalent input-interface plus
destination-address rules in the managed nftables/firewalld configuration.
Do not add wildcard `0.0.0.0`, `::`, LAN-interface, WAN-interface, or Funnel
rules. From an authorized Tailnet client, both TLS listeners must work. From a
LAN client with Tailscale disabled, both must fail. Run the latter from another
machine: a local request is not evidence of a non-Tailnet denial.

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
sudo systemctl show postgresql@'*'.service nmcp-api.service nmcp-worker.service \
  nmcp-files.service -p Slice -p User -p Group
sudo ss -lntup
sudo stat -c '%U:%G %a %n' /etc/nmcp /etc/nmcp/*.env \
  /etc/nmcp/tls-*.pem /var/lib/nmcp/media /var/lib/nmcp/backups
curl --fail --show-error http://127.0.0.1:8080/readyz
```

Against the real File Server TLS URL, repeat the #36 matrix: GET; HEAD without a
body; exact, open-ended, suffix, multi-range 206; unsatisfiable 416; conditional
ETag 304; correct MIME; private immutable cache; and byte equality. Confirm 405
for POST and 404/400 for directory listing, arbitrary name, backup/tmp paths,
symlink, raw dot segment, encoded separator/dot, malformed escape, wrong shard,
and noncanonical case. Verify Nginx cannot create, rename, or unlink in either
bind mount. The repository validation environment supplies this complete HTTP
assertion implementation; production evidence must use the real TLS endpoint.

Stop/start each unit and PostgreSQL, and verify readiness becomes unavailable
then recovers. Send SIGTERM and confirm graceful shutdown within unit timeouts.
Reboot only with explicit host approval, then confirm mount, PostgreSQL,
migration, API, Worker, Nginx, and readiness ordering. If the real host,
authorized/negative clients, firewall authority, or reboot permission are not
available, mark those checks **not executed**; static or container checks must
not be reported as substitutes.
