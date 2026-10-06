# Video helper toolchain

`cmd/nmcp-video-helper` is a closed protocol-v1 executable. It accepts media
only through fixed `/proc/self/fd/*` arguments and discovers only sibling
`ffmpeg` and `ffprobe` executables. It never accepts a codec executable path,
shell fragment, arbitrary filter graph, or fallback encoder from a request.

The pinned toolchain is built into the same absolute prefix as the existing
still-image closure:

```sh
internal/videoprocessor/helper/build-pinned-toolchain.sh \
  /absolute/prefix /absolute/work
go build -o /absolute/prefix/bin/nmcp-video-helper ./cmd/nmcp-video-helper
```

The source build disables FFmpeg network/device/ffplay support and host-library
autodetection. GPL and nonfree codec libraries are not enabled. The only video
encoder used by the MP4 transform is `libsvtav1`; H.264 fallback is forbidden.
The first-frame AVIF uses the already pinned libaom encoder. AAC-LC uses
FFmpeg's native encoder.

The helper implements four commands: `capabilities`, `inspect`, `transform`,
and `verify-output`. Argument order, descriptor numbers, limits, result fields,
and error codes are closed. The Go adapter grants transform and independent
verification one shared two-hour deadline under a 4 GiB address-space limit,
one codec thread setting, 1 MiB stdout/stderr limits, and bounded output files.

Inspection performs full selected-video frame decoding through ffprobe and a
full selected-audio null decode when audio exists. Verification independently
reselects source streams, reprobes output, compares every presentation delta
within one output tick, and fully decodes output video/audio in a separate
helper process. Certification, Worker registration, publication, and runtime
activation remain issue #14.
