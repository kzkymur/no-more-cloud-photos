// Package transformcapability binds claimable profile recipes to the runtime
// capabilities reported by the three transform helpers.
package transformcapability

import (
	"errors"
	"fmt"
	"slices"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
)

var ErrIncompatible = errors.New("claimable profiles exceed processor capabilities")

// Validate requires the reported capability envelope to exactly match every
// processing branch reachable from the supplied profile definitions.
func Validate(definitions []profile.Definition, still stillprocessor.Capabilities, animation animationprocessor.Capabilities, video videoprocessor.Capabilities) error {
	requiredStillDecoders := make(map[string]struct{})
	requiredAnimationDecoders := make(map[string]struct{})
	requiredAnimationEncoders := make(map[string]struct{})
	requiredVideoDecoders := make(map[string]struct{})
	requiredVideoOutputs := make(map[string]struct{})

	for _, definition := range definitions {
		if err := profile.ValidateDraft(definition); err != nil {
			return incompatible("profile %q version %d is invalid", definition.Key, definition.Version)
		}
		parameters, err := profile.DecodeParameters(definition.Parameters)
		if err != nil {
			return incompatible("profile %q version %d parameters are invalid", definition.Key, definition.Version)
		}
		for _, mimeType := range definition.InputMIMETypes {
			recipe := parameters.Recipes[mimeType]
			switch recipe.SourceMode {
			case profile.SourceStill:
				requiredStillDecoders[mimeType] = struct{}{}
			case profile.SourceProbeAnimation:
				requiredAnimationDecoders[mimeType] = struct{}{}
				if recipe.FramePolicy == profile.FrameAll {
					requiredAnimationEncoders["animated-webp"] = struct{}{}
				} else {
					requiredAnimationEncoders["avif"] = struct{}{}
				}
				// WebP is inspected first and a one-frame input is routed through
				// the still processor regardless of the profile frame policy.
				if mimeType == "image/webp" {
					requiredStillDecoders[mimeType] = struct{}{}
				}
			case profile.SourceVideo:
				requiredVideoDecoders[mimeType] = struct{}{}
				if recipe.FramePolicy == profile.FrameAll {
					requiredVideoOutputs["mp4-av1"] = struct{}{}
				} else {
					requiredVideoOutputs["first-frame-avif"] = struct{}{}
				}
			default:
				return incompatible("profile %q has contradictory source mode", definition.Key)
			}
		}
	}

	if still.ProtocolVersion != stillprocessor.ProtocolVersion || still.Threads != stillprocessor.RequiredThreads || still.AVIFEncoder != "aom" || still.ICCSHA256 == "" || still.HelperVersion == "" || len(still.LibraryVersions) == 0 {
		return incompatible("still processor fixed capabilities are contradictory")
	}
	if animation.ProtocolVersion != animationprocessor.ProtocolVersion || animation.Threads != animationprocessor.RequiredThreads || animation.ICCSHA256 == "" || animation.HelperVersion == "" || len(animation.LibraryVersions) == 0 || animation.BuildManifest != animationprocessor.BuildManifest {
		return incompatible("animation processor fixed capabilities are contradictory")
	}
	if video.ProtocolVersion != videoprocessor.ProtocolVersion || video.Threads != videoprocessor.RequiredThreads || video.VideoEncoder != "libsvtav1" || video.AudioEncoder != "aac-lc" || video.VideoMuxer != "mp4" || video.ToneMap != "zscale+hable" || video.ICCSHA256 == "" || video.HelperVersion == "" || len(video.LibraryVersions) == 0 || video.BuildManifest != videoprocessor.BuildManifest {
		return incompatible("video processor fixed capabilities are contradictory")
	}
	if still.ICCSHA256 != animation.ICCSHA256 || still.ICCSHA256 != video.ICCSHA256 {
		return incompatible("processor ICC capabilities contradict each other")
	}

	checks := []struct {
		name     string
		actual   []string
		required map[string]struct{}
	}{
		{name: "still decoder MIME types", actual: still.DecoderMIMETypes, required: requiredStillDecoders},
		{name: "animation decoder MIME types", actual: animation.DecoderMIMETypes, required: requiredAnimationDecoders},
		{name: "animation encoders", actual: animation.Encoders, required: requiredAnimationEncoders},
		{name: "video decoder MIME types", actual: video.DecoderMIMETypes, required: requiredVideoDecoders},
		{name: "video output kinds", actual: video.OutputKinds, required: requiredVideoOutputs},
	}
	for _, check := range checks {
		for required := range check.required {
			if !slices.Contains(check.actual, required) {
				return incompatible("%s are %v, missing required %q", check.name, check.actual, required)
			}
		}
	}
	return nil
}

func incompatible(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrIncompatible, fmt.Sprintf(format, arguments...))
}
