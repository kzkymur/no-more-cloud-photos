# Pinned FFmpeg fixture-tool acquisition

## Identity and evidence

This change does **not** change the trusted version, URL or SHA256 in
`internal/videoprocessor/testdata/fixture-tool.lock`. The binary is CI-only for
fixture generation / independent cross-version decoding, not a production codec.

* Pin introduced in `8ac405cbc3356b2bda08a38580cf9fe75d599496` (2026-10-04).
  The lock records John Van Sickle FFmpeg 6.0.1, upstream FFmpeg source and GPL
  license/use. This is repository pin provenance, not a vendor signed manifest.
* Expected SHA256 is
  `28268bf402f1083833ea269331587f60a242848880073be8016501d864bd07a5`.
  Earlier independent project audit on October 12 recorded a matching canonical
  HTTP 200 download of 41,164,188 bytes. An October 18 15:55:54 UTC measurement
  independently matches the same pin: HTTP 200, `application/x-xz`, 41,164,188
  bytes, Last-Modified November 11 2023, XZ CRC64 archive. Only checksum and
  archive listing were inspected, no downloaded binary was run.
* [Canonical archive](https://johnvansickle.com/ffmpeg/old-releases/ffmpeg-6.0.1-amd64-static.tar.xz)
  still serves the expected bytes. The old-release directory has no corresponding
  vendor manifest link; the `.md5` sidecar returns 404. Neither is a basis for
  replacing the existing stronger repository pin.

CI run [39142314504](https://github.com/kzkymur/no-more-cloud-photos/actions/runs/39142314504),
job 117719058045, failed SHA verification before any product tests. The old step
retained neither actual digest, HTTP identity nor response payload. It is thus
**unknown** whether that specific failure was a bad HTTP 200 response, corrupted
transfer, substitution or another input discrepancy. Current matching bytes do
not prove what that failed runner received. No upstream artifact update has been
established, and no checksum was changed to match a download.

## Acquisition contract

`scripts/fetch-pinned-ffmpeg` reads the unchanged lock, requests its exact HTTPS
URL once with bounded connect/total time and maximum bytes, records curl status,
effective URL, MIME type, sizes, headers and actual SHA256, then requires HTTP 200,
exact byte count, original SHA256 and XZ integrity **before** any extraction.
No automatic digest retry or CI rerun occurs. Failures stay red.

The optional Actions cache key and filename contain the full expected SHA256.
Cache contents are untrusted: every restore is rehashed, size checked and XZ
verified before use. A corrupt cache fails instead of being silently replaced.
There are no broad restore keys. Original `.tools` filenames and all subsequent
build, native, real-PostgreSQL and race commands are unchanged.

`identity.json`, `curl.json`, headers and stderr remain in the job workspace.
Failure uploads that evidence and any bounded rejected response for seven days.
Rejected bodies are untrusted data, not executable artifacts. A successful fetch
promotes only verified bytes into the cache; logs still contain complete identity.
This produces reproducible accepted input and inspectable failures without
claiming to have retrospectively identified the missing historical response.

Unprivileged unit counterexamples cover HTTP errors, HTTP-200 HTML, truncation,
corrupt cache, exact matching bytes and cache revalidation. No test contacts the
upstream or executes downloaded programs.
