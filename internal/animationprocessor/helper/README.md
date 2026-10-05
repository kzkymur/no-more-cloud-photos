# Native animation helper

`nmcp-animation-helper` is the project-owned C++20 implementation of the exact
protocol-v1 CLI in `internal/animationprocessor/processor_linux.go`. It invokes
no subprocesses and accepts only the fixed `/proc/self/fd/*` paths passed by the
Go wrapper. Normal builds place the executable in `<build>/bin`:

```text
internal/animationprocessor/helper/build-pinned-codecs.sh /absolute/prefix /tmp/opencode/animation-codecs
PKG_CONFIG_PATH=/absolute/prefix/lib/pkgconfig:/absolute/prefix/lib64/pkgconfig \
  cmake -S internal/animationprocessor/helper -B build/animation-helper
cmake --build build/animation-helper
ctest --test-dir build/animation-helper --output-on-failure
```

libwebp 1.6.0 and giflib 5.2.2 come from this directory's verified build
script. libheif 1.23.5, libaom 3.8.2, lcms2 2.14, and unchanged sRGB2014.icc
come from `internal/stillprocessor/helper/build-pinned-toolchain.sh`. The
transitive system/OCI closure remains the issue #20 boundary.

For dependency-free parser, accounting, geometry, hash, and resize tests, build
with `-DNMCP_BUILD_HELPER=OFF`. `nmcp-animation-fixture-generator` creates a
variable-duration, finite-loop, alpha/partial-frame WebP. `nmcp-webp-reference`
independently reports output frame count, durations, dimensions, and loop count.

## Closed behavior

The helper dispatches solely on the wrapper-provided MIME. GIF is parsed through
giflib's record API rather than `DGifSlurp`: canvas, frame count, duration, axes,
canvas pixels, and cumulative source-rectangle pixels are checked before each
raster is decompressed. It composites transparency and disposal-to-background
or disposal-to-previous onto transparent RGBA. A missing GIF loop extension is
one total play, zero is infinite, and a positive repetition count becomes
`count + 1` total plays.

WebP metadata is checked with libwebp demux before the animation decoder is
advanced. The decoder supplies fully composited RGBA canvases. Actual frame
count one is classified static and rejected by transform while remaining valid
for inspect. Zero-duration frames in both formats become 100 ms and their
indices are reported. Animated WebP output preserves normalized durations and
the total-play count; thumbnail AVIF encodes the first composited frame with the
pinned AOM plugin and one thread.

Embedded WebP RGB ICC is converted to the wrapper-verified sRGB profile through
lcms2; untagged WebP and GIF are explicitly assumed sRGB. Resize uses bilinear
premultiplied RGBA and round-to-nearest aspect geometry, preserving odd and
one-pixel axes without upscaling. Metadata is stripped and only the supplied
sRGB ICC is attached. Encoded memory and descriptor writes are bounded by
`--max-output-bytes`; the outer process runner separately enforces address-space,
file-size, timeout, and log ceilings.
