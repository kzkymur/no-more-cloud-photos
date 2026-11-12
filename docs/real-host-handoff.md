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

Then check that both bundled profiles are active (uploads create no transform
job otherwise):

```sh
curl --fail --silent -H 'Accept: application/json' \
  "https://<TAILNET_HOSTNAME>:<API_TLS_PORT>/profiles?status=active" | jq -r '.items[] | "\(.key)/v\(.version)"'
```

Expected: `standard/v1` and `thumbnail/v1`. If either is missing, do item 9
first (certify, activate, restart the Worker), then return here.

Helpers used below (paste into your shell on a Tailnet client; they print only
IDs and statuses). If `upload` prints `-` as the job ID, no active profile
accepted the file and nothing was converted: record that fixture as
NOT_EXECUTED (and check the profiles above), not as PASS.

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

## Recommended order

Run the items in this order. The numbers match the #22 checklist, not the
order:

1. **0** preflight, then **9** (both `nmcp-admin` modes, certify, activate,
   Worker restart). Item 9's `check` runs here, on a quiet system, before items
   7 and 8 add uploads and restores (see #54).
2. **1**, **2**, **3**, **5** (read-only or upload-only checks).
3. **6** [window] installs the backup configuration with the notification
   hook already set in `backup.env`, then **4** tests the notification.
4. **7** (device files), then **8** [window] (RPO/RTO) last.

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

## 4. Backup failure notification delivery [window]

Goal: a failed backup produces exactly one notification that actually
arrives, and a successful one produces none. Run this **after item 6**, which
installs `backup.env` with `NMCP_BACKUP_NOTIFY_HOOK`. Follow
`docs/backup-operations.md`, "Failure notifications and measurements".

**Forcing a run.** The shipped service only does a due-check, so after item 6
a manual start is usually `not_due`. "Wake timer, manual runs and results"
requires a forced run to use the same hardened unit with only `ExecStart`
changed. Do that with a runtime drop-in and remove it afterwards:

```sh
force_backup() {
  sudo mkdir -p /run/systemd/system/nmcp-backup.service.d
  printf '[Service]\nExecStart=\nExecStart=/opt/nmcp/current/bin/nmcp-admin backup run --json\n' |
    sudo tee /run/systemd/system/nmcp-backup.service.d/90-handoff-force.conf >/dev/null
  sudo systemctl daemon-reload
  sudo systemctl start nmcp-backup.service; echo "unit exit: $?"
  # Remove the file AND the directory: validate-backup-install rejects any
  # entry, even an empty /run/systemd/system/nmcp-backup.service.d.
  sudo rm /run/systemd/system/nmcp-backup.service.d/90-handoff-force.conf &&
    sudo rmdir /run/systemd/system/nmcp-backup.service.d &&
    sudo systemctl daemon-reload
  sudo scripts/validate-backup-install --installed; echo "validate exit: $?"
  sudo journalctl -u nmcp-backup.service --since -10min -o cat | grep -E '"(status|outcome|error_code|notification_status)"' | tail -n 4
}
# Stop the timer for the whole item: a timer wake while the drop-in exists
# would also run without --if-due. Do not start a restore while it exists
# (restore --cutover refuses a modified backup unit).
sudo systemctl stop nmcp-backup.timer
```

1. **Success case.** `force_backup`. Expected: outcome success, `validate exit: 0`;
   your endpoint receives **no** notification.

   If a `force_backup` is interrupted (Ctrl-C, lost session), the runtime
   drop-in may remain: run its last cleanup lines by hand
   (`rm` the `.conf`, `rmdir` the directory, `daemon-reload`) and
   `sudo scripts/validate-backup-install --installed` until it exits 0 before
   continuing. `/run` is also cleared at reboot.
2. **(a) Failure after the run is recorded.** Make the dump login unable to
   log in, force one run, and restore it immediately:

   ```sh
   sudo -u postgres psql -d nmcp -c 'ALTER ROLE nmcp_backup_dump NOLOGIN'
   force_backup
   sudo -u postgres psql -d nmcp -c 'ALTER ROLE nmcp_backup_dump LOGIN'
   ```

   Expected: the unit fails (nmcp-admin exit 1); the run is recorded with status
   `failed` and error code `prepare`; your endpoint receives exactly one
   notification whose stdin was `{"backup_id":"<id>","phase":"prepare","status":"failed"}`
   and whose `backup_id` matches the failed run; the run's
   `notification_status` is `succeeded` if your hook exited 0, `failed`
   otherwise. Nothing is dumped or published, and no data changes.

   **Mandatory restore check**, before anything else:

   ```sh
   sudo -u postgres psql -d nmcp -Atc "SELECT rolcanlogin FROM pg_roles WHERE rolname='nmcp_backup_dump'"   # t
   sudo scripts/validate-backup-install --installed                                                          # exit 0
   sudo -u postgres psql -d nmcp -X -v ON_ERROR_STOP=1 -f /usr/local/libexec/nmcp/postgresql/verify-runtime-role-graph.sql
   ```

   Expected: `t`, exit 0, and the role graph verifier succeeds. While the
   login is disabled every backup fails at `prepare`, and a restore taken in
   that window is refused (role graph mismatch); after the revert both are
   normal again. The failed run stays in history; that is intended.
3. **(b) Database unreachable (needs a release with #57).** A backup that
   fails before its run is recorded now also notifies, exactly once. On an
   older release it sends nothing, so record this case as NOT_EXECUTED,
   reason "release without #57". With #57:

   ```sh
   PG_UNIT=$(cat /etc/nmcp/postgresql-unit)
   sudo systemctl stop nmcp.target
   sudo systemctl stop "$PG_UNIT"
   force_backup                       # expected to fail
   sudo systemctl start "$PG_UNIT"
   sudo systemctl start nmcp.target
   curl --fail --silent -H 'Accept: application/json' "$API/readyz"
   ```

   Expected: the unit fails (non-zero exit); the journal has one line
   `"backup failed before a history row existed"` with `"cause":"db_unreachable"`
   and `"notification":"succeeded"` (or `failed` if your hook exited non-zero);
   your endpoint receives exactly one notification whose stdin was
   `{"status":"failed","phase":"precondition","cause":"db_unreachable","at":"<UTC>"}`.
   It has no `backup_id`, and nothing new appears in backup history, because
   the database was down. Such a notification is not retried by a later run.
   `/readyz` returns to 200 after the restart, and `validate exit: 0`.
4. `sudo systemctl start nmcp-backup.timer`.

Report: success run (no notification: PASS/FAIL); case (a): unit exit,
failure UTC, notification received UTC, delay in seconds, ID matched yes/no,
`notification_status`, and the three restore-check results; case (b):
NOT_EXECUTED (release without #57) or its result. Never paste the hook URL or
message body.

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

## 6. Real-host install-backup-config [window]

Goal: the backup configuration installs and validates on your host, with the
notification hook already configured, so that item 4 can test it. Follow
`docs/backup-operations.md`, "Local installation prerequisites", "Wake timer,
manual runs and results" and "Failure notifications and measurements".

What `scripts/install-backup-config` actually does, in order (so plan a window):

1. Takes the deployment locks, and **stops** `nmcp-backup.timer` and
   `nmcp-backup.service`.
2. Captures your four input files and installs them root:root 0600:
   `/etc/nmcp/backup.env`, `/etc/nmcp/backup.pg_service.conf` and the two
   `.pgpass` files under `/etc/nmcp/credentials`. It also installs the
   PostgreSQL dependency drop-in for `nmcp-backup.service`.
3. Runs `validate-backup-install --installed`.
4. Runs `configure-backup-source-hba`: it **rewrites the backup logins'
   `pg_hba.conf` lines and reloads PostgreSQL** (`pg_reload_conf()`), then
   checks the cluster identity did not change during the reload.
5. Reloads systemd and validates again with `--fresh-manager`.
6. **Starts `nmcp-backup.service` immediately** (a due-check wake: the first
   install normally runs a backup now), then **enables `nmcp-backup.timer`**.

Before running it, install your reviewed notification hook
(`/usr/local/libexec/nmcp/backup-notify`, root:root 0755, no arguments, JSON
on stdin) and put `NMCP_BACKUP_NOTIFY_HOOK=/usr/local/libexec/nmcp/backup-notify`
in the `backup.env` you install here.

```sh
sudo install -d -m 0700 /root/nmcp-backup-config
# Create there, from infra/ubuntu/env/backup.env.example and the docs, root:root 0600:
#   backup.env (with NMCP_BACKUP_NOTIFY_HOOK), backup.pg_service.conf,
#   backup-dump.pgpass, backup-control.pgpass
sudo scripts/install-backup-config --config-dir /root/nmcp-backup-config
sudo scripts/validate-backup-install --installed
systemctl is-enabled nmcp-backup.timer
sudo journalctl -u nmcp-backup.service --since -30min -o cat | grep -E '"(status|outcome)"' | tail -n 3
```

Expected: both scripts exit 0; `install-backup-config` ends with "Backup
configuration installed; wake timer enabled. No backup success claimed." and
`validate-backup-install --installed` prints its "PASS local install
metadata/client checks" line (which also lists what it does NOT prove); the timer
is `enabled`; `/readyz` stays 200 after the PostgreSQL reload. The service's
structured JSON outcome in the journal is a successful backup, or
`not_due` (exit 4) if the database policy did not consider one due. Neither
`not_due` nor unit success alone is a backup success (see "Wake timer, manual
runs and results").

Report: exit codes, the timer state, and the newest run's status and size
(no paths or IDs needed).

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

## 8. RPO/RTO [window]

Goal: real recovery point and recovery time with your data volume. CI only
measured a small seeded dataset (`docs/release-gate.md`, "RPO/RTO"). Follow
`docs/backup-operations.md`, "Operator restore boundary (acceptance still
required)" and "Explicit cutover and separate resume".

> **Never cut over on the production host until #59 ships.** A production
> cutover needs writes quiesced before the final backup, which has no
> supported procedure yet (#59). Cutover makes the restored database
> authoritative and **discards** every write accepted after the backup's
> snapshot cutoff (`started_at` in backup history).
>
> **Never stop `nmcp.target` before an online cutover.** Online cutover's
> preflight requires `nmcp-migrate` active and the API and Worker running;
> with them stopped the cutover fails into `cutover_failed` with the services
> stopped.
>
> **If a restore or cutover step fails, stop and report.** Do not restart or
> start units around a failed operation ("Explicit cutover and separate
> resume"). A failed cutover (`cutover_failed`) leaves the timer, backup, API
> and Worker stopped and the source fenced in maintenance, with no supported
> recovery today. #58's abandon refuses any restore that recorded a cutover
> phase; a failed cutover is tracked in #59.
>
> The offline restore mode (`--offline-source`) needs the source and the
> candidate in two different PostgreSQL 17 clusters; it is not the path on a
> standard single-cluster host.

Record the dataset size first, in every branch: `du -sh /var/lib/nmcp/media`,
media count from `GET /media`, and database size
(`sudo -u postgres psql -Atc "SELECT pg_size_pretty(pg_database_size('nmcp'))"`).

**A. Production cutover: NOT_EXECUTED.** Record it as NOT_EXECUTED, reason
"planned production cutover requires #59".

**B. Production validation-only drill (requires #58).** This measures backup
and restore up to validation on your real data, then abandons the restore so
production returns to normal service. It needs the abandon command from #58;
until a release with #58 is deployed, record B as NOT_EXECUTED, reason
"requires #58". With #58:

1. Keep `nmcp.target` running. Stop `nmcp-backup.timer`, take a backup with
   `force_backup` (item 4) and note its `started_at`. Start a monotonic clock
   when the restore begins.
2. Prepare the candidate and its receipt as described in "Operator restore
   boundary (acceptance still required)" (the receipt must be less than an
   hour old), then run the restore prepare (an owner command, with the restore
   environment that section lists) and its validation. On entry the source
   goes into `operator restore` maintenance and refuses every write (503)
   until step 3. Prepare does not drain: if a write was in flight at that
   instant, prepare fails without entering maintenance; simply run it again.
3. Abandon the restore as root, following `docs/backup-operations.md`,
   "Abandoning a restore without cutover". It reads the source's `admin.dsn`,
   a `check.dsn` that names the source, `NMCP_RESTORE_LEDGER_ROOT` and
   `NMCP_STORAGE_ROOT`:

   ```sh
   nmcp-admin restore --operation-id <OP> --abandon --yes --json   # root only (non-root: exit 2)
   ```

   Expected: exit 0, and stdout is the maintenance status with mode `normal`
   and a successful check report. In order, the ledger records `abandoned`, a
   fresh full check runs as `nmcp_check`, and an audited `restore_abandon`
   returns the source to normal. Abandon starts and stops no units:
   `nmcp.target` keeps running throughout, and the API and Worker answer in
   maintenance mode until this step. Exit 4 means a refusal that changed
   nothing: a cutover or offline phase was recorded, the operation is
   unknown, the live database is not the source, or maintenance belongs to
   something else. The other exit-4 case is a check with findings, after
   `abandoned` is already recorded: fix the findings, then rerun abandon.
   Start `nmcp-backup.timer` again (you stopped it in step 1; backups during
   the maintenance would only fail with cause `maintenance`). Stop the clock
   when `/readyz` is 200 and one upload completes.
4. Abandon never drops the candidate. Drop the unused candidate database
   yourself as the PostgreSQL cluster administrator (the API and Worker never
   point at it before cutover). Keep the ledger, with its `abandoned` record,
   and the receipt with your deployment record.

**C. Rehearsal host with cutover (for cutover RTO numbers).** On a separate
host built from the same release and fed a copy of a production backup
artifact, with `nmcp.target` **running** throughout:

1. Stop `nmcp-backup.timer`, back up with `force_backup`, note `started_at`,
   and start the clock at restore start.
2. Prepare and validate as in B step 2, then cut over as root
   (`restore --operation-id <OP> --cutover --yes --json`, see "Explicit
   cutover and separate resume") with the API and Worker running: its
   preflight requires `nmcp-migrate` active and both services running. The
   cutover itself stops the timer, backup, API and Worker, switches
   configuration, and starts only the API and Worker. Then point
   `check.dsn`/`repair.dsn` at the candidate.
3. Resume explicitly: the separate root post-restore
   `maintenance resume --yes --json` from that section. Start
   `nmcp-backup.timer` again (cutover leaves it off). Check `/readyz` and one
   upload end to end, then stop the clock. If the cutover fails, record it
   and discard the rehearsal host.
4. Discard the rehearsal host or its cluster afterwards; never promote it to
   production.

Expected: B and C succeed step by step. RPO = the moment the drill treats as
the failure, minus the snapshot cutoff. RTO = the monotonic time from restore
start to the first successful end-to-end upload after resume (C) or after
abandon (B), with per-phase times.

Report: A NOT_EXECUTED (#59); B result or NOT_EXECUTED (#58); C result with
the rehearsal host labelled as such; dataset size (media bytes, count,
database bytes); RPO, RTO and the phase breakdown for each executed branch.

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
   Expected: exit 0, sealed report with 0 findings. Exit 1 with
   `storage_scan_unstable` means storage changed during the scan (for example
   right after a Worker restart): wait briefly and rerun (#54).
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
| #59 | A planned production cutover needs writes quiesced before the final backup; there is no supported procedure yet. | Item 8 A is NOT_EXECUTED. Never cut over on production, and never stop `nmcp.target` before an online cutover. |
| #57 | On releases before #57, a backup that fails before its run is recorded (for example, database unreachable) writes no history and sends no notification. With #57 it notifies once (`phase` `precondition`, a `cause`, no `backup_id`) but still writes no history and does not retry the notification. | Item 4(b) needs a release with #57. On older releases, watch `nmcp-backup.service` failures in the journal. |
| #58 | Before #58, a restore cannot be abandoned: without cutover the source stays in `operator restore` maintenance. #58 adds `restore --operation-id OP --abandon` for restores that have not cut over; it never drops the candidate, and a failed cutover is not covered (#59). | Item 8 B needs a release with #58; drop the candidate yourself and keep the ledger and receipt. |

## Reporting back

Post the blocks to issue #22, one comment per item. When an item fails, add
what you saw (status, code, counter, timing) and nothing private; the team
will reproduce from the release SHA and fixture SHA. Items that need a
decision (for example a device file refused in item 7, or a same-disk layout
in item 5) are tracked as follow-up issues, not as silent passes.
