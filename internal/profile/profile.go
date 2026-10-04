// Package profile defines media processing profiles and their capabilities.
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Processor               = "nmcp-media"
	ParametersSchemaVersion = 1
	EvidenceProvisional     = "provisional-unverified"
)

type SourceMode string

const (
	SourceStill          SourceMode = "still"
	SourceProbeAnimation SourceMode = "probe-animation"
	SourceVideo          SourceMode = "video"
)

type FramePolicy string

const (
	FrameFirst FramePolicy = "first"
	FrameAll   FramePolicy = "all"
)

type Definition struct {
	ID                      string
	Key                     string
	Version                 int
	InputMIMETypes          []string
	Processor               string
	ParametersSchemaVersion int
	Parameters              json.RawMessage
}

type Parameters struct {
	EvidenceStatus string            `json:"evidence_status"`
	Recipes        map[string]Recipe `json:"recipes"`
}

func (p *Parameters) UnmarshalJSON(data []byte) error {
	if err := validateJSONBRepresentable(data); err != nil {
		return err
	}
	if err := requireObjectFields(data, []string{"evidence_status", "recipes"}); err != nil {
		return err
	}
	type wire struct {
		EvidenceStatus string          `json:"evidence_status"`
		Recipes        json.RawMessage `json:"recipes"`
	}
	var decoded wire
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	var recipes map[string]Recipe
	if err := decodeStrict(decoded.Recipes, &recipes); err != nil {
		return err
	}
	if recipes == nil {
		return fmt.Errorf("decode parameters: recipes must be an object")
	}
	*p = Parameters{EvidenceStatus: decoded.EvidenceStatus, Recipes: recipes}
	return nil
}

const (
	maxParametersJSONBytes       = 1 << 20
	maxJSONNumericTokenBytes     = 128
	maxParametersJSONDepth       = 64
	maxPostgreSQLNumericExponent = int64(1_073_741_823)
	maxPostgreSQLNumericWeight   = int64(131_071)
	maxPostgreSQLNumericScale    = int64(16_383)
)

// validateJSONBRepresentable rejects input that encoding/json can normalize
// but PostgreSQL jsonb cannot store. Duplicate keys are rejected rather than
// collapsed so discarded values cannot hide invalid Unicode or numerics.
func validateJSONBRepresentable(data []byte) error {
	if len(data) > maxParametersJSONBytes {
		return fmt.Errorf("decode parameters: JSON document exceeds %d bytes", maxParametersJSONBytes)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("decode parameters: JSON document is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxParametersJSONDepth {
			return fmt.Errorf("decode parameters: JSON nesting exceeds %d", maxParametersJSONDepth)
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode parameters token: %w", err)
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				seen := make(map[string]struct{})
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return fmt.Errorf("decode parameters object key: %w", err)
					}
					key, ok := keyToken.(string)
					if !ok {
						return fmt.Errorf("decode parameters object key: expected string")
					}
					if err := validateJSONBString(key); err != nil {
						return err
					}
					if _, duplicate := seen[key]; duplicate {
						return fmt.Errorf("decode parameters object: duplicate field %q", key)
					}
					seen[key] = struct{}{}
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				closing, err := decoder.Token()
				if err != nil || closing != json.Delim('}') {
					return fmt.Errorf("decode parameters object: invalid closing delimiter")
				}
			case '[':
				for decoder.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				closing, err := decoder.Token()
				if err != nil || closing != json.Delim(']') {
					return fmt.Errorf("decode parameters array: invalid closing delimiter")
				}
			default:
				return fmt.Errorf("decode parameters: unexpected delimiter %q", value)
			}
		case string:
			return validateJSONBString(value)
		case json.Number:
			return validateJSONBNumeric(value.String())
		case bool, nil:
			return nil
		default:
			return fmt.Errorf("decode parameters: unsupported token %T", token)
		}
		return nil
	}
	if err := walk(1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode parameters: trailing JSON value")
		}
		return fmt.Errorf("decode parameters: trailing data: %w", err)
	}
	return nil
}

func validateJSONBString(value string) error {
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("decode parameters: PostgreSQL jsonb cannot store U+0000")
	}
	// encoding/json replaces unpaired UTF-16 surrogates with U+FFFD. Rejecting
	// U+FFFD conservatively prevents that normalization from hiding input that
	// PostgreSQL rejects; profile schema literals do not require this rune.
	if strings.ContainsRune(value, utf8.RuneError) {
		return fmt.Errorf("decode parameters: invalid or unsupported Unicode replacement rune")
	}
	return nil
}

func validateJSONBNumeric(encoded string) error {
	if len(encoded) > maxJSONNumericTokenBytes {
		return fmt.Errorf("decode parameters: numeric token exceeds %d bytes", maxJSONNumericTokenBytes)
	}
	mantissa := encoded
	exponent := int64(0)
	if index := strings.IndexAny(encoded, "eE"); index >= 0 {
		mantissa = encoded[:index]
		parsed, err := strconv.ParseInt(encoded[index+1:], 10, 32)
		if err != nil {
			return fmt.Errorf("decode parameters: invalid JSON numeric exponent")
		}
		exponent = parsed
	}
	if exponent < -maxPostgreSQLNumericExponent || exponent > maxPostgreSQLNumericExponent {
		return fmt.Errorf("decode parameters: JSON numeric exponent exceeds PostgreSQL range")
	}
	mantissa = strings.TrimPrefix(mantissa, "-")
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := whole + fraction
	firstSignificant := strings.IndexFunc(digits, func(r rune) bool { return r != '0' })
	displayScale := int64(len(fraction)) - exponent
	if displayScale < 0 {
		displayScale = 0
	}
	if displayScale > maxPostgreSQLNumericScale {
		return fmt.Errorf("decode parameters: JSON numeric scale exceeds PostgreSQL range")
	}
	if firstSignificant < 0 {
		return nil
	}
	decimalWeight := int64(len(whole)-firstSignificant-1) + exponent
	if decimalWeight > maxPostgreSQLNumericWeight {
		return fmt.Errorf("decode parameters: JSON numeric weight exceeds PostgreSQL range")
	}
	return nil
}

type Recipe struct {
	SourceMode      SourceMode       `json:"source_mode"`
	FramePolicy     FramePolicy      `json:"frame_policy"`
	MaxLongEdge     int              `json:"max_long_edge"`
	AllowUpscale    bool             `json:"allow_upscale"`
	Crop            string           `json:"crop"`
	DimensionRule   string           `json:"dimension_rule"`
	Orientation     string           `json:"orientation"`
	Color           string           `json:"color"`
	Metadata        string           `json:"metadata"`
	Alpha           string           `json:"alpha"`
	Audio           string           `json:"audio"`
	StreamSelection string           `json:"stream_selection"`
	AnimationTiming string           `json:"animation_timing"`
	AnimationLoop   string           `json:"animation_loop"`
	StillOutput     *StillOutput     `json:"still_output"`
	AnimationOutput *AnimationOutput `json:"animation_output"`
	VideoOutput     *VideoOutput     `json:"video_output"`
}

type StillOutput struct {
	Format   string `json:"format"`
	Quality  int    `json:"quality"`
	BitDepth int    `json:"bit_depth"`
}

type AnimationOutput struct {
	Format   string `json:"format"`
	Quality  int    `json:"quality"`
	BitDepth int    `json:"bit_depth"`
}

type VideoOutput struct {
	Container        string `json:"container"`
	VideoCodec       string `json:"video_codec"`
	CRF              int    `json:"crf"`
	BitDepth         int    `json:"bit_depth"`
	Chroma           string `json:"chroma"`
	AudioCodec       string `json:"audio_codec"`
	AudioBitrateKbps int    `json:"audio_bitrate_kbps"`
}

var recipeFields = []string{
	"source_mode", "frame_policy", "max_long_edge", "allow_upscale", "crop",
	"dimension_rule", "orientation", "color", "metadata", "alpha", "audio",
	"stream_selection", "animation_timing", "animation_loop", "still_output",
	"animation_output", "video_output",
}

func (r *Recipe) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, recipeFields, "still_output", "animation_output", "video_output"); err != nil {
		return err
	}
	type wire struct {
		SourceMode      SourceMode       `json:"source_mode"`
		FramePolicy     FramePolicy      `json:"frame_policy"`
		MaxLongEdge     json.RawMessage  `json:"max_long_edge"`
		AllowUpscale    bool             `json:"allow_upscale"`
		Crop            string           `json:"crop"`
		DimensionRule   string           `json:"dimension_rule"`
		Orientation     string           `json:"orientation"`
		Color           string           `json:"color"`
		Metadata        string           `json:"metadata"`
		Alpha           string           `json:"alpha"`
		Audio           string           `json:"audio"`
		StreamSelection string           `json:"stream_selection"`
		AnimationTiming string           `json:"animation_timing"`
		AnimationLoop   string           `json:"animation_loop"`
		StillOutput     *StillOutput     `json:"still_output"`
		AnimationOutput *AnimationOutput `json:"animation_output"`
		VideoOutput     *VideoOutput     `json:"video_output"`
	}
	var decoded wire
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	maxLongEdge, err := decodeJSONBInteger(decoded.MaxLongEdge)
	if err != nil {
		return fmt.Errorf("max_long_edge: %w", err)
	}
	*r = Recipe{
		SourceMode: decoded.SourceMode, FramePolicy: decoded.FramePolicy, MaxLongEdge: maxLongEdge,
		AllowUpscale: decoded.AllowUpscale, Crop: decoded.Crop, DimensionRule: decoded.DimensionRule,
		Orientation: decoded.Orientation, Color: decoded.Color, Metadata: decoded.Metadata,
		Alpha: decoded.Alpha, Audio: decoded.Audio, StreamSelection: decoded.StreamSelection,
		AnimationTiming: decoded.AnimationTiming, AnimationLoop: decoded.AnimationLoop,
		StillOutput: decoded.StillOutput, AnimationOutput: decoded.AnimationOutput, VideoOutput: decoded.VideoOutput,
	}
	return nil
}

func (o *StillOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"format", "quality", "bit_depth"}); err != nil {
		return err
	}
	type wire struct {
		Format   string          `json:"format"`
		Quality  json.RawMessage `json:"quality"`
		BitDepth json.RawMessage `json:"bit_depth"`
	}
	var decoded wire
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	quality, err := decodeJSONBInteger(decoded.Quality)
	if err != nil {
		return fmt.Errorf("quality: %w", err)
	}
	bitDepth, err := decodeJSONBInteger(decoded.BitDepth)
	if err != nil {
		return fmt.Errorf("bit_depth: %w", err)
	}
	*o = StillOutput{Format: decoded.Format, Quality: quality, BitDepth: bitDepth}
	return nil
}

func (o *AnimationOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"format", "quality", "bit_depth"}); err != nil {
		return err
	}
	type wire struct {
		Format   string          `json:"format"`
		Quality  json.RawMessage `json:"quality"`
		BitDepth json.RawMessage `json:"bit_depth"`
	}
	var decoded wire
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	quality, err := decodeJSONBInteger(decoded.Quality)
	if err != nil {
		return fmt.Errorf("quality: %w", err)
	}
	bitDepth, err := decodeJSONBInteger(decoded.BitDepth)
	if err != nil {
		return fmt.Errorf("bit_depth: %w", err)
	}
	*o = AnimationOutput{Format: decoded.Format, Quality: quality, BitDepth: bitDepth}
	return nil
}

func (o *VideoOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"container", "video_codec", "crf", "bit_depth", "chroma", "audio_codec", "audio_bitrate_kbps"}); err != nil {
		return err
	}
	type wire struct {
		Container        string          `json:"container"`
		VideoCodec       string          `json:"video_codec"`
		CRF              json.RawMessage `json:"crf"`
		BitDepth         json.RawMessage `json:"bit_depth"`
		Chroma           string          `json:"chroma"`
		AudioCodec       string          `json:"audio_codec"`
		AudioBitrateKbps json.RawMessage `json:"audio_bitrate_kbps"`
	}
	var decoded wire
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	crf, err := decodeJSONBInteger(decoded.CRF)
	if err != nil {
		return fmt.Errorf("crf: %w", err)
	}
	bitDepth, err := decodeJSONBInteger(decoded.BitDepth)
	if err != nil {
		return fmt.Errorf("bit_depth: %w", err)
	}
	audioBitrate, err := decodeJSONBInteger(decoded.AudioBitrateKbps)
	if err != nil {
		return fmt.Errorf("audio_bitrate_kbps: %w", err)
	}
	*o = VideoOutput{
		Container: decoded.Container, VideoCodec: decoded.VideoCodec, CRF: crf,
		BitDepth: bitDepth, Chroma: decoded.Chroma, AudioCodec: decoded.AudioCodec,
		AudioBitrateKbps: audioBitrate,
	}
	return nil
}

// decodeJSONBInteger accepts exactly the number forms PostgreSQL jsonb renders
// without a fractional scale. This keeps Go decoding compatible with database
// validation after jsonb has canonicalized exponent notation (for example,
// 8e0 becomes 8), while still rejecting 8.0 as non-integer-form jsonb.
func decodeJSONBInteger(number json.RawMessage) (int, error) {
	encoded := string(number)
	if encoded == "" {
		return 0, fmt.Errorf("must be an integer-form JSON number")
	}

	mantissa := encoded
	exponent := int64(0)
	if index := strings.IndexAny(encoded, "eE"); index >= 0 {
		mantissa = encoded[:index]
		parsed, err := strconv.ParseInt(encoded[index+1:], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid JSON number exponent")
		}
		exponent = parsed
	}
	if exponent < -maxPostgreSQLNumericExponent || exponent > maxPostgreSQLNumericExponent {
		return 0, fmt.Errorf("JSON number exponent exceeds PostgreSQL numeric range")
	}
	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	whole, fraction, hasFraction := strings.Cut(mantissa, ".")
	if whole == "" {
		return 0, fmt.Errorf("must be an integer-form JSON number")
	}
	if !hasFraction {
		fraction = ""
	}
	scale := int64(len(fraction)) - exponent
	if scale > 0 {
		return 0, fmt.Errorf("must have zero jsonb numeric scale")
	}
	digits := whole + fraction
	significant := strings.TrimLeft(digits, "0")
	if significant == "" {
		return 0, nil
	}
	if strings.IndexFunc(significant, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("must be an integer-form JSON number")
	}
	trailingZeroes := -scale
	if int64(len(significant))+trailingZeroes > 19 {
		return 0, fmt.Errorf("integer is outside Go int range")
	}
	if trailingZeroes > 0 {
		significant += strings.Repeat("0", int(trailingZeroes))
	}
	magnitude, err := strconv.ParseUint(significant, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("integer is outside Go int range")
	}
	var decoded int64
	if negative {
		if magnitude > uint64(1)<<63 {
			return 0, fmt.Errorf("integer is outside Go int range")
		}
		if magnitude == uint64(1)<<63 {
			decoded = -1 << 63
		} else {
			decoded = -int64(magnitude)
		}
	} else {
		if magnitude > uint64(1<<63-1) {
			return 0, fmt.Errorf("integer is outside Go int range")
		}
		decoded = int64(magnitude)
	}
	if strconv.IntSize == 32 && (decoded < -1<<31 || decoded > 1<<31-1) {
		return 0, fmt.Errorf("integer is outside Go int range")
	}
	return int(decoded), nil
}

// DecodeParameters decodes one complete, strict parameters JSON document.
func DecodeParameters(data []byte) (Parameters, error) {
	var parameters Parameters
	if err := requireObjectFields(data, []string{"evidence_status", "recipes"}); err != nil {
		return Parameters{}, err
	}
	if err := decodeStrict(data, &parameters); err != nil {
		return Parameters{}, err
	}
	return parameters, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode parameters: trailing JSON value")
		}
		return fmt.Errorf("decode parameters: trailing data: %w", err)
	}
	return nil
}

func requireObjectFields(data []byte, required []string, nullable ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode parameters object: %w", err)
	}
	if fields == nil {
		return fmt.Errorf("decode parameters object: expected object")
	}
	nullableFields := make(map[string]struct{}, len(nullable))
	for _, name := range nullable {
		nullableFields[name] = struct{}{}
	}
	allowedFields := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowedFields[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowedFields[name]; !ok {
			return fmt.Errorf("decode parameters object: unknown field %q", name)
		}
	}
	for _, name := range required {
		value, ok := fields[name]
		if !ok {
			return fmt.Errorf("decode parameters object: missing field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if _, ok := nullableFields[name]; !ok {
				return fmt.Errorf("decode parameters object: field %q must not be null", name)
			}
		}
	}
	return nil
}
