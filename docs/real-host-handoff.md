# Real-host handoff (issue #22)

This runbook is for the owner of the production host. It covers the checks the
team cannot run in CI: your devices, your network, your disks, your
notification endpoint and your data volume. Everything that can be automated
is already gated in CI by issue #21 (`docs/release-gate.md`); this document
does not repeat those gates and does not replace them.

Run it on a host deployed from a release that contains issue #21 (PR #49),
following `docs/ubuntu-deployment.md` and `docs/backup-operations.md`.
Sections of those documents are referenced **by heading**.

## Ground rules

- **Never paste secrets or private identifiers** into the issue: no passwords,
  DSNs, API tokens, hook URLs, private keys, IP addresses, Tailnet host names,
  ACL identities, full firewall/ACL policies, raw journals or `curl -v`
  output. Keep raw evidence privately; report only the outcome lines below.
- Placeholders in commands look like `<TAILNET_HOSTNAME>`. Replace them on the
  host only.
- A prepared procedure is not a result. Mark an item PASS only after you ran
  it and saw the expected output.
- Steps that stop services, reboot, or cause a failure need an approved
  maintenance window. They are marked **[window]**.

## Evidence template

Report each check as one block in issue #22 (copy, fill in, keep it short):

```text
item:            <1-9>.<step>, e.g. 3.4
utc:             2026-11-20T10:15:00Z
performer:       <alias, not a real name if you prefer>
release:         <git SHA of the deployed release>
fixture:         <fixture name + sha256, if any>
host/device:     <anonymized, e.g. "server A, Ubuntu 24.04" / "phone B, iOS 18, Safari">
procedure:       docs/real-host-handoff.md item <n>
expected:        <copy the "Expected" line>
observed:        PASS | FAIL | NOT_EXECUTED
numbers:         <exit code, elapsed, counter deltas, sizes>
missing reason:  <only for FAIL / NOT_EXECUTED>
private ref:     <where you keep the raw evidence, e.g. "notes 2026-11-20 #3">
```

## 0. Preflight

```sh
readlink -f /opt/nmcp/current                    # release directory in use
sudo systemctl is-active nmcp.target nmcp-api.service nmcp-worker.service nmcp-files.service
curl --fail --silent https://<TAILNET_HOSTNAME>:<API_TLS_PORT>/readyz -H 'Accept: application/json'
```

Expected: one release directory; all four `active`; `/readyz` returns 200
`{"status":"ready"}`. Record the release SHA once; every later block refers to it.

Helpers used below (paste into your shell on a Tailnet client; they print only
IDs and statuses):

```sh
API=https://<TAILNET_HOSTNAME>:<API_TLS_PORT>
upload() {   # upload FILE -> prints "media_id job_id http_status"
  local out status
  out=$(mktemp); status=$(curl --silent --output "$out" --write-out '%{http_code}' \
    -H "Idempotency-Key: handoff-$(date +%s%N)" -H 'Accept: application/json' \
    -F "file=@$1" "$API/media")
  printf '%s %s %s\n' "$(jq -r '.media.id // .error.code' "$out")" "$(jq -r '.job.id // "-"' "$out")" "$status"
  rm -f "$out"
}
wait_job() { # wait_job JOB_ID [TIMEOUT_SECONDS] -> prints final status
  local deadline=$((SECONDS + ${2:-900})) status
  while :; do
    status=$(curl --fail --silent -H 'Accept: application/json' "$API/jobs/$1" | jq -r .status)
    case "$status" in succeeded|failed|cancelled) echo "$status"; return ;; esac
    [ "$SECONDS" -lt "$deadline" ] || { echo "timeout ($status)"; return 1; }
    sleep 5
  done
}
display_url() { curl --fail --silent -H 'Accept: application/json' "$API/media/$1/display" | jq -r .file_url; }
```

## 1. Viewer compatibility (AVIF, AV1 MP4, animated WebP)

Goal: the renditions your host produces display correctly on the devices and
browsers you will actually use: orientation, transparency, colour, animation
timing and loop, video seek and audio.

1. Build the fixture pack on any machine with a checkout of the deployed
   release (downloads are verified against pinned SHA-256 and sizes; synthetic
   inputs are generated with the pinned ffmpeg):

   ```sh
   python3 -I scripts/prepare-e2e-fixtures --out ~/nmcp-viewer-pack
   (cd ~/nmcp-viewer-pack && sha256sum -- * | sort -k2) > ~/nmcp-viewer-pack.sha256
   ```

   Expected: exit 0; `~/nmcp-viewer-pack/prepared.json` lists every file with
   its SHA-256; the `.sha256` list is your fixture record. Sources and licences
   are in `test/e2e/testdata/fixtures.json`.
2. Upload at least: `jpeg-exif-orientation-6`, one RAW (`raw-dng`),
   `heic-libheif-rainbow`, `webp-animated`, `gif-animated`,
   `video-mp4-generated` (has audio) and `video-quicktime-generated`:

   ```sh
   for f in ~/nmcp-viewer-pack/<FILE>; do read -r media job code < <(upload "$f"); \
     echo "$f $code $(wait_job "$job")"; echo "display: $(display_url "$media")"; done
   ```

   Expected: `201`, job `succeeded`, a display URL for each.
3. Open each display URL on every device/browser you use (phone, tablet,
   desktop; Safari/Chrome/Firefox as applicable).

Expected per device: still images upright (orientation 6 shown rotated
correctly), no colour cast compared with the original, transparency intact;
animated WebP/GIF loop with plausible timing and no ghosting; AV1 MP4 plays,
seeks, and the MP4 with audio has audible 440 Hz tone. A browser without AVIF
or AV1 support is a FAIL with reason "viewer lacks codec", not an app bug.

Report: one block per device/browser with observed PASS/FAIL per fixture.

## 2. Real CPU, RAM and disk under nmcp.slice

Goal: conversion time and memory headroom on your hardware, with all services
sharing one `nmcp.slice` (CPUQuota 200%, MemoryMax 4G). CI numbers
(`docs/release-gate.md`, "L3: resources") are not your capacity.

```sh
systemctl show nmcp.slice -p CPUQuotaPerSecUSec -p MemoryMax -p ControlGroup
for u in nmcp-api nmcp-worker nmcp-files; do systemctl show $u.service -p Slice; done
systemctl show "$(cat /etc/nmcp/postgresql-unit)" -p Slice
S=/sys/fs/cgroup/nmcp.slice
cat $S/memory.peak $S/memory.events $S/cpu.stat > ~/nmcp-slice-before.txt
df -hT /var/lib/nmcp/media /var/lib/nmcp-backup; df -i /var/lib/nmcp/media /var/lib/nmcp-backup
```

Then upload the largest representative files you expect (a camera RAW and a
long phone video) with `upload` / `wait_job`, note the job's queued, started
and finished timestamps from `GET /jobs/{id}`, and capture
`cat $S/memory.peak $S/memory.events $S/cpu.stat > ~/nmcp-slice-after.txt`.

Expected: every unit reports `Slice=nmcp.slice`; CPUQuota 2s per second and
MemoryMax 4G; jobs `succeeded`; `memory.events` `oom`, `oom_kill` and
`oom_group_kill` unchanged between before and after; `/readyz` stays 200
during conversion. `memory.peak` is cumulative since the slice started, so
report the before/after values, not an interval peak.

Report: per file: type, size, elapsed (queued→finished), memory.peak after,
OOM counter delta (must be 0), free space and inodes left.

## 3. Tailnet hostname, ACL, TLS and firewall

Goal: the API and file server are reachable only from authorised Tailnet
devices, never from your LAN or the Internet, and Funnel is off. Follow
`docs/ubuntu-deployment.md`, "Firewall and Tailnet checks" and "TLS renewal
and expiry", from an approved console **[window]**.

1. On the host:

   ```sh
   tailscale funnel status          # Expected: no Funnel listener
   sudo ss -lntup                   # Expected: API direct only 127.0.0.1:8080, PostgreSQL only local
   sudo ufw status verbose          # Expected: default deny incoming, exactly the two tailscale0 rules
   sudo scripts/verify-ubuntu-firewall --tailnet-ip <TAILNET_IP> \
     --api-port <API_TLS_PORT> --files-port <FILES_TLS_PORT>   # Expected: exit 0
   ```
2. From an **authorised** Tailnet device (not the host):

   ```sh
   curl --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}\n' https://<TAILNET_HOSTNAME>:<API_TLS_PORT>/readyz
   curl --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}\n' https://<TAILNET_HOSTNAME>:<FILES_TLS_PORT>/files/
   ```
   Expected: TLS verifies without `--insecure`; API 200; files server answers
   (a 403/404 for the bare path is fine; a TLS error is not).
3. From a Tailnet identity your ACL should **deny**: the same two commands.
   Expected: connection refused or timed out.
4. From a **LAN** machine with Tailscale off, and from an **Internet** host:
   probe the host's LAN/public address on `<API_TLS_PORT>`, `<FILES_TLS_PORT>`,
   `8080` and `5432`, IPv4 and IPv6 separately, keeping the real name for SNI:

   ```sh
   curl --connect-timeout 5 --max-time 10 --resolve <TAILNET_HOSTNAME>:<PORT>:<ADDRESS> \
     https://<TAILNET_HOSTNAME>:<PORT>/
   ```
   Expected: connect timeout or refusal for every port and address family.
   Reaching TCP or TLS at all (even an HTTP error) is a FAIL. A DNS failure is
   not evidence, and a probe from the host itself proves nothing.
5. Certificate: `echo | openssl s_client -connect <TAILNET_HOSTNAME>:<API_TLS_PORT> -servername <TAILNET_HOSTNAME> 2>/dev/null | openssl x509 -noout -subject -enddate`
   Expected: subject/SAN is your Tailnet name; expiry in the future; the
   expiry timer from "TLS renewal and expiry" is enabled.

Report: one block per probe origin (host, allowed device, denied identity,
LAN, Internet) with PASS/FAIL per port and address family. No addresses.

## 4. Backup failure notification delivery

Goal: a failed backup produces a notification that actually arrives, and a
successful one does not. Follow `docs/backup-operations.md`, "Failure
notifications and measurements".

1. Install your reviewed hook at `/usr/local/libexec/nmcp/backup-notify`
   (root:root 0755, no arguments, reads JSON on stdin) and set
   `NMCP_BACKUP_NOTIFY_HOOK` in backup.env through item 6.
2. Success case: run one manual backup as described in "Wake timer, manual
   runs and results". Expected: structured outcome success, **no**
   notification received.
3. Failure case **[window]**: with API and Worker stopped
   (`sudo systemctl stop nmcp.target`), stop the concrete PostgreSQL unit, run
   the manual backup once, then start PostgreSQL and `nmcp.target` again and
   check `/readyz`. This fails the backup without touching data. Expected: the
   backup reports a failure (non-zero exit, failure recorded in history) and
   your endpoint receives one notification whose backup ID matches.

Report: success run (no notification: PASS/FAIL); failure run: backup exit
code, failure UTC, notification received UTC, delay in seconds, ID matched
yes/no. Never paste the hook URL or message body.

## 5. Database backup and originals storage locations

Goal: you know which physical disks hold the originals, the database and the
backups, and you accept the same-disk risk.

```sh
for p in /var/lib/nmcp/media /var/lib/nmcp-backup "$(sudo -u postgres psql -Atc 'SHOW data_directory')"; do
  echo "== $p"; findmnt -T "$p" -o TARGET,SOURCE,FSTYPE; df -hT "$p"; done
lsblk -o NAME,TYPE,FSTYPE,SIZE,MOUNTPOINTS
stat -c '%U:%G %a %d %n' /var/lib/nmcp/media /var/lib/nmcp/media/originals /var/lib/nmcp/media/.quarantine
```

Trace every path to its **physical** disk (through LVM/partitions); two
partitions on one disk are one failure domain.

Expected: media, `.quarantine` share one device (same `%d`), nmcp:nmcp 0700;
the backup root is nmcp-backup 0700 outside media and outside Nginx.

> **Warning.** If originals, the database and the backups are on the same
> physical disk, one disk failure loses all of them. This release provides no
> cloud copy of originals and no point-in-time recovery. Keep an external copy
> if you need one.

Report: a mapping like "media: disk 1; database: disk 1; backups: disk 2"
(no device serials), and "same-disk risk accepted: yes/no".

## 6. Real-host install-backup-config

Goal: the backup configuration installs and validates on your host. Follow
`docs/backup-operations.md`, "Local installation prerequisites" and "Wake
timer, manual runs and results".

```sh
sudo install -d -m 0700 /root/nmcp-backup-config
# Create there, from the examples, root:root 0600:
#   backup.env, backup.pg_service.conf, backup-dump.pgpass, backup-control.pgpass
sudo scripts/install-backup-config --config-dir /root/nmcp-backup-config
sudo scripts/validate-backup-install --installed
```

Expected: both exit 0; the files are under `/etc/nmcp` and
`/etc/nmcp/credentials` as root:root 0600; the timer is enabled only after the
checks in "Wake timer, manual runs and results".

Report: exit codes and `systemctl is-enabled nmcp-backup.timer`.

## 7. Real-device HEIC/HEIF and video acceptance

Goal: files straight from your cameras and phones are accepted and converted.

1. Upload originals from each device you use: HEIC/HEIF stills, Live/motion
   photos if you keep them, phone videos (HDR and SDR), screen recordings, and
   camera RAW. Use `upload` and `wait_job` from item 0.

   Expected: `201` and job `succeeded`, then view the display rendition as in
   item 1. Untagged 8-bit SDR videos are accepted with inferred colour (HD
   BT.709, SD BT.601 matrix; see `docs/release-gate.md`, "Deviations from the
   design snapshot"); check that they do not look washed out. Untagged 10-bit,
   PQ/HLG, BT.2020 or HDR video is still refused by design.
   A rejection is not automatically a bug: record the HTTP status and error
   code (for example `415 unsupported_media_type`, `422 invalid_media`, or a
   failed job's error code), and the device/app that made the file.
2. Brand edge case: HEIF files that carry both still and sequence brands are
   accepted as stills. A HEIF still brand combined with an MP4 container brand
   is deliberately refused (`docs/development.md`). Reference file, Nokia
   conformance `C049.heic` (freely redistributable for conformance testing):

   ```sh
   curl -fsSLo C049.heic https://raw.githubusercontent.com/nokiatech/heif/f01283f3e64ccc6e6ab489bd556d454879069185/content/conformance/C049.heic
   echo 'c63c05ee4a19f661d8454c02f537e80118123010c9bbba24c33a79fc11ab2389  C049.heic' | sha256sum --check
   upload C049.heic
   ```

   Expected: checksum OK (536942 bytes); the upload is refused (4xx, no job).
   If one of **your** device files is refused the same way, report it: that is
   the decision point the team needs.

Report: per device: model class (e.g. "phone, 2024"), file types, accepted /
refused (status + code), visual check PASS/FAIL. No personal media.

## 8. Production-scale RPO/RTO

Goal: real recovery point and recovery time with your data volume. CI only
measured a small seeded dataset (`docs/release-gate.md`, "RPO/RTO").

Follow `docs/backup-operations.md`, "Operator restore boundary (acceptance
still required)" and "Explicit cutover and separate resume" **[window]**:

1. Record the dataset size: `du -sh /var/lib/nmcp/media`, media count from
   `GET /media`, database size
   (`sudo -u postgres psql -Atc "SELECT pg_size_pretty(pg_database_size('nmcp'))"`).
2. Take a manual backup; note its snapshot cutoff.
3. Restore into a new candidate database and validate it. Cut over only if
   your window allows it; otherwise measure up to validation and say so.
4. Resume and confirm `/readyz` and one new upload end to end.

Expected: restore and validation succeed; RPO = time of failure (or test
start) minus the snapshot cutoff; RTO = monotonic time from restore start to
resumed service, with per-phase times.

Report: dataset size (media bytes, count, database bytes), RPO, RTO and the
phase breakdown, and whether cutover was included.

## 9. nmcp-admin on the host, profile certification, Worker restart

Goal: both documented `nmcp-admin` modes work on your host, and the bundled
profiles are certified with the installed helpers before activation. Follow
`docs/ubuntu-deployment.md`, "Running nmcp-admin" and "Profile certification
before activation".

1. Capability mode (runs as `nmcp`):

   ```sh
   sudo systemd-run --quiet --pipe --wait --collect \
     -p User=nmcp -p Group=nmcp \
     -p EnvironmentFile=/etc/nmcp/capability.env \
     -p LoadCredential=check.dsn:/etc/nmcp/credentials/check.dsn \
     -p LoadCredential=repair.dsn:/etc/nmcp/credentials/repair.dsn \
     /opt/nmcp/current/bin/nmcp-admin check --scope all --json
   ```
   Expected: exit 0, sealed report with 0 findings.
2. Owner mode (runs as root):

   ```sh
   run_owner() {
     sudo systemd-run --quiet --pipe --wait --collect -p User=root \
       -p EnvironmentFile=/etc/nmcp/capability.env \
       -p LoadCredential=admin.dsn:/etc/nmcp/credentials/admin.dsn \
       /opt/nmcp/current/bin/nmcp-admin "$@"
   }
   run_owner migrate status --json
   ```
   Expected: exit 0, no pending migrations.
3. Certify each bundled profile with a spec as in "Profile certification
   before activation". Use the pinned probe
   `/opt/nmcp/current/bin/ffprobe` and digests measured on this host:

   ```sh
   sha256sum /opt/nmcp/current/bin/{nmcp-still-helper,nmcp-animation-helper,nmcp-video-helper,ffprobe} \
     /usr/bin/prlimit /opt/nmcp/current/share/sRGB2014.icc
   sudo cat /root/nmcp-certify-thumbnail.json | run_owner profile certify --file - --json
   run_owner profile activate --id 60000000-0000-4000-8000-000000000002 --json
   # repeat for standard/v1 (60000000-0000-4000-8000-000000000001)
   sudo systemctl restart nmcp-worker.service
   ```
   Expected: certify and activate exit 0. After the Worker restart, a new
   upload's job reaches `succeeded` (jobs for a newly activated profile stay
   `queued` until the Worker restarts, #50).

   Certify decodes your fixtures with the real helpers as uid `nmcp`, but not
   inside the Worker's systemd sandbox (#55): **use only fixtures you trust**.
   Re-certify after any release that changes a helper digest; certification
   rows are not yet bound to the running Worker's digests (#52).

Report: exit codes, profile IDs and versions activated, and the post-restart
upload result. Keep the certification JSON and evidence receipts privately
with your deployment record.

## Known limits and follow-ups

These are recorded in `docs/release-gate.md`, "Deviations from the design
snapshot" and "Limits (never counted as passed)", and tracked as open issues.
None blocks this runbook; note them when you interpret a result.

| Issue | Limit | What you may see / what to do |
| --- | --- | --- |
| #50 | A newly activated profile is picked up only after a Worker restart. | Jobs stay `queued` after `profile activate` until `systemctl restart nmcp-worker.service` (item 9). |
| #51 | Per-media directories emptied by purge are not removed. | Empty directories may remain under `originals/` and `renditions/` after purge (item 5 inventory). Not data loss. |
| #52 | Certification evidence is not bound to the running Worker's helper digests. | Re-run item 9 after every release that changes a helper, and keep the certification receipts with the deployment record. |
| #53 | The upload metadata probe uses the distribution `/usr/bin/ffprobe`, while certification uses the pinned `/opt/nmcp/current/bin/ffprobe`. | A file the distribution probe cannot read is refused at upload (item 7) even if the pinned toolchain could convert it; report it with the status code. |
| #54 | `nmcp-admin check` fails closed with `storage_scan_unstable` (exit 1) when storage changes during its scan, and does not retry by itself. | Common right after a Worker restart or during uploads. It is not a finding: wait briefly and rerun (the CI scenario allows up to 3 tries) before reporting item 9. |
| #55 | Root `profile certify` runs helpers as `nmcp` with the Worker's limits, but not inside the Worker unit's systemd sandbox. | Certify only trusted fixtures (item 9). |

## Reporting back

Post the blocks to issue #22, one comment per item. When an item fails, add
what you saw (status, code, counter, timing) and nothing private; the team
will reproduce from the release SHA and fixture SHA. Items that need a
decision (for example a device file refused in item 7, or a same-disk layout
in item 5) are tracked as follow-up issues, not as silent passes.
