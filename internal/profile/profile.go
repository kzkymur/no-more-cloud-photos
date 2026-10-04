// Package profile defines media processing profiles and their capabilities.
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	type plain Recipe
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	*r = Recipe(decoded)
	return nil
}

func (o *StillOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"format", "quality", "bit_depth"}); err != nil {
		return err
	}
	type plain StillOutput
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	*o = StillOutput(decoded)
	return nil
}

func (o *AnimationOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"format", "quality", "bit_depth"}); err != nil {
		return err
	}
	type plain AnimationOutput
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	*o = AnimationOutput(decoded)
	return nil
}

func (o *VideoOutput) UnmarshalJSON(data []byte) error {
	if err := requireObjectFields(data, []string{"container", "video_codec", "crf", "bit_depth", "chroma", "audio_codec", "audio_bitrate_kbps"}); err != nil {
		return err
	}
	type plain VideoOutput
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	*o = VideoOutput(decoded)
	return nil
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
