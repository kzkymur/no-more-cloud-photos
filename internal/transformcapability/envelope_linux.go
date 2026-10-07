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

// Envelope is the immutable result of validating one startup snapshot. Its
// profile IDs can only be produced by Validate and are bound to every claim.
type Envelope struct {
	profileIDs []string
	validated  bool
}

// ProfileIDs returns a copy for repository binding.
func (e Envelope) ProfileIDs() []string {
	return slices.Clone(e.profileIDs)
}

// Validated distinguishes an empty validated snapshot from a zero Envelope.
func (e Envelope) Validated() bool {
	return e.validated
}

// Validate requires the reported capabilities to cover every processing branch
// reachable from the supplied profile definitions.
func Validate(definitions []profile.Definition, still stillprocessor.Capabilities, animation animationprocessor.Capabilities, video videoprocessor.Capabilities) error {
	_, err := ValidateEnvelope(definitions, still, animation, video)
	return err
}

// ValidateEnvelope validates a startup snapshot and binds its exact profile IDs.
func ValidateEnvelope(definitions []profile.Definition, still stillprocessor.Capabilities, animation animationprocessor.Capabilities, video videoprocessor.Capabilities) (Envelope, error) {
	requiredStillDecoders := make(map[string]struct{})
	requiredAnimationDecoders := make(map[string]struct{})
	requiredAnimationEncoders := make(map[string]struct{})
	requiredVideoDecoders := make(map[string]struct{})
	requiredVideoOutputs := make(map[string]struct{})

	profileIDs := make([]string, 0, len(definitions))
	seenProfileIDs := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if err := profile.ValidateDraft(definition); err != nil {
			return Envelope{}, incompatible("profile %q version %d is invalid", definition.Key, definition.Version)
		}
		if _, duplicate := seenProfileIDs[definition.ID]; duplicate {
			return Envelope{}, incompatible("profile ID %q is duplicated", definition.ID)
		}
		seenProfileIDs[definition.ID] = struct{}{}
		profileIDs = append(profileIDs, definition.ID)
		parameters, err := profile.DecodeParameters(definition.Parameters)
		if err != nil {
			return Envelope{}, incompatible("profile %q version %d parameters are invalid", definition.Key, definition.Version)
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
				return Envelope{}, incompatible("profile %q has contradictory source mode", definition.Key)
			}
		}
	}

	if still.ProtocolVersion != stillprocessor.ProtocolVersion || still.Threads != stillprocessor.RequiredThreads || still.AVIFEncoder != "aom" || still.ICCSHA256 == "" || still.HelperVersion == "" || len(still.LibraryVersions) == 0 {
		return Envelope{}, incompatible("still processor fixed capabilities are contradictory")
	}
	if animation.ProtocolVersion != animationprocessor.ProtocolVersion || animation.Threads != animationprocessor.RequiredThreads || animation.ICCSHA256 == "" || animation.HelperVersion == "" || len(animation.LibraryVersions) == 0 || animation.BuildManifest != animationprocessor.BuildManifest {
		return Envelope{}, incompatible("animation processor fixed capabilities are contradictory")
	}
	if video.ProtocolVersion != videoprocessor.ProtocolVersion || video.Threads != videoprocessor.RequiredThreads || video.VideoEncoder != "libsvtav1" || video.AudioEncoder != "aac-lc" || video.VideoMuxer != "mp4" || video.ToneMap != "zscale+hable" || video.ICCSHA256 == "" || video.HelperVersion == "" || len(video.LibraryVersions) == 0 || video.BuildManifest != videoprocessor.BuildManifest {
		return Envelope{}, incompatible("video processor fixed capabilities are contradictory")
	}
	if still.ICCSHA256 != animation.ICCSHA256 || still.ICCSHA256 != video.ICCSHA256 {
		return Envelope{}, incompatible("processor ICC capabilities contradict each other")
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
				return Envelope{}, incompatible("%s are %v, missing required %q", check.name, check.actual, required)
			}
		}
	}
	return Envelope{profileIDs: profileIDs, validated: true}, nil
}

func incompatible(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrIncompatible, fmt.Sprintf(format, arguments...))
}
