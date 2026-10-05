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

CI and production builds first run `build-pinned-toolchain.sh` with an absolute
install prefix and scratch directory. It verifies every direct source archive
against `toolchain.lock` and builds libaom 3.8.2, lcms2 2.14, LibRaw 0.22.2,
libheif 1.23.5, and libvips 8.18.7 using the recorded reduced feature set.
System packages supply only the transitive build closure; pinning that complete
closure and the deployment image is the explicit issue #20 boundary.

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

The only accepted HDR envelope is HEIC/HEIF tagged with BT.2020 primaries,
BT.2020 non-constant-luminance matrix coefficients, explicit full/limited
range, and either PQ or HLG transfer. The fixed policy is a 1000-nit reference
HDR display to 100-nit SDR using ITU-R BT.2446 Method A. HLG uses the BT.2100
1000-nit system gamma 1.2 assumption. The implementation converts the decoded
BT.2020 RGB signal through the Method A luma/chroma mapping and then converts
primaries in linear light to BT.709/sRGB. Its achromatic curve is checked
against independent numeric reference vectors in `core_test.cpp`.

Contradictory ICC+HDR signaling, other primaries/matrices/transfers, and
ambiguous HDR are `unsupported_input`. Gain maps and Dolby Vision are not
claimed. Real PQ and HLG fixture/pixel evidence remains mandatory before the
capability can be certified; the numeric unit vectors alone are not that proof.

## Verification status

The dependency-free tests cover strict CLI parsing, limits, resize edge cases,
BMP restrictions, BT.2446A numeric vectors, stable errors/JSON escaping, and SHA-256. Codec integration
requires the deployment versions of all five libraries and real fixture tests;
see the repository's production certification boundary before enabling it.

At implementation time this workspace had neither CMake/pkg-config nor the
development headers/libraries, so the linked target was not compiled here. The
libheif calls were checked against the v1.23.5 public C headers. The libvips,
LibRaw, libaom, and lcms2 calls use their documented C APIs but remain compile-
and fixture-unverified in this environment. In particular, the selected AOM
plugin must expose the `threads` and `chroma` parameters; their absence fails
closed as `encode_failed` rather than silently using different settings.
