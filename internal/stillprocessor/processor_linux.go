//go:build linux

// Package stillprocessor runs the trusted still-image helper through a closed,
// versioned command-line and JSON protocol.
package stillprocessor

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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

const (
	ProtocolVersion = 1

	MaxTimeout              = 30 * time.Minute
	MaxAddressSpaceBytes    = uint64(4 << 30) // 4 GiB bounds codec allocations and mappings.
	MaxLogBytesPerStream    = int64(1 << 20)
	MaxGeneratedOutputBytes = int64(64 << 20) // 64 MiB is ample for a 1920-edge, 8-bit AVIF.
	MaxSourceDimension      = 1_000_000
	RequiredThreads         = 1

	OutputMIME = "image/avif"
)

var (
	ErrInvalid          = errors.New("invalid still processor input")
	ErrUnsupportedInput = errors.New("unsupported still input")
	ErrAnimatedInput    = errors.New("animated still input")
	ErrDecode           = errors.New("still input decode failed")
	ErrCapability       = errors.New("still helper capability failure")
	ErrResourcePolicy   = errors.New("still processor resource or policy failure")
	ErrProcess          = errors.New("still helper process failure")
	ErrTimeout          = errors.New("still helper timed out")
	ErrLogOutputLimit   = errors.New("still helper log output limit exceeded")
)

type Policy struct {
	Timeout                 time.Duration
	AddressSpaceBytes       uint64
	LogBytesPerStream       int64
	GeneratedOutputMaxBytes int64
	Threads                 int
}

func DefaultPolicy() Policy {
	return Policy{
		Timeout:                 MaxTimeout,
		AddressSpaceBytes:       MaxAddressSpaceBytes,
		LogBytesPerStream:       MaxLogBytesPerStream,
		GeneratedOutputMaxBytes: MaxGeneratedOutputBytes,
		Threads:                 RequiredThreads,
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
	helper    string
	srgbICC   string
	iccSHA256 string
	policy    Policy
	runner    *processrunner.Runner
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
	AVIFEncoder      string
	ICCSHA256        string
	Threads          int
}

type Result struct {
	OutputMIME      string
	OutputExtension string
	Width           int
	Height          int
	Quality         int
	BitDepth        int
	MaxLongEdge     int
	Threads         int
	Audit           Audit
}

type Audit struct {
	Decoder         string
	Encoder         string
	ToolVersion     string
	LibraryVersions map[string]string
	ICCSHA256       string
	Orientation     string
	SourceWidth     int
	SourceHeight    int
	InputColor      string
	OutputColor     string
	OutputTransfer  string
	HDRDisposition  string
	ToneMap         string
	TargetNits      int
	RawProcessing   string
	Alpha           string
	Metadata        string
	Chroma          string
}

func New(config Config) (*Processor, error) {
	if !cleanAbsolute(config.Helper) || !cleanAbsolute(config.Prlimit) || !cleanAbsolute(config.SRGBICC) || !sha256Pattern.MatchString(config.SRGBICCSHA256) {
		return nil, ErrInvalid
	}
	file, err := verifiedRegularFile(config.SRGBICC, config.SRGBICCSHA256)
	if err != nil {
		return nil, ErrInvalid
	}
	_ = file.Close()
	policy, err := normalizePolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	runner, err := processrunner.New(config.Prlimit, processrunner.Limits{
		Timeout:              policy.Timeout,
		AddressSpaceBytes:    policy.AddressSpaceBytes,
		FileSizeBytes:        uint64(policy.GeneratedOutputMaxBytes),
		OutputBytesPerStream: policy.LogBytesPerStream,
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
	arguments := []string{
		"capabilities",
		"--protocol", strconv.Itoa(ProtocolVersion),
		"--threads", strconv.Itoa(p.policy.Threads),
		"--srgb-icc", "/proc/self/fd/3",
	}
	output, err := p.runner.Run(ctx, processrunner.Command{Executable: p.helper, Arguments: arguments, Files: []*os.File{icc}})
	if err != nil {
		return Capabilities{}, mapRunError(err)
	}
	var response capabilityResponse
	if err := decodeProtocolJSON(output.Stdout, &response, capabilityJSON); err != nil {
		return Capabilities{}, ErrCapability
	}
	if response.Protocol != ProtocolVersion || (!response.OK && response.Result != nil) || (response.OK && (response.ErrorCode != "" || response.Result == nil)) {
		return Capabilities{}, ErrCapability
	}
	if !response.OK {
		return Capabilities{}, mapHelperCode(response.ErrorCode, ErrCapability)
	}
	if err := validateCapabilities(*response.Result, p.iccSHA256); err != nil {
		return Capabilities{}, ErrCapability
	}
	result := *response.Result
	return Capabilities{
		ProtocolVersion:  response.Protocol,
		HelperVersion:    result.HelperVersion,
		LibraryVersions:  cloneMap(result.LibraryVersions),
		DecoderMIMETypes: slices.Clone(result.DecoderMIMETypes),
		AVIFEncoder:      result.AVIFEncoder,
		ICCSHA256:        result.ICCSHA256,
		Threads:          result.Threads,
	}, nil
}

func (p *Processor) Transform(ctx context.Context, request Request) (Result, error) {
	if p == nil || p.runner == nil || ctx == nil || validateRequest(request) != nil {
		return Result{}, ErrInvalid
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

	arguments := []string{
		"transform",
		"--protocol", strconv.Itoa(ProtocolVersion),
		"--input", "/proc/self/fd/3",
		"--output", "/proc/self/fd/4",
		"--input-mime", request.MIMEType,
		"--source-mode", string(request.Recipe.SourceMode),
		"--max-long-edge", strconv.Itoa(request.Recipe.MaxLongEdge),
		"--quality", strconv.Itoa(request.Recipe.StillOutput.Quality),
		"--bit-depth", strconv.Itoa(request.Recipe.StillOutput.BitDepth),
		"--threads", strconv.Itoa(p.policy.Threads),
		"--max-output-bytes", strconv.FormatInt(p.policy.GeneratedOutputMaxBytes, 10),
		"--srgb-icc", "/proc/self/fd/5",
	}
	output, err := p.runner.Run(ctx, processrunner.Command{
		Executable: p.helper,
		Arguments:  arguments,
		Files:      []*os.File{input, request.Output, icc},
	})
	if err != nil {
		return Result{}, mapRunError(err)
	}
	var response transformResponse
	if err := decodeProtocolJSON(output.Stdout, &response, transformJSON); err != nil {
		return Result{}, ErrProcess
	}
	if response.Protocol != ProtocolVersion || (!response.OK && response.Result != nil) || (response.OK && (response.ErrorCode != "" || response.Result == nil)) {
		return Result{}, ErrProcess
	}
	if !response.OK {
		return Result{}, mapHelperCode(response.ErrorCode, ErrProcess)
	}
	if err := validateTransformResult(*response.Result, request.MIMEType, request.Recipe, p.policy, p.iccSHA256); err != nil {
		return Result{}, ErrProcess
	}
	info, err := request.Output.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > p.policy.GeneratedOutputMaxBytes {
		return Result{}, ErrResourcePolicy
	}
	if _, err := request.Output.Seek(0, io.SeekStart); err != nil {
		return Result{}, ErrResourcePolicy
	}
	wire := *response.Result
	return Result{
		OutputMIME:      wire.OutputMIME,
		OutputExtension: "avif",
		Width:           wire.Width,
		Height:          wire.Height,
		Quality:         wire.Quality,
		BitDepth:        wire.BitDepth,
		MaxLongEdge:     wire.MaxLongEdge,
		Threads:         wire.Threads,
		Audit: Audit{
			Decoder:         wire.Audit.Decoder,
			Encoder:         wire.Audit.Encoder,
			ToolVersion:     wire.Audit.ToolVersion,
			LibraryVersions: cloneMap(wire.Audit.LibraryVersions),
			ICCSHA256:       wire.Audit.ICCSHA256,
			Orientation:     wire.Audit.Orientation,
			SourceWidth:     wire.Audit.SourceWidth,
			SourceHeight:    wire.Audit.SourceHeight,
			InputColor:      wire.Audit.InputColor,
			OutputColor:     wire.Audit.OutputColor,
			OutputTransfer:  wire.Audit.OutputTransfer,
			HDRDisposition:  wire.Audit.HDRDisposition,
			ToneMap:         wire.Audit.ToneMap,
			TargetNits:      wire.Audit.TargetNits,
			RawProcessing:   wire.Audit.RawProcessing,
			Alpha:           wire.Audit.Alpha,
			Metadata:        wire.Audit.Metadata,
			Chroma:          wire.Audit.Chroma,
		},
	}, nil
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
	AVIFEncoder      string            `json:"avif_encoder"`
	ICCSHA256        string            `json:"icc_sha256"`
	Threads          int               `json:"threads"`
}

type transformResponse struct {
	Protocol  int              `json:"protocol"`
	OK        bool             `json:"ok"`
	ErrorCode string           `json:"error_code"`
	Result    *transformResult `json:"result"`
}

type transformResult struct {
	OutputMIME  string    `json:"output_mime"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Quality     int       `json:"quality"`
	BitDepth    int       `json:"bit_depth"`
	MaxLongEdge int       `json:"max_long_edge"`
	Threads     int       `json:"threads"`
	Audit       auditWire `json:"audit"`
}

type auditWire struct {
	Decoder         string            `json:"decoder"`
	Encoder         string            `json:"encoder"`
	ToolVersion     string            `json:"tool_version"`
	LibraryVersions map[string]string `json:"library_versions"`
	ICCSHA256       string            `json:"icc_sha256"`
	Orientation     string            `json:"orientation"`
	SourceWidth     int               `json:"source_width"`
	SourceHeight    int               `json:"source_height"`
	InputColor      string            `json:"input_color"`
	OutputColor     string            `json:"output_color"`
	OutputTransfer  string            `json:"output_transfer"`
	HDRDisposition  string            `json:"hdr_disposition"`
	ToneMap         string            `json:"tone_map"`
	TargetNits      int               `json:"target_nits"`
	RawProcessing   string            `json:"raw_processing"`
	Alpha           string            `json:"alpha"`
	Metadata        string            `json:"metadata"`
	Chroma          string            `json:"chroma"`
}

func normalizePolicy(policy Policy) (Policy, error) {
	defaults := DefaultPolicy()
	if policy.Timeout == 0 {
		policy.Timeout = defaults.Timeout
	}
	if policy.AddressSpaceBytes == 0 {
		policy.AddressSpaceBytes = defaults.AddressSpaceBytes
	}
	if policy.LogBytesPerStream == 0 {
		policy.LogBytesPerStream = defaults.LogBytesPerStream
	}
	if policy.GeneratedOutputMaxBytes == 0 {
		policy.GeneratedOutputMaxBytes = defaults.GeneratedOutputMaxBytes
	}
	if policy.Threads == 0 {
		policy.Threads = defaults.Threads
	}
	if policy.Timeout < 0 || policy.Timeout > MaxTimeout || policy.AddressSpaceBytes > MaxAddressSpaceBytes ||
		policy.LogBytesPerStream < 0 || policy.LogBytesPerStream > MaxLogBytesPerStream ||
		policy.GeneratedOutputMaxBytes < 0 || policy.GeneratedOutputMaxBytes > MaxGeneratedOutputBytes ||
		policy.AddressSpaceBytes == 0 || policy.LogBytesPerStream == 0 || policy.GeneratedOutputMaxBytes == 0 ||
		policy.Threads != RequiredThreads {
		return Policy{}, ErrInvalid
	}
	return policy, nil
}

func validateRequest(request Request) error {
	if request.Input == nil || request.Output == nil {
		return ErrInvalid
	}
	inputInfo, err := request.Input.Stat()
	if err != nil || !inputInfo.Mode().IsRegular() {
		return ErrInvalid
	}
	outputInfo, err := request.Output.Stat()
	if err != nil || !outputInfo.Mode().IsRegular() || outputInfo.Size() != 0 || os.SameFile(inputInfo, outputInfo) {
		return ErrInvalid
	}
	if offset, err := request.Output.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		return ErrInvalid
	}
	format, ok := mediaformat.Lookup(request.MIMEType)
	if !ok || (format.Family != mediaformat.FamilyStill && !(request.MIMEType == "image/webp" && format.Family == mediaformat.FamilyProbeAnimation)) {
		return ErrInvalid
	}
	expectedMode := profile.SourceStill
	if request.MIMEType == "image/webp" {
		expectedMode = profile.SourceProbeAnimation
	}
	r := request.Recipe
	if r.SourceMode != expectedMode || r.MaxLongEdge <= 0 || r.MaxLongEdge > 1920 || r.AllowUpscale ||
		r.Crop != "none" || r.DimensionRule != "preserve-aspect-no-crop-no-upscale-even-round-down" || r.Orientation != "apply" ||
		r.Color != "normalize-srgb-tone-map-hdr" || r.Metadata != "strip-after-normalization-keep-color-tags" || r.Alpha != "preserve" ||
		r.Audio != "none" || r.StreamSelection != "not-applicable" || r.StillOutput == nil || r.VideoOutput != nil {
		return ErrInvalid
	}
	if expectedMode == profile.SourceStill {
		if r.FramePolicy != profile.FrameFirst || r.AnimationOutput != nil || r.AnimationTiming != "not-applicable" || r.AnimationLoop != "not-applicable" {
			return ErrInvalid
		}
	} else {
		thumbnail := r.FramePolicy == profile.FrameFirst && r.AnimationOutput == nil && r.AnimationTiming == "first-frame" && r.AnimationLoop == "discard"
		standard := r.FramePolicy == profile.FrameAll && r.AnimationOutput != nil && r.AnimationOutput.Format == "animated-webp" &&
			r.AnimationOutput.Quality >= 1 && r.AnimationOutput.Quality <= 100 && r.AnimationOutput.BitDepth == 8 &&
			r.AnimationTiming == "preserve" && r.AnimationLoop == "preserve"
		if !thumbnail && !standard {
			return ErrInvalid
		}
	}
	if r.StillOutput.Format != "avif" || r.StillOutput.Quality < 1 || r.StillOutput.Quality > 100 || r.StillOutput.BitDepth != 8 {
		return ErrInvalid
	}
	return nil
}

func reopenInput(file *os.File) (*os.File, error) {
	return os.Open("/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10))
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateCapabilities(capability capabilityWire, expectedICC string) error {
	if !safeString(capability.HelperVersion) || capability.AVIFEncoder != "aom" || capability.Threads != RequiredThreads ||
		capability.ICCSHA256 != expectedICC || len(capability.DecoderMIMETypes) == 0 ||
		validateVersionMap(capability.LibraryVersions) != nil {
		return ErrCapability
	}
	seen := make(map[string]struct{}, len(capability.DecoderMIMETypes))
	for _, mimeType := range capability.DecoderMIMETypes {
		format, ok := mediaformat.Lookup(mimeType)
		if !ok || (format.Family != mediaformat.FamilyStill && mimeType != "image/webp") {
			return ErrCapability
		}
		if _, duplicate := seen[mimeType]; duplicate {
			return ErrCapability
		}
		seen[mimeType] = struct{}{}
	}
	if !sort.StringsAreSorted(capability.DecoderMIMETypes) {
		return ErrCapability
	}
	return nil
}

func validateTransformResult(result transformResult, mimeType string, recipe profile.Recipe, policy Policy, expectedICC string) error {
	if result.OutputMIME != OutputMIME || result.Width <= 0 || result.Height <= 0 || result.Width > recipe.MaxLongEdge || result.Height > recipe.MaxLongEdge ||
		result.Quality != recipe.StillOutput.Quality || result.BitDepth != recipe.StillOutput.BitDepth || result.MaxLongEdge != recipe.MaxLongEdge || result.Threads != policy.Threads {
		return ErrProcess
	}
	a := result.Audit
	if !validDecoder(mimeType, a.Decoder) || a.Encoder != "aom" || !safeString(a.ToolVersion) || validateVersionMap(a.LibraryVersions) != nil ||
		a.ICCSHA256 != expectedICC || a.Orientation != "applied" || a.SourceWidth <= 0 || a.SourceWidth > MaxSourceDimension ||
		a.SourceHeight <= 0 || a.SourceHeight > MaxSourceDimension ||
		!oneOf(a.InputColor, "embedded-icc", "assumed-srgb", "raw-camera-matrix", "nclx-pq", "nclx-hlg") ||
		a.OutputColor != "srgb" || a.OutputTransfer != "srgb" ||
		!oneOf(a.HDRDisposition, "sdr", "tone-mapped") || !oneOf(a.ToneMap, "not-needed", "bt2446a-method-a") ||
		!oneOf(a.Alpha, "opaque", "preserved") || a.Metadata != "strip-after-normalization-keep-color-tags" ||
		!oneOf(a.Chroma, "4:2:0", "4:4:4") {
		return ErrProcess
	}
	if a.HDRDisposition == "tone-mapped" {
		if a.ToneMap != "bt2446a-method-a" || a.TargetNits != 100 || !oneOf(a.InputColor, "nclx-pq", "nclx-hlg") {
			return ErrProcess
		}
	} else if a.ToneMap != "not-needed" || a.TargetNits != 0 {
		return ErrProcess
	}
	if oneOf(a.InputColor, "nclx-pq", "nclx-hlg") != (a.HDRDisposition == "tone-mapped") {
		return ErrProcess
	}
	if oneOf(a.InputColor, "nclx-pq", "nclx-hlg") && !oneOf(mimeType, "image/heic", "image/heif") {
		return ErrProcess
	}
	raw := isRawMIME(mimeType)
	if (raw && (a.RawProcessing != "camera-wb-camera-matrix-16bit-no-auto-bright" || a.InputColor != "raw-camera-matrix")) ||
		(!raw && (a.RawProcessing != "not-applicable" || a.InputColor == "raw-camera-matrix")) {
		return ErrProcess
	}
	if (a.Alpha == "opaque" && a.Chroma != "4:2:0") || (a.Alpha == "preserved" && a.Chroma != "4:4:4") {
		return ErrProcess
	}
	if !validResize(a.SourceWidth, a.SourceHeight, result.Width, result.Height, recipe.MaxLongEdge) {
		return ErrProcess
	}
	return nil
}

func validateVersionMap(versions map[string]string) error {
	if len(versions) == 0 || len(versions) > 32 {
		return ErrProcess
	}
	for name, version := range versions {
		if !safeString(name) || !safeString(version) {
			return ErrProcess
		}
	}
	for _, name := range []string{"libaom", "libheif", "libraw", "libvips", "lcms2"} {
		if _, ok := versions[name]; !ok {
			return ErrProcess
		}
	}
	return nil
}

func safeString(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

type protocolJSONKind int

const (
	capabilityJSON protocolJSONKind = iota
	transformJSON
)

func decodeProtocolJSON(data []byte, target any, kind protocolJSONKind) error {
	if len(data) == 0 || int64(len(data)) > MaxLogBytesPerStream || !utf8.Valid(data) {
		return ErrProcess
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	if err := validateJSONShape(data, kind); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrProcess
	}
	return nil
}

func validateJSONShape(data []byte, kind protocolJSONKind) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil || !exactFields(envelope, "protocol", "ok", "error_code", "result") {
		return ErrProcess
	}
	for _, name := range []string{"protocol", "ok", "error_code"} {
		if bytes.Equal(bytes.TrimSpace(envelope[name]), []byte("null")) {
			return ErrProcess
		}
	}
	if bytes.Equal(bytes.TrimSpace(envelope["result"]), []byte("null")) {
		return nil
	}
	if containsJSONNull(envelope["result"]) {
		return ErrProcess
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(envelope["result"], &result); err != nil {
		return ErrProcess
	}
	if kind == capabilityJSON {
		if !exactFields(result, "helper_version", "library_versions", "decoder_mime_types", "avif_encoder", "icc_sha256", "threads") {
			return ErrProcess
		}
		return nil
	}
	if !exactFields(result, "output_mime", "width", "height", "quality", "bit_depth", "max_long_edge", "threads", "audit") {
		return ErrProcess
	}
	var audit map[string]json.RawMessage
	if err := json.Unmarshal(result["audit"], &audit); err != nil || !exactFields(audit,
		"decoder", "encoder", "tool_version", "library_versions", "icc_sha256", "orientation", "source_width", "source_height", "input_color",
		"output_color", "output_transfer", "hdr_disposition", "tone_map", "target_nits", "raw_processing", "alpha", "metadata", "chroma") {
		return ErrProcess
	}
	return nil
}

func containsJSONNull(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return err != io.EOF
		}
		if token == nil {
			return true
		}
	}
}

func exactFields(fields map[string]json.RawMessage, names ...string) bool {
	if fields == nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
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
	case "unsupported_input":
		return ErrUnsupportedInput
	case "animated_input":
		return ErrAnimatedInput
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

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, '\x00')
}

func verifiedRegularFile(path, expectedSHA256 string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrInvalid
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	actual := fmt.Sprintf("%x", digest.Sum(nil))
	if actual != expectedSHA256 {
		_ = file.Close()
		return nil, ErrInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func isRawMIME(mimeType string) bool {
	switch mimeType {
	case "image/dng", "image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef",
		"image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw":
		return true
	default:
		return false
	}
}

func validResize(sourceWidth, sourceHeight, outputWidth, outputHeight, maxLongEdge int) bool {
	if outputWidth > sourceWidth || outputHeight > sourceHeight {
		return false
	}
	sourceLong := max(sourceWidth, sourceHeight)
	if sourceLong <= maxLongEdge {
		return outputWidth == sourceWidth && outputHeight == sourceHeight
	}
	if max(outputWidth, outputHeight) != maxLongEdge {
		return false
	}
	var sourceShort, outputShort int
	if sourceWidth >= sourceHeight {
		sourceShort, outputShort = sourceHeight, outputHeight
	} else {
		sourceShort, outputShort = sourceWidth, outputWidth
	}
	numerator := sourceShort * maxLongEdge
	floor := numerator / sourceLong
	ceil := (numerator + sourceLong - 1) / sourceLong
	return outputShort == floor || outputShort == ceil
}

func validDecoder(mimeType, decoder string) bool {
	allowed := map[string][]string{
		"image/bmp":             {"libvips-bmp-bi-rgb-24", "libvips-bmp-bi-rgb-32"},
		"image/dng":             {"libraw-dng"},
		"image/heic":            {"libheif-heic"},
		"image/heif":            {"libheif-heif"},
		"image/jpeg":            {"libvips-jpeg"},
		"image/png":             {"libvips-png"},
		"image/webp":            {"libvips-webp"},
		"image/x-canon-cr2":     {"libraw-cr2"},
		"image/x-canon-cr3":     {"libraw-cr3"},
		"image/x-fuji-raf":      {"libraw-raf"},
		"image/x-nikon-nef":     {"libraw-nef"},
		"image/x-olympus-orf":   {"libraw-orf"},
		"image/x-panasonic-rw2": {"libraw-rw2"},
		"image/x-sony-arw":      {"libraw-arw"},
	}
	return slices.Contains(allowed[mimeType], decoder)
}

func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }
