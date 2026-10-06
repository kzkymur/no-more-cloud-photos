# Video fixture provenance and evidence envelope

Issue #13 fixtures are generated at CI time from project-owned deterministic
color, orientation, timing, and audio patterns. No unexplained binary media is
stored in this directory. Generation and cross-version verification use the
hash-pinned third-party FFmpeg 6.0.1 build recorded in `fixture-tool.lock`;
its version/build configuration and required codecs are logged by CI. This GPL
build is CI-only and distinct from the production LGPL pinned FFmpeg 9.0.2
closure in `../helper/toolchain.lock`.

The acceptance matrix contains short fixtures for:

- AVC MP4 with AAC, odd source geometry, and visibly different first/later
  frames;
- HEVC MOV without audio and with an ISO-BMFF display matrix;
- HEVC Main10 BT.2020/PQ with mastering-display and MaxCLL metadata;
- HEVC Main10 BT.2020/HLG using the fixed nominal 1000-nit policy;
- default and non-default video/audio streams with an attached picture, proving
  default-disposition then absolute-index selection;
- VFR presentation deltas, non-square SAR, small/no-upscale geometry, and
  supported rates plus 192 kHz PCM deterministically resampled to 48 kHz AAC.

Before processing, pinned ffprobe and full null-decode commands assert the
fixture streams, codecs, timing, rotation, color and audio facts. After
processing, the helper's separate `verify-output` process and the independently
downloaded hash-pinned FFmpeg 6.0.1 CI tool both probe and fully decode the AV1
MP4/AAC and AVIF outputs. Controlled generated media proves the named contract
paths; it does not claim coverage of all camera/vendor files, Dolby Vision,
HDR10+, or real-device performance. Those broader interoperability and
performance claims remain issue #21.
