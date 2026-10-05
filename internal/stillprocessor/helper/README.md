# Native still-image helper

This directory owns protocol-v1 executable `nmcp-still-helper`. It is a C++20
program and dynamically links libvips, libheif, LibRaw, libaom, and lcms2. The
normal build places it at `<build>/bin/nmcp-still-helper`:

```text
cmake -S internal/stillprocessor/helper -B build/still-helper
cmake --build build/still-helper
ctest --test-dir build/still-helper --output-on-failure
```

For machines without the image dependencies, the pure protocol/geometry/hash
tests can be built with `-DNMCP_BUILD_HELPER=OFF`.

## Closed behavior

The CLI flag order and cardinality are fixed by `internal/stillprocessor`
protocol v1. The helper emits one exact JSON envelope on stdout and never puts
codec diagnostics, input paths, or library error strings in JSON. Input MIME is
explicitly dispatched; no generic loader or MIME sniff fallback exists.

Advertised inputs are JPEG, PNG, static WebP, HEIC, HEIF, BMP restricted to
uncompressed BI_RGB 24/32-bit, and the eight RAW MIME types in the Go registry.
WebP page count and HEIF top-level image count must both be exactly one.

Embedded RGB ICC profiles are opened by lcms2 and converted to the supplied,
wrapper-hash-pinned sRGB profile. Invalid/non-RGB profiles fail closed. Untagged
inputs are explicitly treated as sRGB. RAW uses camera white balance, the camera
matrix, 16-bit processing, disabled automatic brightness, and LibRaw sRGB
output before the final 8-bit AVIF quantization. Orientation is applied before
the no-crop, no-upscale resize. Alpha inputs use AVIF 4:4:4; opaque inputs use
4:2:0. libheif is required to select its AOM encoder, with one thread, and all
writes are bounded before reaching the caller-owned output.

## HDR boundary

HDR is deliberately not claimed. HEIF NCLX inputs, including BT.2020 PQ and
HLG, return `unsupported_input`. Correct BT.2446 Method A needs a normative,
reviewable implementation, explicit mastering/display assumptions, and numeric
reference vectors. Treating an absent/partial NCLX description as HDR or using
an arbitrary display peak would be ambiguous, so this helper does neither.

## Verification status

The dependency-free tests cover strict CLI parsing, limits, resize edge cases,
BMP restrictions, stable errors/JSON escaping, and SHA-256. Codec integration
requires the deployment versions of all five libraries and real fixture tests;
see the repository's production certification boundary before enabling it.

At implementation time this workspace had neither CMake/pkg-config nor the
development headers/libraries, so the linked target was not compiled here. The
libheif calls were checked against the v1.23.5 public C headers. The libvips,
LibRaw, libaom, and lcms2 calls use their documented C APIs but remain compile-
and fixture-unverified in this environment. In particular, the selected AOM
plugin must expose the `threads` and `chroma` parameters; their absence fails
closed as `encode_failed` rather than silently using different settings.
