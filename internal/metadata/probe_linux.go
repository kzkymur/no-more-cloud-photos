//go:build linux

package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
	"golang.org/x/sys/unix"
)

type ToolPaths struct {
	ExifTool string
	FFProbe  string
	Prlimit  string
}

func DefaultToolPaths() ToolPaths {
	return ToolPaths{ExifTool: "/usr/bin/exiftool", FFProbe: "/usr/bin/ffprobe", Prlimit: "/usr/bin/prlimit"}
}

type Prober struct {
	policy      Policy
	tools       ToolPaths
	runner      boundedRunner
	mu          sync.Mutex
	vers        map[string]string
	versionGate chan struct{}
}

func NewProber(policy Policy, tools ToolPaths) (*Prober, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	for name, path := range map[string]string{"ExifTool": tools.ExifTool, "ffprobe": tools.FFProbe, "prlimit": tools.Prlimit} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("%s path must be absolute and clean", name)
		}
	}
	return &Prober{
		policy: policy, tools: tools,
		runner: boundedRunner{prlimit: tools.Prlimit, policy: policy},
		vers:   make(map[string]string), versionGate: make(chan struct{}, 1),
	}, nil
}

func (p *Prober) ProbeFile(ctx context.Context, path string, defaultTimezone string) (Result, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Result{}, fmt.Errorf("%w: probe input path is not absolute and clean", ErrInvalidMedia)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return Result{}, fmt.Errorf("%w: open probe input", ErrInvalidMedia)
	}
	file := os.NewFile(uintptr(fd), "metadata-input")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("%w: probe input is not a regular file", ErrInvalidMedia)
	}
	detected, detectErr := Detect(file, info.Size())
	if detectErr != nil {
		return Result{}, detectErr
	}
	operationCtx, cancel := context.WithTimeout(ctx, p.policy.Timeout)
	defer cancel()

	var result Result
	switch detected.Format.Family {
	case mediaformat.FamilyStill, mediaformat.FamilyProbeAnimation:
		result, err = p.probeStill(operationCtx, file, defaultTimezone, detected)
	case mediaformat.FamilyVideo:
		result, err = p.probeVideo(operationCtx, file, defaultTimezone, detected)
	default:
		err = ErrUnsupportedMediaType
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return Result{}, ErrProbeTimeout
		}
		return Result{}, err
	}
	if err := p.validateDimensions(result.Width, result.Height); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (p *Prober) validateDimensions(width, height int) error {
	if width < 1 || height < 1 {
		return fmt.Errorf("%w: missing or non-positive dimensions", ErrInvalidMedia)
	}
	if width > p.policy.MaxDimension || height > p.policy.MaxDimension {
		return fmt.Errorf("%w: %w: limit %d", ErrPolicyViolation, ErrMaxDimension, p.policy.MaxDimension)
	}
	pixels := uint64(width) * uint64(height)
	if pixels > p.policy.MaxPixels {
		return fmt.Errorf("%w: %w: limit %d", ErrPolicyViolation, ErrMaxPixels, p.policy.MaxPixels)
	}
	return nil
}

func (p *Prober) probeStill(ctx context.Context, file *os.File, timezone string, detected Detection) (Result, error) {
	arguments := []string{
		"-config", "", "-json", "-n", "-charset", "filename=UTF8",
		"-ImageWidth", "-ImageHeight", "-EXIF:Make", "-EXIF:Model", "-EXIF:LensMake", "-EXIF:LensModel",
		"-EXIF:Orientation", "-EXIF:ExposureTime", "-EXIF:FNumber", "-EXIF:ISO", "-EXIF:FocalLength",
		"-EXIF:GPSLatitude", "-EXIF:GPSLongitude", "-EXIF:GPSAltitude",
		"-EXIF:DateTimeOriginal", "-EXIF:OffsetTimeOriginal", "-EXIF:CreateDate", "-EXIF:OffsetTimeDigitized", "-EXIF:ModifyDate", "-EXIF:OffsetTime",
		"/proc/self/fd/3",
	}
	output, err := p.runner.runWithFiles(ctx, p.tools.ExifTool, []*os.File{file}, arguments...)
	if err != nil {
		return Result{}, err
	}
	values, err := decodeExifTool(output.stdout)
	if err != nil {
		return Result{}, err
	}
	width, okWidth := integerField(values, "ImageWidth")
	height, okHeight := integerField(values, "ImageHeight")
	if !okWidth || !okHeight {
		return Result{}, fmt.Errorf("%w: ExifTool omitted dimensions", ErrInvalidMedia)
	}
	candidates := []CaptureCandidate{
		exifCandidate(values, "DateTimeOriginal", "DateTimeOriginal", "OffsetTimeOriginal"),
		exifCandidate(values, "DateTimeDigitized", "CreateDate", "OffsetTimeDigitized"),
		exifCandidate(values, "DateTime", "ModifyDate", "OffsetTime"),
	}
	capture, err := ResolveCapture(candidates, timezone)
	if err != nil {
		return Result{}, err
	}
	version, err := p.toolVersion(ctx, "exiftool")
	if err != nil {
		return Result{}, err
	}
	return Result{
		MIMEType: detected.Format.MIMEType, Extension: detected.Format.Extension,
		Width: width, Height: height, EXIF: normalizedEXIF(values, capture),
		Derived: DerivedMetadata{DetectorEvidence: detected.Evidence, ProbeTool: "exiftool", ProbeVersion: version, Capture: capture},
	}, nil
}

func (p *Prober) probeVideo(ctx context.Context, file *os.File, timezone string, detected Detection) (Result, error) {
	arguments := []string{
		"-v", "error", "-protocol_whitelist", "file", "-print_format", "json",
		"-show_entries", "stream=index,codec_type,codec_name,width,height,duration:stream_disposition=default,attached_pic,still_image,timed_thumbnails:stream_tags=creation_time:format=duration:format_tags=creation_time",
		"/proc/self/fd/3",
	}
	output, err := p.runner.runWithFiles(ctx, p.tools.FFProbe, []*os.File{file}, arguments...)
	if err != nil {
		return Result{}, err
	}
	probe, err := decodeFFProbe(output.stdout)
	if err != nil {
		return Result{}, err
	}
	stream, ok := primaryVideoStream(probe.Streams)
	if !ok {
		return Result{}, fmt.Errorf("%w: no primary video stream", ErrInvalidMedia)
	}
	duration, ok := durationMilliseconds(probe.Format.Duration)
	if !ok {
		duration, ok = durationMilliseconds(stream.Duration)
	}
	if !ok {
		return Result{}, fmt.Errorf("%w: missing or invalid video duration", ErrInvalidMedia)
	}
	capture, err := ResolveCapture([]CaptureCandidate{
		{Name: "primary_stream_creation_time", Kind: CaptureCandidateVideo, DateTime: boundedMapString(stream.Tags, "creation_time")},
		{Name: "container_creation_time", Kind: CaptureCandidateVideo, DateTime: boundedMapString(probe.Format.Tags, "creation_time")},
	}, timezone)
	if err != nil {
		return Result{}, err
	}
	version, err := p.toolVersion(ctx, "ffprobe")
	if err != nil {
		return Result{}, err
	}
	streamIndex := stream.Index
	return Result{
		MIMEType: detected.Format.MIMEType, Extension: detected.Format.Extension,
		Width: stream.Width, Height: stream.Height, DurationMS: &duration,
		Derived: DerivedMetadata{
			DetectorEvidence: detected.Evidence, ProbeTool: "ffprobe", ProbeVersion: version,
			PrimaryStream: &streamIndex, VideoCodec: sanitizeString(stream.CodecName), Capture: capture,
		},
	}, nil
}

func (p *Prober) toolVersion(ctx context.Context, tool string) (string, error) {
	select {
	case p.versionGate <- struct{}{}:
		defer func() { <-p.versionGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	p.mu.Lock()
	if version := p.vers[tool]; version != "" {
		p.mu.Unlock()
		return version, nil
	}
	p.mu.Unlock()
	var executable string
	var arguments []string
	switch tool {
	case "exiftool":
		executable, arguments = p.tools.ExifTool, []string{"-ver"}
	case "ffprobe":
		executable, arguments = p.tools.FFProbe, []string{"-version"}
	default:
		return "", fmt.Errorf("%w: unknown probe tool", ErrProbeFailed)
	}
	output, err := p.runner.run(ctx, executable, arguments...)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(string(output.stdout), "\n", 2)[0])
	if line == "" || len(line) > 256 || sanitizeString(line) != line {
		return "", fmt.Errorf("%w: invalid probe version", ErrProbeFailed)
	}
	p.mu.Lock()
	p.vers[tool] = line
	p.mu.Unlock()
	return line, nil
}

func decodeExifTool(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: malformed ExifTool UTF-8", ErrProbeFailed)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var rows []map[string]json.RawMessage
	if err := decoder.Decode(&rows); err != nil || len(rows) != 1 {
		return nil, fmt.Errorf("%w: malformed ExifTool JSON", ErrProbeFailed)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return nil, fmt.Errorf("%w: trailing ExifTool JSON value", ErrProbeFailed)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing ExifTool JSON data", ErrProbeFailed)
	}
	return rows[0], nil
}

func integerField(values map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(number.String(), 10, 32)
	return int(value), err == nil
}

func stringField(values map[string]json.RawMessage, key string) string {
	raw, ok := values[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return sanitizeString(value)
}

func floatField(values map[string]json.RawMessage, key string) *float64 {
	raw, ok := values[key]
	if !ok {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return nil
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return nil
	}
	return &value
}

func intPointerField(values map[string]json.RawMessage, key string) *int {
	value, ok := integerField(values, key)
	if !ok {
		return nil
	}
	return &value
}

func boundedIntPointerField(values map[string]json.RawMessage, key string, minimum, maximum int) *int {
	value := intPointerField(values, key)
	if value == nil || *value < minimum || *value > maximum {
		return nil
	}
	return value
}

func numberOrStringPointerField(values map[string]json.RawMessage, key string) *string {
	raw, ok := values[key]
	if !ok {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := strconv.ParseFloat(number.String(), 64); err == nil && value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value) {
			normalized := number.String()
			return &normalized
		}
		return nil
	}
	value := stringField(values, key)
	if len(value) > 64 {
		return nil
	}
	rational, ok := new(big.Rat).SetString(value)
	if !ok || rational.Sign() <= 0 {
		return nil
	}
	return &value
}

func positiveFloat(value **float64) {
	if *value != nil && **value <= 0 {
		*value = nil
	}
}

func boundedFloat(value **float64, minimum, maximum float64) {
	if *value != nil && (**value < minimum || **value > maximum) {
		*value = nil
	}
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func exifCandidate(values map[string]json.RawMessage, name, dateKey, offsetKey string) CaptureCandidate {
	dateTime := stringField(values, dateKey)
	offset, offsetPresent, offsetOK := "", false, false
	if _, ok := values[offsetKey]; ok {
		offsetPresent = true
		offset = stringField(values, offsetKey)
		_, offsetOK = parseNumericOffset(offset)
	}
	return CaptureCandidate{Name: name, Kind: CaptureCandidateEXIF, DateTime: dateTime, Offset: offset, OffsetPresent: offsetPresent, OffsetOK: offsetOK}
}

func normalizedEXIF(values map[string]json.RawMessage, capture Capture) SourceEXIF {
	exif := SourceEXIF{
		Make: stringPointer(stringField(values, "Make")), Model: stringPointer(stringField(values, "Model")),
		LensMake: stringPointer(stringField(values, "LensMake")), LensModel: stringPointer(stringField(values, "LensModel")),
		Orientation: boundedIntPointerField(values, "Orientation", 1, 8), ExposureTime: numberOrStringPointerField(values, "ExposureTime"),
		Aperture: floatField(values, "FNumber"), ISO: intPointerField(values, "ISO"), FocalLengthMM: floatField(values, "FocalLength"),
		GPSLatitude: floatField(values, "GPSLatitude"), GPSLongitude: floatField(values, "GPSLongitude"), GPSAltitudeM: floatField(values, "GPSAltitude"),
	}
	positiveFloat(&exif.Aperture)
	positiveFloat(&exif.FocalLengthMM)
	if exif.ISO != nil && *exif.ISO <= 0 {
		exif.ISO = nil
	}
	boundedFloat(&exif.GPSLatitude, -90, 90)
	boundedFloat(&exif.GPSLongitude, -180, 180)
	if capture.Selected != nil {
		exif.DateTimeRaw = stringPointer(capture.Selected.DateTime)
		exif.DateTimeOffset = stringPointer(capture.Selected.Offset)
	}
	return exif
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Duration    string            `json:"duration"`
	Tags        map[string]string `json:"tags"`
	Disposition struct {
		Default         int `json:"default"`
		AttachedPic     int `json:"attached_pic"`
		StillImage      int `json:"still_image"`
		TimedThumbnails int `json:"timed_thumbnails"`
	} `json:"disposition"`
}

type ffprobeFormat struct {
	Duration string            `json:"duration"`
	Tags     map[string]string `json:"tags"`
}

func decodeFFProbe(data []byte) (ffprobeOutput, error) {
	if !utf8.Valid(data) {
		return ffprobeOutput{}, fmt.Errorf("%w: malformed ffprobe UTF-8", ErrProbeFailed)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var output ffprobeOutput
	if err := decoder.Decode(&output); err != nil {
		return ffprobeOutput{}, fmt.Errorf("%w: malformed ffprobe JSON", ErrProbeFailed)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return ffprobeOutput{}, fmt.Errorf("%w: trailing ffprobe JSON value", ErrProbeFailed)
	} else if !errors.Is(err, io.EOF) {
		return ffprobeOutput{}, fmt.Errorf("%w: trailing ffprobe JSON data", ErrProbeFailed)
	}
	return output, nil
}

func primaryVideoStream(streams []ffprobeStream) (ffprobeStream, bool) {
	videos := make([]ffprobeStream, 0, len(streams))
	for _, stream := range streams {
		if stream.CodecType == "video" && stream.Width > 0 && stream.Height > 0 &&
			stream.Disposition.AttachedPic == 0 && stream.Disposition.StillImage == 0 && stream.Disposition.TimedThumbnails == 0 {
			videos = append(videos, stream)
		}
	}
	if len(videos) == 0 {
		return ffprobeStream{}, false
	}
	sort.Slice(videos, func(i, j int) bool {
		if videos[i].Disposition.Default != videos[j].Disposition.Default {
			return videos[i].Disposition.Default > videos[j].Disposition.Default
		}
		areaI := uint64(videos[i].Width) * uint64(videos[i].Height)
		areaJ := uint64(videos[j].Width) * uint64(videos[j].Height)
		if areaI != areaJ {
			return areaI > areaJ
		}
		return videos[i].Index < videos[j].Index
	})
	return videos[0], true
}

func durationMilliseconds(raw string) (int64, bool) {
	if raw == "" || len(raw) > 64 {
		return 0, false
	}
	seconds, ok := new(big.Rat).SetString(raw)
	if !ok || seconds.Sign() <= 0 {
		return 0, false
	}
	scaled := new(big.Rat).Mul(seconds, big.NewRat(1000, 1))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(scaled.Num(), scaled.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(scaled.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Sign() <= 0 {
		return 0, false
	}
	return quotient.Int64(), true
}

func boundedMapString(values map[string]string, key string) string {
	return sanitizeString(values[key])
}

func sanitizeString(value string) string {
	if !utf8.ValidString(value) || len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}
