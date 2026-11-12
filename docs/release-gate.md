# Issue #21 release gate: lanes, evidence and limits

This is the #21 gate map. Each published requirement that #21 owns
([traceability](traceability.md): ARC-01, ARC-02, ARC-07, ARC-13, WRK-08–10,
WRK-13, OPS-01, OPS-05–OPS-10) is mapped to the lane that proves it, the
binaries it runs on, and its pass condition. An item that cannot run in the
team environment is recorded as a limit with its owning issue and is never
counted as passed. Results (run and job IDs, measured values) are attached to
the #21 pull request for the exact head that is merged.

## Deviations from the design snapshot

The published design ([`docs/design-snapshot/`](design-snapshot/README.md),
including `worker-spec.html`) is immutable. Where #21 changed behaviour against
it, the change is recorded here once, with its decision, code and evidence, so
#22 and review have one place to check.

| Change | Decision | Code | Evidence / rule text |
| --- | --- | --- | --- |
| Untagged SDR video is accepted with conventional color inference (8-bit only; HD (width >= 1280 or height > 576) BT.709, SD BT.601 matrix `smpte170m` with BT.709 primaries; unspecified range limited except full for `yuvj*`; partial tags must agree; untagged 10-bit+, PQ/HLG, BT.2020 or HDR side data still refused). Recorded as `color_inferred`, an `inferred:` rendition audit `input_color`, and a Worker log line; tagged input is unchanged. | Customer decision 2026-11-12 (option 1) | [`internal/videohelper/probe.go`](../internal/videohelper/probe.go) (`inferSDRColor`), [`internal/videoprocessor/processor_linux.go`](../internal/videoprocessor/processor_linux.go), [`internal/transformexecutor/executor_linux.go`](../internal/transformexecutor/executor_linux.go) | [video helper README, "Source color"](../internal/videoprocessor/helper/README.md#source-color); [video testdata README](../internal/videoprocessor/testdata/README.md); untagged 1080p MP4 in the E2E certification matrix; untagged 640x480 MP4 uploaded by the E2E chain (real zscale, BT.601 matrix) |
| The still helper advertises seven more camera RAW formats (CR2, CR3, RAF, NEF, ORF, RW2, ARW) beside DNG, each proven against the separately compiled LibRaw reference at the DNG pixel standard. Before this, the bundled profiles listed MIMEs the helper refused and could never be certified. | Leader decision 2026-11-12 (option A; no profile descope) | [`internal/stillprocessor/helper/src/image_pipeline.cpp`](../internal/stillprocessor/helper/src/image_pipeline.cpp) (`kProvenRawMimes`), [`internal/stillprocessor/processor_integration_linux_test.go`](../internal/stillprocessor/processor_integration_linux_test.go) | [still testdata README](../internal/stillprocessor/testdata/README.md); pins in [`test/e2e/testdata/fixtures.json`](../test/e2e/testdata/fixtures.json) |
| Profile certification requirements follow the Worker's executable routes: a probe-animation MIME other than `image/webp` under an all-frames recipe needs no still output, because the Worker always produces animated WebP there. | Leader decision 2026-11-11 (D1: no substitute route; fix the derivation) | [`internal/database/migrations/0024_profile_route_certification.sql`](../internal/database/migrations/0024_profile_route_certification.sql), [`internal/admin/profile_certify.go`](../internal/admin/profile_certify.go) | [`profile certify` contract](contracts/core-api.md) |
| A HEIF whose `ftyp` carries both still (`mif1`/`heic`) and sequence (`msf1`/`hevc`) brands is classified as a still image (its primary item), not refused as an image sequence; a still brand mixed with MP4 video brands (`mp42`/`isom`, e.g. nokiatech C049) stays refused. | #21 finding, leader decision 2026-11-11 | [`internal/metadata/detector.go`](../internal/metadata/detector.go) | [`docs/development.md`](development.md) (C049 note); `heif-libheif-example-mixed-brands` in [`test/e2e/testdata/fixtures.json`](../test/e2e/testdata/fixtures.json) |
| Activating a profile takes effect for transform claims only when the Worker next starts: it binds its claimable profiles to the capability envelope validated at startup, so jobs for a newly activated profile stay `queued` until a Worker restart. Documented as an operator step; live re-evaluation is a tracked follow-up. | #21 finding, leader decision 2026-11-12 | [`cmd/core-worker/main.go`](../cmd/core-worker/main.go) (`BindTransformClaims`) | [`profile activate` contract](contracts/core-api.md); [Ubuntu deployment, certification steps](ubuntu-deployment.md) |
| Known limit, not a behaviour change: profile certification probes outputs with the pinned `/opt/nmcp/current/bin/ffprobe`, while the API's upload metadata probe uses the distribution `/usr/bin/ffprobe` (the L3 guest provides the pinned binary at that path because it omits Ubuntu's ffmpeg). The two versions can differ in edge-case container parsing. | Reviewer B5, leader decision 2026-11-12 | [`internal/metadata/probe_linux.go`](../internal/metadata/probe_linux.go) (`DefaultToolPaths`) | [Ubuntu deployment, certification spec](ubuntu-deployment.md) |

## Lanes

| Lane (CI job) | Environment | Binaries | Scenario |
|---|---|---|---|
| `test` (CI) | Hosted runner, PostgreSQL service | Unit/integration builds | vet, `-race`, unit and real-PostgreSQL integration suites, including the in-process crash-point suites cited below |
| Compose E2E, release (infra lane, `Infra / Compose E2E issue 21`) | Real API, Worker with pinned helpers, PostgreSQL 17 and HTTPS Nginx (infra/validation) | **Release** core image, gated by `scripts/check-release-no-failpoints` and `go version -m` showing no build tags | `test/e2e` `TestReleaseGateChain` (L1) and `TestReleaseGateFaults` (L2) |
| Compose E2E, failpoints (`Infra / Compose E2E issue 21 failpoints`) | Same stack, separate project | **Instrumented** image built with `-tags nmcp_failpoints` | `test/e2e` failpoint scenario, built with `-tags nmcp_failpoints_e2e` |
| `Release gate / RAW and AV1 under one nmcp.slice` (CI root lane) | Isolated systemd 255 guest (nspawn) with the production units and sandbox | Release binaries | `TestReleaseGateResources` (L3) plus one real-unit ENOSPC case |
| `Backup / actual systemd all-pool cutover and separate resume` (CI root lane) | Same guest | Release binaries | Live backup/restore/cutover/resume with a second independent cycle, plus the sized RPO/RTO dataset |
| `Backup / offline two-cluster source-stop recovery and separate resume` (CI root lane) | Same guest, two PG17 clusters | Release binaries | Offline source-down recovery, external authorization, HBA promotion and real Go-timeout cleanup |
| Ubuntu deployment (PR-only) | Hosted Ubuntu 24.04 | Release scripts and units | Production install, configuration, firewall, TLS, systemd, migration/role compatibility (current = 24) |

## L1: end-to-end chain (release binaries)

`TestReleaseGateChain` runs these steps in order:
- The bundled `thumbnail/v1` and `standard/v1` drafts are certified on the
  stack's exact helpers with `nmcp-admin profile certify` (real transforms of
  the pinned fixture matrix, independent probes, one audited transaction),
  then activated with `profile activate`. Helper digests come from a
  separate same-image container.
- Upload: filename normalization, idempotent replay, and `duplicate_media`.
- The asynchronous job completes, producing standard (1920) and thumbnail
  (640) AVIF.
- List, detail and display.
- Original and rendition bytes are fetched through Nginx: GET digest and
  size, immutable/nosniff headers, HEAD, 206 Range, 416 and 405. They are
  cross-checked against the media volume.
- Delete, restore and purge: idempotent delete, list filters, files retained
  until purge, an idempotent purge enqueue, 404s after purge, the volume
  emptied, and job history retained.
- The change feed carries upload, rendition_current, logical_delete,
  restore and physical_purge, in increasing positions with the tombstone
  last.
- Regenerate produces a new current rendition and URL. Cleanup of the
  superseded rendition at retention 0 removes the old rendition and file,
  and never the current one: the owner-mode `cleanup --kind renditions`
  exits 0 with nothing pending once the Worker's sweep has removed it.

## L2: faults

Real-binary faults (release lane, `TestReleaseGateFaults`). After every case,
`nmcp-admin check --scope all` must seal, and every finding must be crash
residue the storage contract keeps: a recent attempt temporary file
(`recent_or_live_temp`) or an unreferenced published final (`final_orphan`).
Any reference-level finding (missing, size/sha mismatch, invalid current or
provenance, unexpected path or type) fails the case. Each target must have
exactly one current rendition served byte-exactly:
- Worker SIGKILL during an AV1 transform, reclaimed after the real 2-minute
  lease.
- Worker SIGSTOP past lease expiry, then SIGCONT, with no double publish.
- PostgreSQL loss: readyz and upload return 503, and the same
  idempotency key is accepted after recovery.
- Read-only and full media filesystem: 500/503 with no accepted row, then
  the same key is accepted after recovery.

Deterministic crash points (failpoint lane): every named upload, rendition
publish and purge point (listed in [development](development.md)) is fired
by an instrumented API or Worker that SIGKILLs itself. Each point's residue
is diffed against the previous sealed report: a `before_*_publish` point may
add only the interrupted attempt's temporary file (non-actionable); an
`after_*_publish` point adds exactly one repairable `final_orphan` of that
upload (matched by its SHA-256) or media; `after_db_commit` and every purge
point add nothing. Only the failpoint lane image contains these points; the
release lane proves their absence.
The in-process crash-point suites in `internal/storage`, `internal/upload`
and the purge/cleanup durability tests are supporting evidence only. They
do not replace this lane.

## L3: resources

The production budget is one `nmcp.slice` (CPUQuota=200%, MemoryMax=4G)
shared by every service. In the root guest, the effective `memory.max` and
`cpu.max` are asserted first. A 6000 px CR3 and a 1440p MP4 to AV1 are then
transformed through the real units. Pass means slice `memory.peak` stays
below 4 GiB and the `memory.events` oom/oom_kill/oom_group_kill counters
are unchanged. Per-unit peaks, `cpu.stat` and elapsed times are recorded,
not asserted. One real-unit ENOSPC case runs on a sized tmpfs media root.

Measured (CI PR run 43155480640, L3 job 130986308816, head 4c509de): the
CR3 (6000x4000) and 2560x1440 MP4 to AV1 transforms ran under one
`nmcp.slice` (CPUQuota 200%, MemoryMax 4G) with `memory.peak` 0.61 GiB and
no oom/oom_kill/oom_group_kill events; CPU was throttled once (1.9 ms). The
OOM counters are the gate; `memory.peak < MemoryMax` is recorded as headroom
evidence only, since the kernel enforces MemoryMax itself.

## RPO/RTO

The live cutover lane seeds 3000 distinct BMP originals (about 37 MB; a
sized CI dataset, not production scale) through the real API before the
first backup and logs RPO and end-to-end RTO with the dataset
size: media, jobs and database bytes. The offline lane logs its own RPO/RTO.
These numbers come from the CI runner and the seeded dataset size.
Real-hardware figures are #22 evidence.

## Limits (never counted as passed)

- Real Tailnet, Viewer, notification delivery, HDD timings and server
  reboot behaviour (#22).
- Real-device HEIC/HEIF files, including mixed still+sequence brands and the
  deliberately rejected HEIF+MP4 brand mix (#22 checklist).
- Production-host `profile certify` of the bundled profiles on the real
  helpers before activation (#22).
- systemd versions other than 255. The backup unit gate fails closed on any
  other manager version, so a different version needs re-derived allow-sets.
- Hosted-runner performance numbers are not production capacity.
