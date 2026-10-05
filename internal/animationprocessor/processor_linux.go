//go:build linux

// Package animationprocessor runs the trusted GIF/animated-WebP helper through
// a closed, versioned descriptor-only protocol.
package animationprocessor

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
	"time"
	"unicode/utf8"

	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

const (
	ProtocolVersion = 1
	BuildManifest   = "giflib=5.2.2;libwebp=1.6.0;simd=on;threads=on;near-lossless=on"

	MaxTimeout                 = 2 * time.Hour
	MaxAddressSpaceBytes       = uint64(4 << 30)
	MaxLogBytesPerStream       = int64(1 << 20)
	MaxGeneratedOutputBytes    = int64(256 << 20)
	MaxFrames                  = 1000
	MaxDurationMS              = int64(3_600_000)
	MaxDimension               = 100_000
	MaxCanvasPixels            = uint64(1_000_000_000)
	MaxCumulativeDecodedPixels = uint64(1_000_000_000)
	RequiredThreads            = 1
)

var (
	ErrInvalid          = errors.New("invalid animation processor input")
	ErrStaticInput      = errors.New("static WebP input")
	ErrUnsupportedInput = errors.New("unsupported animation input")
	ErrDecode           = errors.New("animation input decode failed")
	ErrCapability       = errors.New("animation helper capability failure")
	ErrResourcePolicy   = errors.New("animation processor resource or policy failure")
	ErrProcess          = errors.New("animation helper process failure")
	ErrTimeout          = errors.New("animation helper timed out")
	ErrLogOutputLimit   = errors.New("animation helper log output limit exceeded")
)

type Classification string

const (
	ClassificationStatic    Classification = "static"
	ClassificationAnimation Classification = "animation"
)

type Policy struct {
	Timeout                 time.Duration
	AddressSpaceBytes       uint64
	LogBytesPerStream       int64
	GeneratedOutputMaxBytes int64
	Frames                  int
	DurationMS              int64
	Dimension               int
	CanvasPixels            uint64
	CumulativeDecodedPixels uint64
	Threads                 int
}

func DefaultPolicy() Policy {
	return Policy{
		Timeout: MaxTimeout, AddressSpaceBytes: MaxAddressSpaceBytes,
		LogBytesPerStream: MaxLogBytesPerStream, GeneratedOutputMaxBytes: MaxGeneratedOutputBytes,
		Frames: MaxFrames, DurationMS: MaxDurationMS, Dimension: MaxDimension,
		CanvasPixels: MaxCanvasPixels, CumulativeDecodedPixels: MaxCumulativeDecodedPixels,
		Threads: RequiredThreads,
	}
}

type Config struct {
	Helper        string
	Prlimit       string
	SRGBICC       string
	SRGBICCSHA256 string
	Policy        Policy
}

type Processor struct {
	helper, srgbICC, iccSHA256 string
	policy                     Policy
	runner                     *processrunner.Runner
}

type InspectRequest struct {
	Input    *os.File
	MIMEType string
}

type Request struct {
	Input    *os.File
	Output   *os.File
	MIMEType string
	Recipe   profile.Recipe
}

type Capabilities struct {
	ProtocolVersion  int
	HelperVersion    string
	LibraryVersions  map[string]string
	DecoderMIMETypes []string
	Encoders         []string
	ICCSHA256        string
	Threads          int
	BuildManifest    string
}

type Inspection struct {
	Classification           Classification `json:"classification"`
	Width                    int            `json:"width"`
	Height                   int            `json:"height"`
	FrameCount               int            `json:"frame_count"`
	FrameDurationsMS         []int          `json:"frame_durations_ms"`
	DurationMS               int64          `json:"duration_ms"`
	TotalPlays               int            `json:"total_plays"`
	HasAlpha                 bool           `json:"has_alpha"`
	ZeroDurationFrameIndices []int          `json:"zero_duration_frame_indices"`
	DecodedPixels            uint64         `json:"decoded_pixels"`
}

type Result struct {
	OutputMIME, OutputExtension string
	Width, Height               int
	Quality, BitDepth           int
	MaxLongEdge, Threads        int
	Source                      Inspection
	Audit                       Audit
}

type Audit struct {
	Decoder, Encoder, ToolVersion            string
	LibraryVersions                          map[string]string
	ICCSHA256, Composition                   string
	TimingNormalization, LoopNormalization   string
	InputColor, OutputColor, Alpha, Metadata string
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
	return &Processor{helper: config.Helper, srgbICC: config.SRGBICC, iccSHA256: config.SRGBICCSHA256, policy: policy, runner: runner}, nil
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
	output, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: []string{
		"capabilities", "--protocol", strconv.Itoa(ProtocolVersion), "--threads", strconv.Itoa(p.policy.Threads), "--srgb-icc", "/proc/self/fd/3",
	}, Files: []*os.File{icc}})
	if err != nil {
		return Capabilities{}, mapRunError(err)
	}
	var response capabilityResponse
	if decodeProtocolJSON(output.Stdout, &response) != nil || response.Protocol != ProtocolVersion ||
		(!response.OK && response.Result != nil) || (response.OK && (response.ErrorCode != "" || response.Result == nil)) {
		return Capabilities{}, ErrCapability
	}
	if !response.OK {
		return Capabilities{}, mapHelperCode(response.ErrorCode, ErrCapability)
	}
	if validateCapability(*response.Result, p.iccSHA256) != nil {
		return Capabilities{}, ErrCapability
	}
	r := response.Result
	return Capabilities{ProtocolVersion: response.Protocol, HelperVersion: r.HelperVersion,
		LibraryVersions: cloneMap(r.LibraryVersions), DecoderMIMETypes: slices.Clone(r.DecoderMIMETypes),
		Encoders: slices.Clone(r.Encoders), ICCSHA256: r.ICCSHA256, Threads: r.Threads,
		BuildManifest: r.BuildManifest}, nil
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
	arguments := append([]string{"inspect", "--protocol", strconv.Itoa(ProtocolVersion), "--input", "/proc/self/fd/3", "--input-mime", request.MIMEType}, p.limitArguments()...)
	output, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{input}})
	if err != nil {
		return Inspection{}, mapRunError(err)
	}
	var response inspectionResponse
	if decodeProtocolJSON(output.Stdout, &response) != nil || response.Protocol != ProtocolVersion ||
		(!response.OK && response.Result != nil) || (response.OK && (response.ErrorCode != "" || response.Result == nil)) {
		return Inspection{}, ErrProcess
	}
	if !response.OK {
		return Inspection{}, mapHelperCode(response.ErrorCode, ErrProcess)
	}
	if validateInspection(*response.Result, request.MIMEType, p.policy) != nil {
		return Inspection{}, ErrProcess
	}
	return cloneInspection(*response.Result), nil
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
	quality, bitDepth, outputKind := recipeOutput(request.Recipe)
	arguments := []string{
		"transform", "--protocol", strconv.Itoa(ProtocolVersion), "--input", "/proc/self/fd/3", "--output", "/proc/self/fd/4",
		"--input-mime", request.MIMEType, "--output-kind", outputKind, "--max-long-edge", strconv.Itoa(request.Recipe.MaxLongEdge),
		"--quality", strconv.Itoa(quality), "--bit-depth", strconv.Itoa(bitDepth), "--threads", strconv.Itoa(p.policy.Threads),
		"--srgb-icc", "/proc/self/fd/5",
	}
	arguments = append(arguments, p.limitArguments()...)
	output, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{input, request.Output, icc}})
	if err != nil {
		return Result{}, mapRunError(err)
	}
	var response transformResponse
	if decodeProtocolJSON(output.Stdout, &response) != nil || response.Protocol != ProtocolVersion ||
		(!response.OK && response.Result != nil) || (response.OK && (response.ErrorCode != "" || response.Result == nil)) {
		return Result{}, ErrProcess
	}
	if !response.OK {
		return Result{}, mapHelperCode(response.ErrorCode, ErrProcess)
	}
	if validateTransform(*response.Result, request, p.policy, p.iccSHA256) != nil {
		return Result{}, ErrProcess
	}
	info, err := request.Output.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > p.policy.GeneratedOutputMaxBytes {
		return Result{}, ErrResourcePolicy
	}
	if _, err := request.Output.Seek(0, io.SeekStart); err != nil {
		return Result{}, ErrResourcePolicy
	}
	r := response.Result
	if validateOutputSignature(request.Output, r.OutputMIME) != nil {
		return Result{}, ErrProcess
	}
	extension := "webp"
	if r.OutputMIME == "image/avif" {
		extension = "avif"
	}
	return Result{r.OutputMIME, extension, r.Width, r.Height, r.Quality, r.BitDepth, r.MaxLongEdge, r.Threads, cloneInspection(r.Source), Audit{
		r.Audit.Decoder, r.Audit.Encoder, r.Audit.ToolVersion, cloneMap(r.Audit.LibraryVersions), r.Audit.ICCSHA256,
		r.Audit.Composition, r.Audit.TimingNormalization, r.Audit.LoopNormalization, r.Audit.InputColor, r.Audit.OutputColor, r.Audit.Alpha, r.Audit.Metadata,
	}}, nil
}

func validateOutputSignature(file *os.File, mimeType string) error {
	var header [12]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return ErrProcess
	}
	valid := mimeType == "image/webp" && string(header[0:4]) == "RIFF" && string(header[8:12]) == "WEBP"
	valid = valid || (mimeType == "image/avif" && string(header[4:8]) == "ftyp" && (string(header[8:12]) == "avif" || string(header[8:12]) == "avis"))
	if _, err := file.Seek(0, io.SeekStart); err != nil || !valid {
		return ErrProcess
	}
	return nil
}

func (p *Processor) limitArguments() []string {
	return []string{
		"--max-frames", strconv.Itoa(p.policy.Frames), "--max-duration-ms", strconv.FormatInt(p.policy.DurationMS, 10),
		"--max-dimension", strconv.Itoa(p.policy.Dimension), "--max-canvas-pixels", strconv.FormatUint(p.policy.CanvasPixels, 10),
		"--max-decoded-pixels", strconv.FormatUint(p.policy.CumulativeDecodedPixels, 10),
		"--max-output-bytes", strconv.FormatInt(p.policy.GeneratedOutputMaxBytes, 10),
	}
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
	Encoders         []string          `json:"encoders"`
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
	OutputMIME  string     `json:"output_mime"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	Quality     int        `json:"quality"`
	BitDepth    int        `json:"bit_depth"`
	MaxLongEdge int        `json:"max_long_edge"`
	Threads     int        `json:"threads"`
	Source      Inspection `json:"source"`
	Audit       auditWire  `json:"audit"`
}
type auditWire struct {
	Decoder             string            `json:"decoder"`
	Encoder             string            `json:"encoder"`
	ToolVersion         string            `json:"tool_version"`
	LibraryVersions     map[string]string `json:"library_versions"`
	ICCSHA256           string            `json:"icc_sha256"`
	Composition         string            `json:"composition"`
	TimingNormalization string            `json:"timing_normalization"`
	LoopNormalization   string            `json:"loop_normalization"`
	InputColor          string            `json:"input_color"`
	OutputColor         string            `json:"output_color"`
	Alpha               string            `json:"alpha"`
	Metadata            string            `json:"metadata"`
}

func normalizePolicy(p Policy) (Policy, error) {
	d := DefaultPolicy()
	if p.Timeout == 0 {
		p.Timeout = d.Timeout
	}
	if p.AddressSpaceBytes == 0 {
		p.AddressSpaceBytes = d.AddressSpaceBytes
	}
	if p.LogBytesPerStream == 0 {
		p.LogBytesPerStream = d.LogBytesPerStream
	}
	if p.GeneratedOutputMaxBytes == 0 {
		p.GeneratedOutputMaxBytes = d.GeneratedOutputMaxBytes
	}
	if p.Frames == 0 {
		p.Frames = d.Frames
	}
	if p.DurationMS == 0 {
		p.DurationMS = d.DurationMS
	}
	if p.Dimension == 0 {
		p.Dimension = d.Dimension
	}
	if p.CanvasPixels == 0 {
		p.CanvasPixels = d.CanvasPixels
	}
	if p.CumulativeDecodedPixels == 0 {
		p.CumulativeDecodedPixels = d.CumulativeDecodedPixels
	}
	if p.Threads == 0 {
		p.Threads = d.Threads
	}
	if p.Timeout < 0 || p.Timeout > MaxTimeout || p.AddressSpaceBytes == 0 || p.AddressSpaceBytes > MaxAddressSpaceBytes ||
		p.LogBytesPerStream <= 0 || p.LogBytesPerStream > MaxLogBytesPerStream || p.GeneratedOutputMaxBytes <= 0 || p.GeneratedOutputMaxBytes > MaxGeneratedOutputBytes ||
		p.Frames <= 0 || p.Frames > MaxFrames || p.DurationMS <= 0 || p.DurationMS > MaxDurationMS || p.Dimension <= 0 || p.Dimension > MaxDimension ||
		p.CanvasPixels == 0 || p.CanvasPixels > MaxCanvasPixels || p.CumulativeDecodedPixels == 0 || p.CumulativeDecodedPixels > MaxCumulativeDecodedPixels || p.Threads != RequiredThreads {
		return Policy{}, ErrInvalid
	}
	return p, nil
}

func validateInput(input *os.File, mimeType string) error {
	if input == nil || (mimeType != "image/gif" && mimeType != "image/webp") {
		return ErrInvalid
	}
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ErrInvalid
	}
	return nil
}

func validateRequest(r Request) error {
	if validateInput(r.Input, r.MIMEType) != nil || r.Output == nil {
		return ErrInvalid
	}
	in, _ := r.Input.Stat()
	out, err := r.Output.Stat()
	if err != nil || !out.Mode().IsRegular() || out.Size() != 0 || os.SameFile(in, out) {
		return ErrInvalid
	}
	if offset, err := r.Output.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		return ErrInvalid
	}
	recipe := r.Recipe
	if recipe.SourceMode != profile.SourceProbeAnimation || recipe.MaxLongEdge <= 0 || recipe.MaxLongEdge > 1920 || recipe.AllowUpscale || recipe.Crop != "none" ||
		recipe.DimensionRule != "preserve-aspect-no-crop-no-upscale-round-nearest" || recipe.Orientation != "apply" || recipe.Color != "normalize-srgb-tone-map-hdr" ||
		recipe.Metadata != "strip-after-normalization-keep-color-tags" || recipe.Alpha != "preserve" || recipe.Audio != "none" || recipe.StreamSelection != "not-applicable" || recipe.VideoOutput != nil || recipe.StillOutput == nil ||
		recipe.StillOutput.Format != "avif" || recipe.StillOutput.Quality < 1 || recipe.StillOutput.Quality > 100 || recipe.StillOutput.BitDepth != 8 {
		return ErrInvalid
	}
	thumbnail := recipe.FramePolicy == profile.FrameFirst && recipe.AnimationOutput == nil && recipe.AnimationTiming == "first-frame" && recipe.AnimationLoop == "discard"
	standard := recipe.FramePolicy == profile.FrameAll && recipe.AnimationOutput != nil && recipe.AnimationOutput.Format == "animated-webp" && recipe.AnimationOutput.Quality >= 1 && recipe.AnimationOutput.Quality <= 100 && recipe.AnimationOutput.BitDepth == 8 && recipe.AnimationTiming == "preserve" && recipe.AnimationLoop == "preserve"
	if !thumbnail && !standard {
		return ErrInvalid
	}
	return nil
}

func recipeOutput(r profile.Recipe) (int, int, string) {
	if r.FramePolicy == profile.FrameAll {
		return r.AnimationOutput.Quality, r.AnimationOutput.BitDepth, "animated-webp"
	}
	return r.StillOutput.Quality, r.StillOutput.BitDepth, "first-frame-avif"
}

func validateCapability(c capabilityWire, digest string) error {
	if !safeString(c.HelperVersion) || c.ICCSHA256 != digest || c.Threads != 1 || c.BuildManifest != BuildManifest || !slices.Equal(c.DecoderMIMETypes, []string{"image/gif", "image/webp"}) || !slices.Equal(c.Encoders, []string{"animated-webp", "avif"}) || validateVersionMap(c.LibraryVersions) != nil {
		return ErrCapability
	}
	return nil
}

func validateInspection(i Inspection, mimeType string, p Policy) error {
	if i.Width <= 0 || i.Height <= 0 || i.Width > p.Dimension || i.Height > p.Dimension || uint64(i.Width)*uint64(i.Height) > p.CanvasPixels || i.FrameCount <= 0 || i.FrameCount > p.Frames || len(i.FrameDurationsMS) != i.FrameCount || i.DurationMS <= 0 || i.DurationMS > p.DurationMS || i.TotalPlays < 0 || i.TotalPlays > 65535 || i.DecodedPixels == 0 || i.DecodedPixels > p.CumulativeDecodedPixels {
		return ErrProcess
	}
	if (mimeType == "image/gif" && i.Classification != ClassificationAnimation) || (mimeType == "image/webp" && i.Classification != ClassificationStatic && i.Classification != ClassificationAnimation) || (mimeType == "image/webp" && (i.FrameCount == 1) != (i.Classification == ClassificationStatic)) {
		return ErrProcess
	}
	var sum int64
	for _, duration := range i.FrameDurationsMS {
		if duration <= 0 || int64(duration) > p.DurationMS || sum > p.DurationMS-int64(duration) {
			return ErrProcess
		}
		sum += int64(duration)
	}
	if sum != i.DurationMS {
		return ErrProcess
	}
	last := -1
	for _, index := range i.ZeroDurationFrameIndices {
		if index <= last || index < 0 || index >= i.FrameCount || i.FrameDurationsMS[index] != 100 {
			return ErrProcess
		}
		last = index
	}
	return nil
}

func validateTransform(r transformWire, request Request, p Policy, digest string) error {
	if validateInspection(r.Source, request.MIMEType, p) != nil || r.Source.Classification != ClassificationAnimation {
		return ErrProcess
	}
	quality, bitDepth, kind := recipeOutput(request.Recipe)
	mime := "image/webp"
	encoder := "libwebp"
	if kind == "first-frame-avif" {
		mime, encoder = "image/avif", "aom"
	}
	if r.OutputMIME != mime || r.Width <= 0 || r.Height <= 0 || r.Width > request.Recipe.MaxLongEdge || r.Height > request.Recipe.MaxLongEdge || r.Quality != quality || r.BitDepth != bitDepth || r.MaxLongEdge != request.Recipe.MaxLongEdge || r.Threads != p.Threads || !validResize(r.Source.Width, r.Source.Height, r.Width, r.Height, request.Recipe.MaxLongEdge) {
		return ErrProcess
	}
	a := r.Audit
	decoder := map[string]string{"image/gif": "giflib-gif", "image/webp": "libwebp-animation"}[request.MIMEType]
	if a.Decoder != decoder || a.Encoder != encoder || !safeString(a.ToolVersion) || validateVersionMap(a.LibraryVersions) != nil || a.ICCSHA256 != digest || a.Composition != "composited-rgba" || a.TimingNormalization != "zero-duration-to-100ms" || a.LoopNormalization != "total-play-count" || !oneOf(a.InputColor, "embedded-icc", "assumed-srgb") || a.OutputColor != "srgb" || !oneOf(a.Alpha, "opaque", "preserved") || a.Metadata != "strip-after-normalization-keep-color-tags" {
		return ErrProcess
	}
	return nil
}

func validateVersionMap(v map[string]string) error {
	expected := map[string]string{"giflib": "5.2.2", "libwebp": "1.6.0", "libwebp-demux": "1.6.0", "libwebp-mux": "1.6.0", "libheif": "1.23.5", "libaom": "v3.8.2", "lcms2": "2.14"}
	if len(v) != len(expected) {
		return ErrProcess
	}
	for name, version := range v {
		if !safeString(name) || !safeString(version) || expected[name] != version {
			return ErrProcess
		}
	}
	return nil
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
func safeString(s string) bool {
	if s == "" || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
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
func cloneInspection(i Inspection) Inspection {
	i.FrameDurationsMS = slices.Clone(i.FrameDurationsMS)
	i.ZeroDurationFrameIndices = slices.Clone(i.ZeroDurationFrameIndices)
	return i
}
func oneOf(v string, allowed ...string) bool { return slices.Contains(allowed, v) }

func validResize(sw, sh, ow, oh, edge int) bool {
	if ow > sw || oh > sh {
		return false
	}
	long := max(sw, sh)
	if long <= edge {
		return ow == sw && oh == sh
	}
	if max(ow, oh) != edge {
		return false
	}
	short, out := sh, oh
	if sh > sw {
		short, out = sw, ow
	}
	numerator := int64(short) * int64(edge)
	expected := max(1, int((numerator+int64(long)/2)/int64(long)))
	return out == expected
}

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
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
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
func mapHelperCode(code string, fallback error) error {
	switch code {
	case "static_input":
		return ErrStaticInput
	case "unsupported_input":
		return ErrUnsupportedInput
	case "decode_failed":
		return ErrDecode
	case "capability_failed":
		return ErrCapability
	case "resource_limit", "policy_violation", "output_too_large":
		return ErrResourcePolicy
	case "processing_failed", "encode_failed":
		return ErrProcess
	default:
		return fallback
	}
}
