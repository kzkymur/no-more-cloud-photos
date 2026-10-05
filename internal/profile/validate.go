package profile

import (
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidateDraft validates a definition against the provisional candidate registry.
func ValidateDraft(definition Definition) error {
	_, err := validateDefinition(definition, CandidateRegistry(), false)
	return err
}

// ValidateActivation additionally requires every recipe output to fit a certified option envelope.
func ValidateActivation(definition Definition, registry Registry) error {
	parameters, err := validateDefinition(definition, registry, true)
	if err != nil {
		return err
	}
	for _, mimeType := range definition.InputMIMETypes {
		recipe := parameters.Recipes[mimeType]
		outputs := recipeOutputs(recipe)
		for _, output := range outputs {
			if !registry.certifies(mimeType, recipe.SourceMode, output.kind, recipe.MaxLongEdge, output.setting) {
				return fmt.Errorf("input MIME type %q output kind %q is not certified for edge %d and setting %d", mimeType, output.kind, recipe.MaxLongEdge, output.setting)
			}
		}
	}
	return nil
}

func validateDefinition(definition Definition, registry Registry, activation bool) (Parameters, error) {
	if !validUUIDv4(definition.ID) {
		return Parameters{}, fmt.Errorf("ID must be a canonical UUIDv4")
	}
	if !keyPattern.MatchString(definition.Key) {
		return Parameters{}, fmt.Errorf("key must match %s", keyPattern)
	}
	if definition.Version < 1 || definition.Version > math.MaxInt32 {
		return Parameters{}, fmt.Errorf("version must be between 1 and %d", math.MaxInt32)
	}
	if definition.Processor != Processor {
		return Parameters{}, fmt.Errorf("unknown processor %q", definition.Processor)
	}
	if definition.ParametersSchemaVersion != ParametersSchemaVersion {
		return Parameters{}, fmt.Errorf("unknown parameters schema version %d", definition.ParametersSchemaVersion)
	}
	if len(definition.InputMIMETypes) == 0 {
		return Parameters{}, fmt.Errorf("input MIME types must not be empty")
	}

	seen := make(map[string]struct{}, len(definition.InputMIMETypes))
	for _, mimeType := range definition.InputMIMETypes {
		if mimeType == "" || strings.Contains(mimeType, "*") {
			return Parameters{}, fmt.Errorf("input MIME type %q is not canonical", mimeType)
		}
		if _, duplicate := seen[mimeType]; duplicate {
			return Parameters{}, fmt.Errorf("input MIME type %q is duplicated", mimeType)
		}
		seen[mimeType] = struct{}{}
		if _, ok := registry.capability(mimeType); !ok {
			if activation {
				return Parameters{}, fmt.Errorf("input MIME type %q is not registered", mimeType)
			}
			return Parameters{}, fmt.Errorf("input MIME type %q is not a candidate", mimeType)
		}
	}

	parameters, err := DecodeParameters(definition.Parameters)
	if err != nil {
		return Parameters{}, err
	}
	if parameters.EvidenceStatus != EvidenceProvisional {
		return Parameters{}, fmt.Errorf("evidence_status must be %q", EvidenceProvisional)
	}
	if len(parameters.Recipes) != len(seen) {
		return Parameters{}, fmt.Errorf("recipes must exactly cover input MIME types")
	}
	for mimeType := range seen {
		recipe, ok := parameters.Recipes[mimeType]
		if !ok {
			return Parameters{}, fmt.Errorf("recipe for input MIME type %q is missing", mimeType)
		}
		capability, _ := registry.capability(mimeType)
		if recipe.SourceMode != capability.SourceMode {
			return Parameters{}, fmt.Errorf("recipe %q source mode %q does not match registry mode %q", mimeType, recipe.SourceMode, capability.SourceMode)
		}
		if err := validateRecipe(mimeType, recipe); err != nil {
			return Parameters{}, err
		}
	}
	for mimeType := range parameters.Recipes {
		if _, ok := seen[mimeType]; !ok {
			return Parameters{}, fmt.Errorf("recipe %q has no matching input MIME type", mimeType)
		}
	}
	return parameters, nil
}

func validateRecipe(mimeType string, recipe Recipe) error {
	invalid := func(message string, arguments ...any) error {
		return fmt.Errorf("recipe %q: %s", mimeType, fmt.Sprintf(message, arguments...))
	}
	if recipe.MaxLongEdge < 1 || recipe.MaxLongEdge > math.MaxInt32 {
		return invalid("max_long_edge must be between 1 and %d", math.MaxInt32)
	}
	if recipe.AllowUpscale {
		return invalid("allow_upscale must be false")
	}
	expectedDimensionRule := "preserve-aspect-no-crop-no-upscale-round-nearest"
	if recipe.SourceMode == SourceVideo {
		expectedDimensionRule = "preserve-aspect-no-crop-no-upscale-even-round-down"
	}
	if recipe.Crop != "none" || recipe.DimensionRule != expectedDimensionRule || recipe.Orientation != "apply" || recipe.Metadata != "strip-after-normalization-keep-color-tags" || recipe.Alpha != "preserve" {
		return invalid("common normalization values are invalid")
	}
	if err := validateStillOutput(recipe.StillOutput); err != nil {
		return invalid("%v", err)
	}
	if err := validateAnimationOutput(recipe.AnimationOutput); err != nil {
		return invalid("%v", err)
	}
	if err := validateVideoOutput(recipe.VideoOutput); err != nil {
		return invalid("%v", err)
	}

	switch recipe.SourceMode {
	case SourceStill:
		if recipe.FramePolicy != FrameFirst || recipe.Color != "normalize-srgb-tone-map-hdr" || recipe.Audio != "none" || recipe.StreamSelection != "not-applicable" || recipe.AnimationTiming != "not-applicable" || recipe.AnimationLoop != "not-applicable" || recipe.StillOutput == nil || recipe.AnimationOutput != nil || recipe.VideoOutput != nil {
			return invalid("invalid still output combination")
		}
	case SourceProbeAnimation:
		if recipe.Color != "normalize-srgb-tone-map-hdr" || recipe.Audio != "none" || recipe.StreamSelection != "not-applicable" || recipe.StillOutput == nil || recipe.VideoOutput != nil {
			return invalid("invalid probe-animation output combination")
		}
		if recipe.FramePolicy == FrameAll && (recipe.AnimationTiming != "preserve" || recipe.AnimationLoop != "preserve" || recipe.AnimationOutput == nil) {
			return invalid("frame policy all requires animation output")
		}
		if recipe.FramePolicy == FrameFirst && (recipe.AnimationTiming != "first-frame" || recipe.AnimationLoop != "discard" || recipe.AnimationOutput != nil) {
			return invalid("frame policy first forbids animation output")
		}
		if recipe.FramePolicy != FrameAll && recipe.FramePolicy != FrameFirst {
			return invalid("invalid frame policy %q", recipe.FramePolicy)
		}
	case SourceVideo:
		allFrames := recipe.FramePolicy == FrameAll && recipe.Color == "normalize-bt709-tone-map-hdr" && recipe.Audio == "aac-if-present" && recipe.StreamSelection == "primary-video" && recipe.AnimationTiming == "not-applicable" && recipe.AnimationLoop == "not-applicable" && recipe.VideoOutput != nil && recipe.StillOutput == nil && recipe.AnimationOutput == nil
		firstFrame := recipe.FramePolicy == FrameFirst && recipe.Color == "normalize-srgb-tone-map-hdr" && recipe.Audio == "discard" && recipe.StreamSelection == "primary-video" && recipe.AnimationTiming == "first-frame" && recipe.AnimationLoop == "discard" && recipe.StillOutput != nil && recipe.AnimationOutput == nil && recipe.VideoOutput == nil
		if !allFrames && !firstFrame {
			return invalid("invalid video output combination")
		}
	default:
		return invalid("invalid source mode %q", recipe.SourceMode)
	}
	return nil
}

type recipeOutput struct {
	kind    OutputKind
	setting int
}

func recipeOutputs(recipe Recipe) []recipeOutput {
	outputs := make([]recipeOutput, 0, 2)
	if recipe.StillOutput != nil {
		outputs = append(outputs, recipeOutput{kind: OutputStillAVIF, setting: recipe.StillOutput.Quality})
	}
	if recipe.AnimationOutput != nil {
		outputs = append(outputs, recipeOutput{kind: OutputAnimationWebP, setting: recipe.AnimationOutput.Quality})
	}
	if recipe.VideoOutput != nil {
		outputs = append(outputs, recipeOutput{kind: OutputVideoAV1, setting: recipe.VideoOutput.CRF})
	}
	return outputs
}

func validateStillOutput(output *StillOutput) error {
	if output == nil {
		return nil
	}
	if output.Format != "avif" || output.Quality < 1 || output.Quality > 100 || output.BitDepth != 8 {
		return fmt.Errorf("invalid still output")
	}
	return nil
}

func validateAnimationOutput(output *AnimationOutput) error {
	if output == nil {
		return nil
	}
	if output.Format != "animated-webp" || output.Quality < 1 || output.Quality > 100 || output.BitDepth != 8 {
		return fmt.Errorf("invalid animation output")
	}
	return nil
}

func validateVideoOutput(output *VideoOutput) error {
	if output == nil {
		return nil
	}
	if output.Container != "mp4" || output.VideoCodec != "av1" || output.CRF < 0 || output.CRF > 63 || output.BitDepth != 10 || output.Chroma != "4:2:0" || output.AudioCodec != "aac" || output.AudioBitrateKbps != 128 {
		return fmt.Errorf("invalid video output")
	}
	return nil
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded := make([]byte, 16)
	if _, err := hex.Decode(decoded, []byte(compact)); err != nil {
		return false
	}
	return hex.EncodeToString(decoded) == compact
}
