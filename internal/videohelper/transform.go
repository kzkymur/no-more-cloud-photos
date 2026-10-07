//go:build linux

package videohelper

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
)

func (e *engine) transform(r request, out io.Writer) (returnErr error) {
	output, err := os.OpenFile(r.output, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fail("policy_violation")
	}
	defer output.Close()
	succeeded := false
	defer func() {
		if !succeeded {
			if truncateErr := output.Truncate(0); truncateErr != nil {
				returnErr = fail("resource_limit")
			}
		}
	}()
	source, probe, err := e.inspect(r.input, r.mime, r.expectedVideoStreamIndex, r.limits)
	if err != nil {
		return err
	}
	digest, err := digestFile(r.icc)
	if err != nil {
		return fail("capability_failed")
	}
	even := r.kind == "mp4-av1"
	width, height, err := fitRationalDimensions(probe.displayWidth, probe.displayHeight, r.maxLongEdge, even)
	if err != nil {
		return err
	}
	videoFilter := filterGraph(source, width, height, r.bitDepth, r.kind)
	audioRate, audioFilter := outputAudio(source)
	if r.kind != "mp4-av1" {
		audioRate, audioFilter = 0, "none"
	}
	outputLimit := r.limits.VideoBytes
	if r.kind == "first-frame-avif" {
		outputLimit = r.limits.ThumbBytes
	}
	if remaining := r.limits.GeneratedBytes - r.generatedBytesBefore; remaining < outputLimit {
		outputLimit = remaining
	}
	if outputLimit <= 0 {
		return fail("output_too_large")
	}
	args := []string{"-v", "error", "-xerror", "-nostdin", "-y", "-noautorotate"}
	if probe.rotation != "none" {
		args = append(args, "-display_rotation:"+strconv.Itoa(source.VideoStreamIndex), "0")
	}
	args = append(args, "-threads", "1", "-i", r.input, "-map", "0:"+strconv.Itoa(source.VideoStreamIndex), "-vf", videoFilter, "-map_metadata", "-1", "-map_chapters", "-1", "-threads:v", "1")
	if r.kind == "mp4-av1" {
		args = append(args, "-c:v", "libsvtav1", "-crf", "32", "-preset", "6", "-svtav1-params", "lp=1", "-fps_mode:v", "passthrough", "-enc_time_base:v", source.TimeBase.NumeratorString()+"/"+source.TimeBase.DenominatorString(), "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv")
		if source.AudioPresent {
			args = append(args, "-map", "0:"+strconv.Itoa(source.AudioStreamIndex), "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "128k")
			if audioFilter != "none" {
				args = append(args, "-af", audioFilter)
			}
		} else {
			args = append(args, "-an")
		}
		args = append(args, "-movflags", "+faststart+write_colr", "-metadata:s:v:0", "rotate=", "-fs", strconv.FormatInt(outputLimit, 10), "-f", "mp4", r.output)
	} else {
		crf := 63 - int(math.Round(float64(r.quality)*63/100))
		args = append(args, "-an", "-frames:v", "1", "-c:v", "libaom-av1", "-crf", strconv.Itoa(crf), "-b:v", "0", "-cpu-used", "6", "-still-picture", "1", "-pix_fmt", "yuv420p", "-color_primaries", "bt709", "-color_trc", "iec61966-2-1", "-colorspace", "bt709", "-color_range", "tv", "-fs", strconv.FormatInt(outputLimit, 10), "-f", "avif", r.output)
	}
	if _, _, err := e.run.run(e.ffmpeg, args, 1<<20); err != nil {
		return fail("encode_failed")
	}
	info, err := output.Stat()
	limit := r.limits.VideoBytes
	if r.kind == "first-frame-avif" {
		limit = r.limits.ThumbBytes
	}
	if err != nil || info.Size() <= 0 || info.Size() > limit || r.generatedBytesBefore > r.limits.GeneratedBytes-info.Size() {
		return fail("output_too_large")
	}
	result := struct {
		Kind             string     `json:"kind"`
		OutputMIME       string     `json:"output_mime"`
		Width            int        `json:"width"`
		Height           int        `json:"height"`
		MaxLongEdge      int        `json:"max_long_edge"`
		Threads          int        `json:"threads"`
		CRF              int        `json:"crf"`
		Quality          int        `json:"quality"`
		BitDepth         int        `json:"bit_depth"`
		Chroma           string     `json:"chroma"`
		AudioPresent     bool       `json:"audio_present"`
		AudioCodec       string     `json:"audio_codec"`
		AudioBitrateKbps int        `json:"audio_bitrate_kbps"`
		Source           inspection `json:"source"`
		Audit            audit      `json:"audit"`
	}{
		Kind: r.kind, OutputMIME: map[bool]string{true: "video/mp4", false: "image/avif"}[r.kind == "mp4-av1"], Width: width, Height: height,
		MaxLongEdge: r.maxLongEdge, Threads: 1, CRF: map[bool]int{true: 32, false: 0}[r.kind == "mp4-av1"], Quality: r.quality, BitDepth: r.bitDepth,
		Chroma: "4:2:0", AudioPresent: r.kind == "mp4-av1" && source.AudioPresent,
		AudioCodec: map[bool]string{true: "aac", false: "none"}[r.kind == "mp4-av1" && source.AudioPresent], AudioBitrateKbps: map[bool]int{true: 128, false: 0}[r.kind == "mp4-av1" && source.AudioPresent], Source: source,
		Audit: makeAudit(source, probe, digest, videoFilter, audioFilter, audioRate, r.kind),
	}
	if err := writeEnvelope(out, true, "", result); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func makeAudit(source inspection, probe *mediaProbe, digest, videoFilter, audioFilter string, audioRate int, kind string) audit {
	audioDecoder, audioEncoder, muxer := "none", "none", "avif"
	if source.AudioPresent {
		audioDecoder = source.AudioCodec
	}
	if kind == "mp4-av1" {
		muxer = "mp4"
		if source.AudioPresent {
			audioEncoder = "aac"
		}
	}
	hdrDisposition, toneMap := "sdr-normalized", "not-needed"
	if source.HDR {
		hdrDisposition, toneMap = "tone-mapped", "zscale-linear-tonemap-hable-desat-0-100nit"
	}
	geometry, outputColor, outputTransfer := "display-aspect-square-pixel-thumbnail-no-upscale", "srgb", "iec61966-2-1"
	if kind == "mp4-av1" {
		geometry, outputColor, outputTransfer = "display-aspect-square-pixel-even-floor-no-upscale", "bt709-sdr-100nit", "bt709"
	}
	return audit{
		ToolVersion: helperVersion, BuildManifest: buildManifest, LibraryVersions: versions, ICCSHA256: digest,
		Demuxer: firstFormat(probe.document.Format.FormatName), VideoDecoder: source.VideoCodec, AudioDecoder: audioDecoder,
		VideoEncoder: map[bool]string{true: "libsvtav1", false: "libaom-av1"}[kind == "mp4-av1"], AudioEncoder: audioEncoder, Muxer: muxer,
		SelectedVideoStream: source.VideoStreamIndex, SelectedAudioStream: source.AudioStreamIndex, StreamSelection: "expected-absolute-index",
		RotationSource: probe.rotation, RotationDegreesApplied: source.RotationDegrees, Orientation: "identity", Geometry: geometry,
		Timing: "preserve-presentation-order-vfr-rebase-zero", VideoFilterGraph: videoFilter, AudioFilterGraph: audioFilter,
		InputColor: source.ColorPrimaries + "/" + source.ColorTransfer + "/" + source.ColorMatrix + "/" + source.ColorRange, OutputColor: outputColor,
		OutputPrimaries: "bt709", OutputTransfer: outputTransfer, OutputMatrix: "bt709", OutputRange: "limited", HDRDisposition: hdrDisposition, ToneMap: toneMap,
		TargetNits: 100, SourcePeakNits: source.SourcePeakNits, InputAudioLayout: source.AudioChannelLayout,
		OutputAudioLayout: map[bool]string{true: source.AudioChannelLayout, false: "none"}[source.AudioPresent && kind == "mp4-av1"], InputAudioSampleRate: source.AudioSampleRate,
		OutputAudioSampleRate: map[bool]int{true: audioRate, false: 0}[source.AudioPresent && kind == "mp4-av1"], Metadata: "strip-after-normalization-keep-color-tags",
	}
}

func filterGraph(source inspection, width, height, bitDepth int, kind string) string {
	parts := []string{}
	switch source.RotationDegrees {
	case 90:
		parts = append(parts, "transpose=clock")
	case 180:
		parts = append(parts, "hflip", "vflip")
	case 270:
		parts = append(parts, "transpose=cclock")
	}
	outputTransfer := "bt709"
	if kind == "first-frame-avif" {
		outputTransfer = "iec61966-2-1"
	}
	if source.HDR {
		parts = append(parts,
			"zscale=transfer=linear:npl="+strconv.Itoa(source.SourcePeakNits),
			"format=gbrpf32le", "tonemap=hable:desat=0:peak="+strconv.Itoa(source.SourcePeakNits),
			"zscale=primaries=bt709:transfer="+outputTransfer+":matrix=bt709:range=limited:npl=100")
	} else {
		parts = append(parts, "zscale=primaries=bt709:transfer="+outputTransfer+":matrix=bt709:range=limited")
	}
	pixelFormat := "format=yuv420p"
	if bitDepth == 10 {
		pixelFormat = "format=yuv420p10le"
	}
	parts = append(parts, "scale="+strconv.Itoa(width)+":"+strconv.Itoa(height)+":flags=lanczos", "setsar=1", pixelFormat, "setpts=PTS-STARTPTS")
	return strings.Join(parts, ",")
}

func outputAudio(source inspection) (int, string) {
	if !source.AudioPresent {
		return 0, "none"
	}
	supported := map[int]bool{7350: true, 8000: true, 11025: true, 12000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true, 64000: true, 88200: true, 96000: true}
	if supported[source.AudioSampleRate] {
		return source.AudioSampleRate, "asetpts=PTS-STARTPTS"
	}
	return 48000, "aresample=48000,asetpts=PTS-STARTPTS"
}

func fitDimensions(width, height, edge int, even bool) (int, int, error) {
	if width <= 0 || height <= 0 || edge <= 0 || width > maxDimension || height > maxDimension {
		return 0, 0, fail("unsupported_input")
	}
	return fitRationalDimensions(new(big.Rat).SetInt64(int64(width)), new(big.Rat).SetInt64(int64(height)), edge, even)
}

func fitRationalDimensions(width, height *big.Rat, edge int, even bool) (int, int, error) {
	if width == nil || height == nil || width.Sign() <= 0 || height.Sign() <= 0 || edge <= 0 {
		return 0, 0, fail("unsupported_input")
	}
	long := new(big.Rat).Set(width)
	if height.Cmp(long) > 0 {
		long.Set(height)
	}
	scale := new(big.Rat).SetInt64(1)
	if long.Cmp(new(big.Rat).SetInt64(int64(edge))) > 0 {
		scale.Quo(new(big.Rat).SetInt64(int64(edge)), long)
	}
	outWidth, errWidth := floorRationalAxis(new(big.Rat).Mul(width, scale))
	outHeight, errHeight := floorRationalAxis(new(big.Rat).Mul(height, scale))
	if errWidth != nil || errHeight != nil {
		return 0, 0, fail("unsupported_input")
	}
	if even {
		outWidth -= outWidth % 2
		outHeight -= outHeight % 2
		if outWidth < 2 || outHeight < 2 {
			return 0, 0, fail("unsupported_input")
		}
	} else if outWidth < 1 || outHeight < 1 {
		return 0, 0, fail("unsupported_input")
	}
	return outWidth, outHeight, nil
}

func (r rational) NumeratorString() string   { return strconv.FormatInt(r.Numerator, 10) }
func (r rational) DenominatorString() string { return strconv.FormatInt(r.Denominator, 10) }

type outputProbe struct {
	document      probeDocument
	video         probeStream
	audio         *probeStream
	pts           []int64
	timeBase      rational
	durationTicks int64
}

func (e *engine) probeOutput(path, kind string, limit limits) (*outputProbe, error) {
	stdout, _, err := e.run.run(e.ffprobe, []string{"-v", "error", "-threads", "1", "-show_format", "-show_streams", "-of", "json", path}, 1<<20)
	if err != nil {
		return nil, fail("decode_failed")
	}
	var document probeDocument
	if json.Unmarshal(stdout, &document) != nil || len(document.Streams) == 0 || len(document.Streams) > limit.Streams {
		return nil, fail("decode_failed")
	}
	videoIndex, ok := selectStream(document.Streams, "video")
	if !ok {
		return nil, fail("decode_failed")
	}
	video, _ := streamByIndex(document.Streams, videoIndex)
	timeBase, err := parseRational(video.TimeBase)
	if err != nil {
		return nil, fail("decode_failed")
	}
	frameLimit := limit
	if kind == "first-frame-avif" {
		frameLimit.FPSNum, frameLimit.FPSDen = math.MaxInt64, 1
	}
	facts, err := e.frameFacts(path, videoIndex, video.Width, video.Height, timeBase, video.DurationTicks, frameLimit)
	if err != nil {
		return nil, err
	}
	result := &outputProbe{document: document, video: video, pts: facts.pts, timeBase: timeBase, durationTicks: facts.durationTicks}
	if index, found := selectStream(document.Streams, "audio"); found {
		audio, _ := streamByIndex(document.Streams, index)
		result.audio = &audio
	}
	return result, nil
}

func (e *engine) verify(r request, out io.Writer) error {
	source, sourceProbe, err := e.inspect(r.source, r.mime, r.expectedVideoStreamIndex, r.limits)
	if err != nil {
		return err
	}
	output, err := e.probeOutput(r.output, r.kind, r.limits)
	if err != nil {
		return err
	}
	if err := validateOutputProbe(output, r.kind); err != nil {
		return err
	}
	if r.kind == "mp4-av1" {
		if (output.audio != nil) != source.AudioPresent {
			return fail("decode_failed")
		}
		if output.audio != nil {
			rate, rateErr := decimalInt(output.audio.SampleRate)
			expectedRate, _ := outputAudio(source)
			if rateErr != nil || rate != expectedRate || output.audio.ChannelLayout != source.AudioChannelLayout || output.audio.Channels != source.AudioChannels {
				return fail("decode_failed")
			}
		}
	}
	decodeArgs := []string{"-v", "error", "-xerror", "-nostdin", "-noautorotate", "-threads", "1", "-i", r.output, "-map", "0:" + strconv.Itoa(output.video.Index), "-threads:v", "1"}
	if output.audio != nil {
		decodeArgs = append(decodeArgs, "-map", "0:"+strconv.Itoa(output.audio.Index), "-threads:a", "1")
	}
	decodeArgs = append(decodeArgs, "-f", "null", "-")
	if _, _, err := e.run.run(e.ffmpeg, decodeArgs, 1<<20); err != nil {
		return fail("decode_failed")
	}
	maxError := 0
	maxDurationError := 0
	if r.kind == "mp4-av1" {
		if output.pts[0] != 0 || len(output.pts) != len(sourceProbe.pts) {
			return fail("decode_failed")
		}
		maxError, err = timingErrorTicks(sourceProbe.pts, source.TimeBase, output.pts, output.timeBase)
		if err != nil || maxError > 1 {
			return fail("decode_failed")
		}
		maxDurationError, err = durationErrorTicks(sourceProbe.durationTicks, source.TimeBase, output.durationTicks, output.timeBase)
		if err != nil || maxDurationError > 1 {
			return fail("decode_failed")
		}
	} else if len(output.pts) != 1 {
		return fail("decode_failed")
	}
	rotation, _, err := streamRotation(output.video)
	if err != nil || rotation != 0 {
		return fail("decode_failed")
	}
	result := struct {
		Kind                  string     `json:"kind"`
		Container             string     `json:"container"`
		Width                 int        `json:"width"`
		Height                int        `json:"height"`
		SampleAspectRatio     rational   `json:"sample_aspect_ratio"`
		VideoCodec            string     `json:"video_codec"`
		BitDepth              int        `json:"bit_depth"`
		Chroma                string     `json:"chroma"`
		FrameCount            int        `json:"frame_count"`
		PTSDeltaSHA256        string     `json:"pts_delta_sha256"`
		MaxTimingErrorTicks   int        `json:"max_timing_error_ticks"`
		OutputDurationUS      int64      `json:"output_duration_us"`
		MaxDurationErrorTicks int        `json:"max_duration_error_ticks"`
		RotationDegrees       int        `json:"rotation_degrees"`
		HasDisplayMatrix      bool       `json:"has_display_matrix"`
		HasRotateMetadata     bool       `json:"has_rotate_metadata"`
		ColorPrimaries        string     `json:"color_primaries"`
		ColorTransfer         string     `json:"color_transfer"`
		ColorMatrix           string     `json:"color_matrix"`
		ColorRange            string     `json:"color_range"`
		AudioPresent          bool       `json:"audio_present"`
		AudioCodec            string     `json:"audio_codec"`
		AudioProfile          string     `json:"audio_profile"`
		FullyDecodedVideo     bool       `json:"fully_decoded_video"`
		FullyDecodedAudio     bool       `json:"fully_decoded_audio"`
		Source                inspection `json:"source"`
	}{
		Kind: r.kind, Container: map[bool]string{true: "mp4", false: "avif"}[r.kind == "mp4-av1"], Width: output.video.Width, Height: output.video.Height,
		SampleAspectRatio: mustRationalDefault(output.video.SampleAspect, rational{1, 1}), VideoCodec: output.video.CodecName, BitDepth: pixelBitDepth(output.video), Chroma: pixelChroma(output.video.PixFmt),
		FrameCount: len(output.pts), PTSDeltaSHA256: hashDeltas(output.pts), MaxTimingErrorTicks: maxError,
		OutputDurationUS: durationMicrosUnchecked(output.durationTicks, output.timeBase), MaxDurationErrorTicks: maxDurationError, RotationDegrees: rotation,
		HasDisplayMatrix: hasDisplayMatrix(output.video), HasRotateMetadata: output.video.Tags["rotate"] != "",
		ColorPrimaries: output.video.ColorPrimaries, ColorTransfer: output.video.ColorTransfer, ColorMatrix: output.video.ColorSpace, ColorRange: normalizeRange(output.video.ColorRange),
		AudioPresent: output.audio != nil, AudioCodec: "none", AudioProfile: "none", FullyDecodedVideo: true, FullyDecodedAudio: output.audio != nil, Source: source,
	}
	if output.audio != nil {
		result.AudioCodec, result.AudioProfile = output.audio.CodecName, normalizeAACProfile(output.audio.Profile)
	}
	return writeEnvelope(out, true, "", result)
}

func validateOutputProbe(output *outputProbe, kind string) error {
	format := output.document.Format.FormatName
	wantStreams := 1
	if output.audio != nil {
		wantStreams = 2
	}
	if len(output.document.Streams) != wantStreams {
		return fail("decode_failed")
	}
	expectedTransfer := "bt709"
	if kind == "first-frame-avif" {
		expectedTransfer = "iec61966-2-1"
	}
	if output.video.CodecName != "av1" || output.video.Width <= 0 || output.video.Height <= 0 || output.video.ColorPrimaries != "bt709" || output.video.ColorTransfer != expectedTransfer || output.video.ColorSpace != "bt709" || normalizeRange(output.video.ColorRange) != "limited" {
		return fail("decode_failed")
	}
	if kind == "mp4-av1" {
		if !strings.Contains(format, "mp4") && !strings.Contains(format, "mov") || output.video.Width%2 != 0 || output.video.Height%2 != 0 || pixelBitDepth(output.video) != 10 || pixelChroma(output.video.PixFmt) != "4:2:0" {
			return fail("decode_failed")
		}
		if output.audio != nil && (output.audio.CodecName != "aac" || normalizeAACProfile(output.audio.Profile) != "LC") {
			return fail("decode_failed")
		}
	} else if output.audio != nil || len(output.pts) != 1 {
		return fail("decode_failed")
	}
	return nil
}

func timingErrorTicks(source []int64, sourceBase rational, output []int64, outputBase rational) (int, error) {
	maximum := int64(0)
	for i := 1; i < len(source); i++ {
		sourceDelta := new(big.Rat).SetFrac(big.NewInt(source[i]-source[i-1]), big.NewInt(1))
		sourceDelta.Mul(sourceDelta, new(big.Rat).SetFrac(big.NewInt(sourceBase.Numerator), big.NewInt(sourceBase.Denominator)))
		outputDelta := new(big.Rat).SetFrac(big.NewInt(output[i]-output[i-1]), big.NewInt(1))
		outputDelta.Mul(outputDelta, new(big.Rat).SetFrac(big.NewInt(outputBase.Numerator), big.NewInt(outputBase.Denominator)))
		difference := new(big.Rat).Sub(sourceDelta, outputDelta)
		if difference.Sign() < 0 {
			difference.Neg(difference)
		}
		ticks := new(big.Rat).Quo(difference, new(big.Rat).SetFrac(big.NewInt(outputBase.Numerator), big.NewInt(outputBase.Denominator)))
		ceiling := new(big.Int).Quo(ticks.Num(), ticks.Denom())
		if new(big.Int).Mod(ticks.Num(), ticks.Denom()).Sign() != 0 {
			ceiling.Add(ceiling, big.NewInt(1))
		}
		if !ceiling.IsInt64() {
			return 0, errors.New("timing overflow")
		}
		maximum = max(maximum, ceiling.Int64())
	}
	if maximum > math.MaxInt {
		return 0, errors.New("timing overflow")
	}
	return int(maximum), nil
}

func durationErrorTicks(sourceTicks int64, sourceBase rational, outputTicks int64, outputBase rational) (int, error) {
	if sourceTicks <= 0 || outputTicks <= 0 {
		return 0, errors.New("invalid duration")
	}
	sourceDuration := new(big.Rat).Mul(new(big.Rat).SetInt64(sourceTicks), new(big.Rat).SetFrac(big.NewInt(sourceBase.Numerator), big.NewInt(sourceBase.Denominator)))
	outputDuration := new(big.Rat).Mul(new(big.Rat).SetInt64(outputTicks), new(big.Rat).SetFrac(big.NewInt(outputBase.Numerator), big.NewInt(outputBase.Denominator)))
	difference := new(big.Rat).Sub(sourceDuration, outputDuration)
	if difference.Sign() < 0 {
		difference.Neg(difference)
	}
	ticks := new(big.Rat).Quo(difference, new(big.Rat).SetFrac(big.NewInt(outputBase.Numerator), big.NewInt(outputBase.Denominator)))
	ceiling := new(big.Int).Quo(ticks.Num(), ticks.Denom())
	if new(big.Int).Mod(ticks.Num(), ticks.Denom()).Sign() != 0 {
		ceiling.Add(ceiling, big.NewInt(1))
	}
	if !ceiling.IsInt64() || ceiling.Int64() > math.MaxInt {
		return 0, errors.New("duration overflow")
	}
	return int(ceiling.Int64()), nil
}

func durationMicrosUnchecked(ticks int64, base rational) int64 {
	value := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(base.Numerator))
	value.Mul(value, big.NewInt(1_000_000))
	value.Quo(value, big.NewInt(base.Denominator))
	if !value.IsInt64() {
		return 0
	}
	return value.Int64()
}

func mustRationalDefault(value string, fallback rational) rational {
	r, err := parseRationalDefault(value, fallback)
	if err != nil {
		return rational{}
	}
	return r
}

func pixelBitDepth(stream probeStream) int {
	if n, err := decimalInt(stream.BitsPerRaw); err == nil && n > 0 {
		return n
	}
	if strings.Contains(stream.PixFmt, "10") {
		return 10
	}
	return 8
}

func pixelChroma(format string) string {
	if strings.HasPrefix(format, "yuv420") || strings.HasPrefix(format, "yuva420") {
		return "4:2:0"
	}
	if strings.HasPrefix(format, "yuv444") || strings.HasPrefix(format, "gbr") {
		return "4:4:4"
	}
	return "unknown"
}

func normalizeRange(value string) string {
	return map[string]string{"tv": "limited", "mpeg": "limited", "pc": "full", "jpeg": "full"}[value]
}

func normalizeAACProfile(value string) string {
	if value == "LC" || strings.EqualFold(value, "AAC LC") {
		return "LC"
	}
	return value
}

func hasDisplayMatrix(stream probeStream) bool {
	for _, side := range stream.SideDataList {
		if _, ok := side["rotation"]; ok {
			return true
		}
	}
	return false
}

func firstFormat(value string) string {
	if name, _, ok := strings.Cut(value, ","); ok {
		return name
	}
	if value == "" {
		return "unknown"
	}
	return value
}
