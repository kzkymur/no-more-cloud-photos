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

The video build requires the still builder's deterministic manifest, verifies
its libaom source fields and concrete shared-object digest before and after the
FFmpeg build, and records the same evidence in its own manifest. FFmpeg uses an
absolute, non-fallback pinned-prefix RPATH. At runtime the helper rehashes that
exact object and verifies the FFmpeg-to-libavcodec-to-libaom ELF dependency chain before it
accepts any command or advertises library identities.

The helper implements four commands: `capabilities`, `inspect`, `transform`,
and `verify-output`. Argument order, descriptor numbers, limits, result fields,
and error codes are closed. The Go adapter grants transform and independent
verification one shared two-hour deadline under a 4 GiB address-space limit,
one codec thread setting, 1 MiB stdout/stderr limits, and bounded output files.

## Source color

A source whose color signalling is complete keeps the strict rule: range
(`tv`/`mpeg` limited, `pc`/`jpeg` full) plus either BT.709 primaries, transfer
and matrix, BT.2020 primaries/matrix with BT.709 transfer, or BT.2020 PQ/HLG
HDR (tone mapped to BT.709 SDR). Anything else is `unsupported_input`.

By customer decision (#21) a source whose signalling is wholly or partly
unspecified (absent, `unknown`, `unspecified` or `reserved`) is accepted with
conventional SDR inference instead of being refused, and only then:

- the stream must be provably 8-bit (an 8-bit `pix_fmt`, `bits_per_raw_sample`
  absent or `8`); untagged 10-bit or deeper video may be HDR and stays refused,
  as does any stream with HDR mastering/content-light side data;
- an HD frame (coded width >= 1280 or coded height > 576) assumes BT.709
  primaries, transfer and matrix; an SD frame (narrower than 1280 and at most
  576 lines) assumes the BT.601 matrix (`smpte170m`, shared by 525- and
  625-line BT.601) with BT.709 primaries and transfer, because the helper does
  not distinguish the 525/625 primaries;
- an unspecified range is limited, except for a JPEG-range `yuvj*` format,
  which is full by definition (a `yuvj*` stream declaring limited range is
  refused); any other declared range is kept;
- every declared primaries/transfer/matrix value must equal the inferred one,
  so a partial PQ, HLG, BT.2020 or mismatching matrix tag is still refused.

The assumption is never silent: the inspection result carries
`color_inferred: true` with the assumed values, the zscale conversion states
them explicitly (`primariesin/transferin/matrixin/rangein`, because untagged
frames give zscale nothing to read), the persisted rendition audit records
`input_color` as `inferred:<primaries>/<transfer>/<matrix>/<range>`, and the
Worker logs `video source color inferred` with the job id. Explicitly tagged
streams are processed exactly as before. The immutable design snapshot
(`docs/design-snapshot/worker-spec.html`) predates this decision; this section
is the current rule.

Inspection performs full selected-video frame decoding through ffprobe and a
full selected-audio null decode when audio exists. Verification independently
reselects source streams, reprobes output, compares every presentation delta
within one output tick, and fully decodes output video/audio in a separate
helper process. Certification, Worker registration, publication, and runtime
activation remain issue #14.
