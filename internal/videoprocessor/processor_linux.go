//go:build linux

package videoprocessor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

const BuildManifest = "ffmpeg=9.0.2;svt-av1=4.2.0;zimg=3.0.6;libaom=v3.8.2;threads=1"

type Config struct {
	Helper        string
	Prlimit       string
	SRGBICC       string
	SRGBICCSHA256 string
	Policy        Policy
}

type Processor struct {
	helper, prlimit, srgbICC, iccSHA256 string
	policy                              Policy
	runner, thumbnailRunner             *processrunner.Runner
}

type InspectRequest struct {
	Input    *os.File
	MIMEType string
}

type Request struct {
	Input                *os.File
	Output               *os.File
	MIMEType             string
	Recipe               profile.Recipe
	GeneratedBytesBefore int64
}

type Rational struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

type Capabilities struct {
	ProtocolVersion  int
	HelperVersion    string
	LibraryVersions  map[string]string
	DecoderMIMETypes []string
	OutputKinds      []string
	VideoEncoder     string
	AudioEncoder     string
	VideoMuxer       string
	ToneMap          string
	ICCSHA256        string
	Threads          int
	BuildManifest    string
}

type Inspection struct {
	TotalStreams            int      `json:"total_streams"`
	VideoStreamIndex        int      `json:"video_stream_index"`
	AudioPresent            bool     `json:"audio_present"`
	AudioStreamIndex        int      `json:"audio_stream_index"`
	VideoCodec              string   `json:"video_codec"`
	CodedWidth              int      `json:"coded_width"`
	CodedHeight             int      `json:"coded_height"`
	DisplayWidth            int      `json:"display_width"`
	DisplayHeight           int      `json:"display_height"`
	SampleAspectRatio       Rational `json:"sample_aspect_ratio"`
	RotationDegrees         int      `json:"rotation_degrees"`
	FrameCount              int      `json:"frame_count"`
	DurationUS              int64    `json:"duration_us"`
	EffectiveFPS            Rational `json:"effective_fps"`
	TimeBase                Rational `json:"time_base"`
	FirstPTS                int64    `json:"first_pts"`
	LastPTS                 int64    `json:"last_pts"`
	PTSDeltaSHA256          string   `json:"pts_delta_sha256"`
	ColorPrimaries          string   `json:"color_primaries"`
	ColorTransfer           string   `json:"color_transfer"`
	ColorMatrix             string   `json:"color_matrix"`
	ColorRange              string   `json:"color_range"`
	HDR                     bool     `json:"hdr"`
	SourcePeakNits          int      `json:"source_peak_nits"`
	MasteringMaxNits        int      `json:"mastering_max_nits"`
	MaxCLLNits              int      `json:"max_cll_nits"`
	AudioCodec              string   `json:"audio_codec"`
	AudioChannels           int      `json:"audio_channels"`
	AudioChannelLayout      string   `json:"audio_channel_layout"`
	AudioSampleRate         int      `json:"audio_sample_rate"`
	CumulativeDecodedPixels uint64   `json:"cumulative_decoded_pixels"`
}

type Result struct {
	Kind                        string
	OutputMIME, OutputExtension string
	Width, Height               int
	MaxLongEdge, Threads        int
	CRF, Quality, BitDepth      int
	Chroma                      string
	AudioPresent                bool
	AudioCodec                  string
	AudioBitrateKbps            int
	OutputDurationUS            int64
	Source                      Inspection
	Audit                       Audit
}

type Audit struct {
	ToolVersion, BuildManifest                    string
	LibraryVersions                               map[string]string
	ICCSHA256                                     string
	Demuxer, VideoDecoder, AudioDecoder           string
	VideoEncoder, AudioEncoder, Muxer             string
	SelectedVideoStream, SelectedAudioStream      int
	StreamSelection, RotationSource               string
	RotationDegreesApplied                        int
	Orientation, Geometry, Timing                 string
	VideoFilterGraph, AudioFilterGraph            string
	InputColor, OutputColor                       string
	OutputPrimaries, OutputTransfer, OutputMatrix string
	OutputRange, HDRDisposition, ToneMap          string
	TargetNits, SourcePeakNits                    int
	InputAudioLayout, OutputAudioLayout           string
	InputAudioSampleRate, OutputAudioSampleRate   int
	Metadata                                      string
}

func New(config Config) (*Processor, error) {
	if !cleanAbsolute(config.Helper) || !cleanAbsolute(config.Prlimit) || !cleanAbsolute(config.SRGBICC) || !sha256Pattern.MatchString(config.SRGBICCSHA256) {
		return nil, ErrInvalid
	}
	icc, err := verifiedRegularFile(config.SRGBICC, config.SRGBICCSHA256)
	if err != nil {
		return nil, ErrInvalid
	}
	_ = icc.Close()
	policy, err := normalizePolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	runner, err := processrunner.New(config.Prlimit, processrunner.Limits{
		Timeout: policy.Timeout, AddressSpaceBytes: policy.AddressSpaceBytes,
		FileSizeBytes: uint64(policy.GeneratedOutputMaxBytes), OutputBytesPerStream: policy.LogBytesPerStream,
	})
	if err != nil {
		return nil, ErrInvalid
	}
	thumbnailRunner, err := processrunner.New(config.Prlimit, processrunner.Limits{
		Timeout: policy.Timeout, AddressSpaceBytes: policy.AddressSpaceBytes,
		FileSizeBytes: uint64(policy.ThumbnailOutputMaxBytes), OutputBytesPerStream: policy.LogBytesPerStream,
	})
	if err != nil {
		return nil, ErrInvalid
	}
	return &Processor{helper: config.Helper, prlimit: config.Prlimit, srgbICC: config.SRGBICC, iccSHA256: config.SRGBICCSHA256, policy: policy, runner: runner, thumbnailRunner: thumbnailRunner}, nil
}

func (p *Processor) Capabilities(ctx context.Context) (Capabilities, error) {
	if p == nil || p.runner == nil || ctx == nil {
		return Capabilities{}, ErrInvalid
	}
	icc, err := verifiedRegularFile(p.srgbICC, p.iccSHA256)
	if err != nil {
		return Capabilities{}, ErrCapability
	}
	defer icc.Close()
	out, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: []string{
		"capabilities", "--protocol", "1", "--threads", "1", "--srgb-icc", "/proc/self/fd/3",
	}, Files: []*os.File{icc}})
	if err != nil {
		return Capabilities{}, mapRunError(err)
	}
	var response capabilityResponse
	if decodeProtocolJSON(out.Stdout, &response) != nil || !validEnvelope(response.Protocol, response.OK, response.ErrorCode, response.Result != nil) {
		return Capabilities{}, ErrCapability
	}
	if !response.OK {
		return Capabilities{}, mapCapabilityError(response.ErrorCode)
	}
	if validateCapability(*response.Result, p.iccSHA256) != nil {
		return Capabilities{}, ErrCapability
	}
	r := response.Result
	return Capabilities{response.Protocol, r.HelperVersion, cloneMap(r.LibraryVersions), slices.Clone(r.DecoderMIMETypes), slices.Clone(r.OutputKinds), r.VideoEncoder, r.AudioEncoder, r.VideoMuxer, r.ToneMap, r.ICCSHA256, r.Threads, r.BuildManifest}, nil
}

func (p *Processor) Inspect(ctx context.Context, request InspectRequest) (Inspection, error) {
	if p == nil || p.runner == nil || ctx == nil || validateInput(request.Input, request.MIMEType) != nil {
		return Inspection{}, ErrInvalid
	}
	input, err := reopenInput(request.Input)
	if err != nil {
		return Inspection{}, ErrInvalid
	}
	defer input.Close()
	arguments := append([]string{"inspect", "--protocol", "1", "--input", "/proc/self/fd/3", "--input-mime", request.MIMEType}, p.limitArguments()...)
	out, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{input}})
	if err != nil {
		return Inspection{}, mapRunError(err)
	}
	var response inspectionResponse
	if decodeProtocolJSON(out.Stdout, &response) != nil || !validEnvelope(response.Protocol, response.OK, response.ErrorCode, response.Result != nil) {
		return Inspection{}, ErrProcess
	}
	if !response.OK {
		return Inspection{}, mapInspectError(response.ErrorCode)
	}
	if validateInspection(*response.Result, p.policy) != nil {
		return Inspection{}, ErrProcess
	}
	return *response.Result, nil
}

func (p *Processor) Transform(ctx context.Context, request Request) (result Result, returnErr error) {
	if p == nil || p.runner == nil || ctx == nil || validateRequest(request) != nil {
		return Result{}, ErrInvalid
	}
	defer func() {
		if returnErr == nil {
			return
		}
		if request.Output.Truncate(0) != nil {
			returnErr = ErrResourcePolicy
			return
		}
		if _, err := request.Output.Seek(0, io.SeekStart); err != nil {
			returnErr = ErrResourcePolicy
		}
	}()
	operation, cancel := context.WithTimeoutCause(ctx, p.policy.Timeout, errOperationTimeout)
	defer cancel()
	if operation.Err() != nil {
		return Result{}, mapOperationError(operation.Err(), ctx, operation)
	}
	input, err := reopenInput(request.Input)
	if err != nil {
		return Result{}, ErrInvalid
	}
	defer input.Close()
	icc, err := verifiedRegularFile(p.srgbICC, p.iccSHA256)
	if err != nil {
		return Result{}, ErrCapability
	}
	defer icc.Close()
	kind, quality, bitDepth := recipeOutput(request.Recipe)
	arguments := []string{"transform", "--protocol", "1", "--input", "/proc/self/fd/3", "--output", "/proc/self/fd/4",
		"--input-mime", request.MIMEType, "--output-kind", kind, "--max-long-edge", strconv.Itoa(request.Recipe.MaxLongEdge),
		"--quality", strconv.Itoa(quality), "--bit-depth", strconv.Itoa(bitDepth), "--threads", "1", "--srgb-icc", "/proc/self/fd/5",
		"--generated-bytes-before", strconv.FormatInt(request.GeneratedBytesBefore, 10)}
	arguments = append(arguments, p.limitArguments()...)
	operationRunner := p.runner
	if kind == "first-frame-avif" {
		operationRunner = p.thumbnailRunner
	}
	if request.GeneratedBytesBefore > p.policy.GeneratedOutputMaxBytes {
		return Result{}, ErrInvalid
	}
	remaining := p.policy.GeneratedOutputMaxBytes - request.GeneratedBytesBefore
	if remaining == 0 {
		return Result{}, ErrResourcePolicy
	}
	if kindLimit := limitForKind(p.policy, kind); remaining < kindLimit {
		operationRunner, err = processrunner.New(p.prlimit, processrunner.Limits{Timeout: p.policy.Timeout, AddressSpaceBytes: p.policy.AddressSpaceBytes, FileSizeBytes: uint64(remaining), OutputBytesPerStream: p.policy.LogBytesPerStream})
		if err != nil {
			return Result{}, ErrInvalid
		}
	}
	out, err := operationRunner.Run(operation, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{input, request.Output, icc}})
	if err != nil {
		return Result{}, mapOperationError(err, ctx, operation)
	}
	var response transformResponse
	if decodeProtocolJSON(out.Stdout, &response) != nil || !validEnvelope(response.Protocol, response.OK, response.ErrorCode, response.Result != nil) {
		return Result{}, ErrProcess
	}
	if !response.OK {
		return Result{}, mapTransformError(response.ErrorCode)
	}
	if validateTransform(*response.Result, request, p.policy, p.iccSHA256) != nil {
		return Result{}, ErrProcess
	}
	info, err := request.Output.Stat()
	limit := p.policy.VideoOutputMaxBytes
	if kind == "first-frame-avif" {
		limit = p.policy.ThumbnailOutputMaxBytes
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit || request.GeneratedBytesBefore > p.policy.GeneratedOutputMaxBytes-info.Size() {
		return Result{}, ErrResourcePolicy
	}
	if !validateVideoContainer(request.Output, info.Size(), kind, response.Result.AudioPresent) {
		return Result{}, ErrProcess
	}
	if _, err := request.Output.Seek(0, io.SeekStart); err != nil {
		return Result{}, ErrResourcePolicy
	}
	outputDurationUS, err := p.verifyOutput(operation, operationRunner, request, response.Result)
	if err != nil {
		return Result{}, mapOperationError(err, ctx, operation)
	}
	r := response.Result
	extension := "mp4"
	if r.Kind == "first-frame-avif" {
		extension = "avif"
	}
	return Result{Kind: r.Kind, OutputMIME: r.OutputMIME, OutputExtension: extension, Width: r.Width, Height: r.Height,
		MaxLongEdge: r.MaxLongEdge, Threads: r.Threads, CRF: r.CRF, Quality: r.Quality, BitDepth: r.BitDepth,
		Chroma: r.Chroma, AudioPresent: r.AudioPresent, AudioCodec: r.AudioCodec, AudioBitrateKbps: r.AudioBitrateKbps,
		OutputDurationUS: outputDurationUS, Source: r.Source, Audit: publicAudit(r.Audit)}, nil
}

func limitForKind(policy Policy, kind string) int64 {
	if kind == "first-frame-avif" {
		return policy.ThumbnailOutputMaxBytes
	}
	return policy.VideoOutputMaxBytes
}

func (p *Processor) verifyOutput(ctx context.Context, runner *processrunner.Runner, request Request, transformed *transformWire) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	source, err := reopenInput(request.Input)
	if err != nil {
		return 0, ErrProcess
	}
	defer source.Close()
	output, err := reopenInput(request.Output)
	if err != nil {
		return 0, ErrProcess
	}
	defer output.Close()
	arguments := append([]string{"verify-output", "--protocol", "1", "--source", "/proc/self/fd/3", "--output", "/proc/self/fd/4",
		"--source-mime", request.MIMEType, "--output-kind", transformed.Kind}, p.limitArguments()...)
	result, err := runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{source, output}})
	if err != nil {
		return 0, err
	}
	var response verificationResponse
	if decodeProtocolJSON(result.Stdout, &response) != nil || !validEnvelope(response.Protocol, response.OK, response.ErrorCode, response.Result != nil) || !response.OK {
		return 0, ErrProcess
	}
	got := response.Result
	if !got.FullyDecodedVideo || got.Kind != transformed.Kind || got.Width != transformed.Width || got.Height != transformed.Height || got.VideoCodec != "av1" || got.RotationDegrees != 0 || got.HasDisplayMatrix || got.HasRotateMetadata || got.Source != transformed.Source {
		return 0, ErrProcess
	}
	if transformed.Kind == "mp4-av1" {
		if got.Container != "mp4" || got.SampleAspectRatio != (Rational{1, 1}) || got.BitDepth != 10 || got.Chroma != "4:2:0" || got.FrameCount != transformed.Source.FrameCount || !sha256Pattern.MatchString(got.PTSDeltaSHA256) || got.MaxTimingErrorTicks < 0 || got.MaxTimingErrorTicks > 1 || got.OutputDurationUS <= 0 || got.MaxDurationErrorTicks < 0 || got.MaxDurationErrorTicks > 1 || got.AudioPresent != transformed.AudioPresent || got.AudioPresent && (!got.FullyDecodedAudio || got.AudioCodec != "aac" || got.AudioProfile != "LC") || got.ColorPrimaries != "bt709" || got.ColorTransfer != "bt709" || got.ColorMatrix != "bt709" || got.ColorRange != "limited" {
			return 0, ErrProcess
		}
	} else if got.Container != "avif" || got.FrameCount != 1 || got.AudioPresent || got.FullyDecodedAudio || got.ColorPrimaries != "bt709" || got.ColorTransfer != "iec61966-2-1" || got.ColorMatrix != "bt709" || got.ColorRange != "limited" {
		return 0, ErrProcess
	}
	if _, err = request.Output.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return got.OutputDurationUS, nil
}

func (p *Processor) limitArguments() []string {
	q := p.policy
	return []string{"--max-streams", strconv.Itoa(q.Streams), "--max-dimension", strconv.Itoa(q.Dimension),
		"--max-pixels-per-frame", strconv.FormatUint(q.PixelsPerFrame, 10), "--max-duration-us", strconv.FormatInt(q.Duration.Microseconds(), 10),
		"--max-fps-numerator", strconv.FormatInt(q.EffectiveFPSNumerator, 10), "--max-fps-denominator", strconv.FormatInt(q.EffectiveFPSDenominator, 10),
		"--max-frames", strconv.Itoa(q.Frames), "--max-decoded-pixels", strconv.FormatUint(q.CumulativeDecodedPixels, 10),
		"--max-audio-channels", strconv.Itoa(q.AudioChannels), "--max-audio-sample-rate", strconv.Itoa(q.AudioSampleRate),
		"--max-video-output-bytes", strconv.FormatInt(q.VideoOutputMaxBytes, 10), "--max-thumbnail-output-bytes", strconv.FormatInt(q.ThumbnailOutputMaxBytes, 10),
		"--max-generated-output-bytes", strconv.FormatInt(q.GeneratedOutputMaxBytes, 10)}
}

func validateInput(file *os.File, mime string) error {
	if file == nil || !oneOf(mime, "video/mp4", "video/quicktime") {
		return ErrInvalid
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ErrInvalid
	}
	return nil
}

func validateRequest(request Request) error {
	if validateInput(request.Input, request.MIMEType) != nil || request.Output == nil || request.GeneratedBytesBefore < 0 || request.GeneratedBytesBefore > MaxGeneratedOutputBytes || validateRecipe(request.Recipe, request.MIMEType) != nil {
		return ErrInvalid
	}
	inputInfo, err := request.Input.Stat()
	if err != nil {
		return ErrInvalid
	}
	outputInfo, err := request.Output.Stat()
	if err != nil || !outputInfo.Mode().IsRegular() || outputInfo.Size() != 0 || os.SameFile(inputInfo, outputInfo) {
		return ErrInvalid
	}
	offset, err := request.Output.Seek(0, io.SeekCurrent)
	if err != nil || offset != 0 {
		return ErrInvalid
	}
	return nil
}

func validateRecipe(r profile.Recipe, mime string) error {
	if !oneOf(mime, "video/mp4", "video/quicktime") || r.SourceMode != profile.SourceVideo || r.MaxLongEdge <= 0 || r.AllowUpscale || r.Crop != "none" || r.DimensionRule != "preserve-aspect-no-crop-no-upscale-even-round-down" || r.Orientation != "apply" || r.Metadata != "strip-after-normalization-keep-color-tags" || r.Alpha != "preserve" || r.StreamSelection != "primary-video" || r.AnimationOutput != nil {
		return ErrInvalid
	}
	if r.FramePolicy == profile.FrameAll {
		v := r.VideoOutput
		if r.MaxLongEdge > OutputVideoMaxLongEdge || r.Audio != "aac-if-present" || r.Color != "normalize-bt709-tone-map-hdr" || r.AnimationTiming != "not-applicable" || r.AnimationLoop != "not-applicable" || r.StillOutput != nil || v == nil || v.Container != "mp4" || v.VideoCodec != "av1" || v.CRF != OutputAV1CRF || v.BitDepth != OutputAV1BitDepth || v.Chroma != "4:2:0" || v.AudioCodec != "aac" || v.AudioBitrateKbps != OutputAACBitrateKbps {
			return ErrInvalid
		}
		return nil
	}
	s := r.StillOutput
	if r.FramePolicy != profile.FrameFirst || r.MaxLongEdge > OutputThumbnailMaxLongEdge || r.Audio != "discard" || r.Color != "normalize-srgb-tone-map-hdr" || r.AnimationTiming != "first-frame" || r.AnimationLoop != "discard" || r.VideoOutput != nil || s == nil || s.Format != "avif" || s.Quality <= 0 || s.Quality > 100 || s.BitDepth != 8 {
		return ErrInvalid
	}
	return nil
}

func recipeOutput(r profile.Recipe) (string, int, int) {
	if r.VideoOutput != nil {
		return "mp4-av1", 0, r.VideoOutput.BitDepth
	}
	return "first-frame-avif", r.StillOutput.Quality, r.StillOutput.BitDepth
}

func validateInspection(i Inspection, p Policy) error {
	if i.TotalStreams <= 0 || i.TotalStreams > p.Streams || i.VideoStreamIndex < 0 || !safeString(i.VideoCodec) ||
		i.CodedWidth <= 0 || i.CodedHeight <= 0 || i.DisplayWidth <= 0 || i.DisplayHeight <= 0 || i.CodedWidth > p.Dimension || i.CodedHeight > p.Dimension || i.DisplayWidth > p.Dimension || i.DisplayHeight > p.Dimension ||
		!checkedProductWithin(uint64(i.CodedWidth), uint64(i.CodedHeight), p.PixelsPerFrame) || !validPositiveRational(i.SampleAspectRatio) || !oneOfInt(i.RotationDegrees, 0, 90, 180, 270) || i.FrameCount <= 0 || i.FrameCount > p.Frames || i.DurationUS <= 0 || i.DurationUS > p.Duration.Microseconds() || !validPositiveRational(i.EffectiveFPS) || rationalGreater(i.EffectiveFPS.Numerator, i.EffectiveFPS.Denominator, p.EffectiveFPSNumerator, p.EffectiveFPSDenominator) || !validPositiveRational(i.TimeBase) || i.FirstPTS > i.LastPTS || !sha256Pattern.MatchString(i.PTSDeltaSHA256) || !safeColor(i) || i.CumulativeDecodedPixels == 0 || i.CumulativeDecodedPixels > p.CumulativeDecodedPixels {
		return ErrProcess
	}
	if i.HDR != oneOf(i.ColorTransfer, "smpte2084", "arib-std-b67") || i.HDR && (i.ColorPrimaries != "bt2020" || !oneOf(i.ColorMatrix, "bt2020nc", "bt2020c") || i.SourcePeakNits <= 0) || i.ColorTransfer == "smpte2084" && (i.MasteringMaxNits <= 0 || i.MaxCLLNits <= 0 || i.MaxCLLNits > i.MasteringMaxNits || i.SourcePeakNits != i.MasteringMaxNits) || i.ColorTransfer == "arib-std-b67" && (i.SourcePeakNits != 1000 || i.MasteringMaxNits != 0 || i.MaxCLLNits != 0) || !i.HDR && (i.SourcePeakNits != 0 || i.MasteringMaxNits != 0 || i.MaxCLLNits != 0) {
		return ErrProcess
	}
	if i.AudioPresent {
		if i.AudioStreamIndex < 0 || !safeString(i.AudioCodec) || i.AudioChannels <= 0 || i.AudioChannels > p.AudioChannels || !safeString(i.AudioChannelLayout) || i.AudioChannelLayout == "unknown" || i.AudioSampleRate <= 0 || i.AudioSampleRate > p.AudioSampleRate {
			return ErrProcess
		}
	} else if i.AudioStreamIndex != -1 || i.AudioCodec != "none" || i.AudioChannels != 0 || i.AudioChannelLayout != "none" || i.AudioSampleRate != 0 {
		return ErrProcess
	}
	return nil
}

func safeColor(i Inspection) bool {
	return oneOf(i.ColorPrimaries, "bt709", "bt2020") && oneOf(i.ColorTransfer, "bt709", "smpte2084", "arib-std-b67") && oneOf(i.ColorMatrix, "bt709", "bt2020nc", "bt2020c") && oneOf(i.ColorRange, "limited", "full")
}

func validateTransform(r transformWire, request Request, p Policy, digest string) error {
	if validateInspection(r.Source, p) != nil {
		return ErrProcess
	}
	kind, quality, depth := recipeOutput(request.Recipe)
	mime := "video/mp4"
	if kind == "first-frame-avif" {
		mime = "image/avif"
	}
	if r.Kind != kind || r.OutputMIME != mime || r.Width <= 0 || r.Height <= 0 || r.Width > request.Recipe.MaxLongEdge || r.Height > request.Recipe.MaxLongEdge || r.MaxLongEdge != request.Recipe.MaxLongEdge || r.Threads != 1 || r.Quality != quality || r.BitDepth != depth {
		return ErrProcess
	}
	expectedWidth, expectedHeight, err := fitDimensions(r.Source.DisplayWidth, r.Source.DisplayHeight, request.Recipe.MaxLongEdge, kind == "mp4-av1")
	if err != nil || r.Width != expectedWidth || r.Height != expectedHeight {
		return ErrProcess
	}
	if kind == "mp4-av1" && (r.Width%2 != 0 || r.Height%2 != 0 || r.CRF != 32 || r.Chroma != "4:2:0" || r.AudioPresent != r.Source.AudioPresent || r.AudioCodec != map[bool]string{true: "aac", false: "none"}[r.Source.AudioPresent] || r.AudioBitrateKbps != map[bool]int{true: 128, false: 0}[r.Source.AudioPresent]) {
		return ErrProcess
	}
	a := r.Audit
	geometry := "display-aspect-square-pixel-thumbnail-no-upscale"
	videoEncoder, muxer, outputColor, outputTransfer := "libaom-av1", "avif", "srgb", "iec61966-2-1"
	if kind == "mp4-av1" {
		geometry, videoEncoder, muxer, outputColor, outputTransfer = "display-aspect-square-pixel-even-floor-no-upscale", "libsvtav1", "mp4", "bt709-sdr-100nit", "bt709"
	}
	expectedAudioRate, expectedAudioFilter := auditAudio(r.Source, kind)
	expectedAudioDecoder, expectedAudioEncoder := "none", "none"
	if r.Source.AudioPresent {
		expectedAudioDecoder = r.Source.AudioCodec
	}
	if kind == "mp4-av1" && r.Source.AudioPresent {
		expectedAudioEncoder = "aac"
	}
	expectedOutputLayout := "none"
	if kind == "mp4-av1" && r.Source.AudioPresent {
		expectedOutputLayout = r.Source.AudioChannelLayout
	}
	if a.ToolVersion != "nmcp-video-helper/1" || a.BuildManifest != BuildManifest || validateVersionMap(a.LibraryVersions) != nil || a.ICCSHA256 != digest || a.Demuxer != "mov" || a.VideoDecoder != r.Source.VideoCodec || a.AudioDecoder != expectedAudioDecoder || a.SelectedVideoStream != r.Source.VideoStreamIndex || a.SelectedAudioStream != r.Source.AudioStreamIndex || a.StreamSelection != "default-first-then-index" || !oneOf(a.RotationSource, "none", "display-matrix", "rotate-tag", "display-matrix+rotate") || a.RotationDegreesApplied != r.Source.RotationDegrees || a.Orientation != "identity" || a.Geometry != geometry || a.Timing != "preserve-presentation-order-vfr-rebase-zero" || a.VideoFilterGraph != auditVideoFilter(r.Source, r.Width, r.Height, r.BitDepth, kind) || a.AudioFilterGraph != expectedAudioFilter || a.VideoEncoder != videoEncoder || a.AudioEncoder != expectedAudioEncoder || a.Muxer != muxer || a.InputColor != auditInputColor(r.Source) || a.OutputColor != outputColor || a.OutputPrimaries != "bt709" || a.OutputTransfer != outputTransfer || a.OutputMatrix != "bt709" || a.OutputRange != "limited" || a.InputAudioLayout != r.Source.AudioChannelLayout || a.OutputAudioLayout != expectedOutputLayout || a.InputAudioSampleRate != r.Source.AudioSampleRate || a.OutputAudioSampleRate != expectedAudioRate || a.Metadata != "strip-after-normalization-keep-color-tags" {
		return ErrProcess
	}
	if r.Source.HDR {
		if a.HDRDisposition != "tone-mapped" || a.ToneMap != "zscale-linear-tonemap-hable-desat-0-100nit" || a.TargetNits != 100 || a.SourcePeakNits != r.Source.SourcePeakNits {
			return ErrProcess
		}
	} else if a.HDRDisposition != "sdr-normalized" || a.ToneMap != "not-needed" || a.TargetNits != 100 || a.SourcePeakNits != 0 {
		return ErrProcess
	}
	return nil
}

func auditVideoFilter(source Inspection, width, height, bitDepth int, kind string) string {
	parts := []string{}
	switch source.RotationDegrees {
	case 90:
		parts = append(parts, "transpose=clock")
	case 180:
		parts = append(parts, "hflip", "vflip")
	case 270:
		parts = append(parts, "transpose=cclock")
	}
	transfer := "bt709"
	if kind == "first-frame-avif" {
		transfer = "iec61966-2-1"
	}
	if source.HDR {
		peak := strconv.Itoa(source.SourcePeakNits)
		parts = append(parts, "zscale=transfer=linear:npl="+peak, "format=gbrpf32le", "tonemap=hable:desat=0:peak="+peak, "zscale=primaries=bt709:transfer="+transfer+":matrix=bt709:range=limited:npl=100")
	} else {
		parts = append(parts, "zscale=primaries=bt709:transfer="+transfer+":matrix=bt709:range=limited")
	}
	format := "format=yuv420p"
	if bitDepth == 10 {
		format = "format=yuv420p10le"
	}
	parts = append(parts, "scale="+strconv.Itoa(width)+":"+strconv.Itoa(height)+":flags=lanczos", "setsar=1", format, "setpts=PTS-STARTPTS")
	return strings.Join(parts, ",")
}

func auditAudio(source Inspection, kind string) (int, string) {
	if kind != "mp4-av1" || !source.AudioPresent {
		return 0, "none"
	}
	supported := map[int]bool{7350: true, 8000: true, 11025: true, 12000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true, 64000: true, 88200: true, 96000: true}
	if supported[source.AudioSampleRate] {
		return source.AudioSampleRate, "asetpts=PTS-STARTPTS"
	}
	return 48000, "aresample=48000,asetpts=PTS-STARTPTS"
}

func auditInputColor(source Inspection) string {
	return source.ColorPrimaries + "/" + source.ColorTransfer + "/" + source.ColorMatrix + "/" + source.ColorRange
}

func validateCapability(r capabilityWire, digest string) error {
	if !safeString(r.HelperVersion) || r.BuildManifest != BuildManifest || validateVersionMap(r.LibraryVersions) != nil || !slices.Equal(r.DecoderMIMETypes, []string{"video/mp4", "video/quicktime"}) || !slices.Equal(r.OutputKinds, []string{"first-frame-avif", "mp4-av1"}) || r.VideoEncoder != "libsvtav1" || r.AudioEncoder != "aac-lc" || r.VideoMuxer != "mp4" || r.ToneMap != "zscale+hable" || r.ICCSHA256 != digest || r.Threads != 1 {
		return ErrCapability
	}
	return nil
}

func validateVersionMap(v map[string]string) error {
	expected := map[string]string{"ffmpeg": "9.0.2", "libsvtav1": "4.2.0", "zimg": "3.0.6", "libaom": "v3.8.2"}
	if len(v) != len(expected) {
		return ErrProcess
	}
	for name, version := range v {
		if expected[name] != version {
			return ErrProcess
		}
	}
	return nil
}

func validPositiveRational(r Rational) bool { return r.Numerator > 0 && r.Denominator > 0 }
func oneOfInt(v int, allowed ...int) bool   { return slices.Contains(allowed, v) }

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, '\x00')
}
func reopenInput(file *os.File) (*os.File, error) {
	return os.Open("/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10))
}
func verifiedRegularFile(path, expected string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrInvalid
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		file.Close()
		return nil, err
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != expected {
		file.Close()
		return nil, ErrInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func mapRunError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, processrunner.ErrTimeout):
		return ErrTimeout
	case errors.Is(err, processrunner.ErrOutputLimit):
		return ErrLogOutputLimit
	case errors.Is(err, processrunner.ErrFileSizeLimit):
		return ErrResourcePolicy
	default:
		return ErrProcess
	}
}
func mapOperationError(err error, parent, operation context.Context) error {
	if operation.Err() != nil && context.Cause(operation) == errOperationTimeout {
		return ErrTimeout
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return mapRunError(err)
}
func mapCapabilityError(code string) error {
	if code == "resource_limit" || code == "policy_violation" {
		return ErrResourcePolicy
	}
	return ErrCapability
}
func mapInspectError(code string) error {
	switch code {
	case "unsupported_input":
		return ErrUnsupportedInput
	case "decode_failed":
		return ErrDecode
	case "capability_failed":
		return ErrCapability
	case "resource_limit", "policy_violation":
		return ErrResourcePolicy
	default:
		return ErrProcess
	}
}
func mapTransformError(code string) error {
	switch code {
	case "unsupported_input":
		return ErrUnsupportedInput
	case "decode_failed":
		return ErrDecode
	case "capability_failed":
		return ErrCapability
	case "resource_limit", "policy_violation", "output_too_large":
		return ErrResourcePolicy
	default:
		return ErrProcess
	}
}

func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }
func safeString(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}
func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for k, v := range source {
		result[k] = v
	}
	return result
}

func publicAudit(w auditWire) Audit {
	return Audit{w.ToolVersion, w.BuildManifest, cloneMap(w.LibraryVersions), w.ICCSHA256, w.Demuxer, w.VideoDecoder, w.AudioDecoder, w.VideoEncoder, w.AudioEncoder, w.Muxer, w.SelectedVideoStream, w.SelectedAudioStream, w.StreamSelection, w.RotationSource, w.RotationDegreesApplied, w.Orientation, w.Geometry, w.Timing, w.VideoFilterGraph, w.AudioFilterGraph, w.InputColor, w.OutputColor, w.OutputPrimaries, w.OutputTransfer, w.OutputMatrix, w.OutputRange, w.HDRDisposition, w.ToneMap, w.TargetNits, w.SourcePeakNits, w.InputAudioLayout, w.OutputAudioLayout, w.InputAudioSampleRate, w.OutputAudioSampleRate, w.Metadata}
}

func validEnvelope(protocol int, ok bool, code string, hasResult bool) bool {
	return protocol == ProtocolVersion && (ok && code == "" && hasResult || !ok && code != "" && !hasResult)
}
func decodeProtocolJSON(data []byte, target any) error {
	if len(data) == 0 || int64(len(data)) > MaxLogBytesPerStream || !utf8.Valid(data) || rejectDuplicateJSONFields(data) != nil {
		return ErrProcess
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || !exactFields(raw, "protocol", "ok", "error_code", "result") {
		return ErrProcess
	}
	for _, name := range []string{"protocol", "ok", "error_code"} {
		if bytes.Equal(bytes.TrimSpace(raw[name]), []byte("null")) {
			return ErrProcess
		}
	}
	if !bytes.Equal(bytes.TrimSpace(raw["result"]), []byte("null")) && containsJSONNull(raw["result"]) {
		return ErrProcess
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return ErrProcess
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ErrProcess
	}
	return nil
}
func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrProcess
				}
				if _, exists := seen[key]; exists {
					return ErrProcess
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return ErrProcess
		}
	}
	return walk()
}
func containsJSONNull(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := d.Token()
		if err != nil {
			return err != io.EOF
		}
		if token == nil {
			return true
		}
	}
}
func exactFields(m map[string]json.RawMessage, names ...string) bool {
	if len(m) != len(names) {
		return false
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			return false
		}
	}
	return true
}

type capabilityResponse struct {
	Protocol  int             `json:"protocol"`
	OK        bool            `json:"ok"`
	ErrorCode string          `json:"error_code"`
	Result    *capabilityWire `json:"result"`
}
type capabilityWire struct {
	HelperVersion    string            `json:"helper_version"`
	LibraryVersions  map[string]string `json:"library_versions"`
	DecoderMIMETypes []string          `json:"decoder_mime_types"`
	OutputKinds      []string          `json:"output_kinds"`
	VideoEncoder     string            `json:"video_encoder"`
	AudioEncoder     string            `json:"audio_encoder"`
	VideoMuxer       string            `json:"video_muxer"`
	ToneMap          string            `json:"tone_map"`
	ICCSHA256        string            `json:"icc_sha256"`
	Threads          int               `json:"threads"`
	BuildManifest    string            `json:"build_manifest"`
}
type inspectionResponse struct {
	Protocol  int         `json:"protocol"`
	OK        bool        `json:"ok"`
	ErrorCode string      `json:"error_code"`
	Result    *Inspection `json:"result"`
}
type transformResponse struct {
	Protocol  int            `json:"protocol"`
	OK        bool           `json:"ok"`
	ErrorCode string         `json:"error_code"`
	Result    *transformWire `json:"result"`
}
type transformWire struct {
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
	Source           Inspection `json:"source"`
	Audit            auditWire  `json:"audit"`
}
type auditWire struct {
	ToolVersion            string            `json:"tool_version"`
	BuildManifest          string            `json:"build_manifest"`
	LibraryVersions        map[string]string `json:"library_versions"`
	ICCSHA256              string            `json:"icc_sha256"`
	Demuxer                string            `json:"demuxer"`
	VideoDecoder           string            `json:"video_decoder"`
	AudioDecoder           string            `json:"audio_decoder"`
	VideoEncoder           string            `json:"video_encoder"`
	AudioEncoder           string            `json:"audio_encoder"`
	Muxer                  string            `json:"muxer"`
	SelectedVideoStream    int               `json:"selected_video_stream"`
	SelectedAudioStream    int               `json:"selected_audio_stream"`
	StreamSelection        string            `json:"stream_selection"`
	RotationSource         string            `json:"rotation_source"`
	RotationDegreesApplied int               `json:"rotation_degrees_applied"`
	Orientation            string            `json:"orientation"`
	Geometry               string            `json:"geometry"`
	Timing                 string            `json:"timing"`
	VideoFilterGraph       string            `json:"video_filter_graph"`
	AudioFilterGraph       string            `json:"audio_filter_graph"`
	InputColor             string            `json:"input_color"`
	OutputColor            string            `json:"output_color"`
	OutputPrimaries        string            `json:"output_primaries"`
	OutputTransfer         string            `json:"output_transfer"`
	OutputMatrix           string            `json:"output_matrix"`
	OutputRange            string            `json:"output_range"`
	HDRDisposition         string            `json:"hdr_disposition"`
	ToneMap                string            `json:"tone_map"`
	TargetNits             int               `json:"target_nits"`
	SourcePeakNits         int               `json:"source_peak_nits"`
	InputAudioLayout       string            `json:"input_audio_layout"`
	OutputAudioLayout      string            `json:"output_audio_layout"`
	InputAudioSampleRate   int               `json:"input_audio_sample_rate"`
	OutputAudioSampleRate  int               `json:"output_audio_sample_rate"`
	Metadata               string            `json:"metadata"`
}
type verificationResponse struct {
	Protocol  int               `json:"protocol"`
	OK        bool              `json:"ok"`
	ErrorCode string            `json:"error_code"`
	Result    *verificationWire `json:"result"`
}
type verificationWire struct {
	Kind                  string     `json:"kind"`
	Container             string     `json:"container"`
	Width                 int        `json:"width"`
	Height                int        `json:"height"`
	SampleAspectRatio     Rational   `json:"sample_aspect_ratio"`
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
	Source                Inspection `json:"source"`
}
