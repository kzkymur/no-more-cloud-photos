//go:build linux

package transformcapability

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
)

const testICC = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateCapabilityEnvelopeMatrix(t *testing.T) {
	definitions := profile.BundledDefinitions()
	still, animation, video := exactCapabilities()
	tests := []struct {
		name   string
		mutate func(*stillprocessor.Capabilities, *animationprocessor.Capabilities, *videoprocessor.Capabilities)
	}{
		{name: "exact"},
		{name: "still helper only DNG", mutate: func(s *stillprocessor.Capabilities, _ *animationprocessor.Capabilities, _ *videoprocessor.Capabilities) {
			s.DecoderMIMETypes = []string{"image/dng"}
		}},
		{name: "missing animation decoder", mutate: func(_ *stillprocessor.Capabilities, a *animationprocessor.Capabilities, _ *videoprocessor.Capabilities) {
			a.DecoderMIMETypes = []string{"image/webp"}
		}},
		{name: "missing animation encoder", mutate: func(_ *stillprocessor.Capabilities, a *animationprocessor.Capabilities, _ *videoprocessor.Capabilities) {
			a.Encoders = []string{"avif"}
		}},
		{name: "missing video decoder", mutate: func(_ *stillprocessor.Capabilities, _ *animationprocessor.Capabilities, v *videoprocessor.Capabilities) {
			v.DecoderMIMETypes = []string{"video/mp4"}
		}},
		{name: "missing video output kind", mutate: func(_ *stillprocessor.Capabilities, _ *animationprocessor.Capabilities, v *videoprocessor.Capabilities) {
			v.OutputKinds = []string{"mp4-av1"}
		}},
		{name: "contradictory ICC", mutate: func(_ *stillprocessor.Capabilities, a *animationprocessor.Capabilities, _ *videoprocessor.Capabilities) {
			a.ICCSHA256 = strings.Repeat("b", 64)
		}},
		{name: "contradictory encoder", mutate: func(_ *stillprocessor.Capabilities, _ *animationprocessor.Capabilities, v *videoprocessor.Capabilities) {
			v.VideoEncoder = "other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, a, v := cloneCapabilities(still, animation, video)
			if test.mutate != nil {
				test.mutate(&s, &a, &v)
			}
			err := Validate(definitions, s, a, v)
			if test.mutate == nil && err != nil {
				t.Fatalf("Validate() exact capabilities error = %v", err)
			}
			if test.mutate != nil && !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Validate() error = %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestValidateAllowsHelperCapabilitySuperset(t *testing.T) {
	definition := onlyRecipe(t, profile.StandardV1(), "image/jpeg")
	still, animation, video := exactCapabilities()
	if err := Validate([]profile.Definition{definition}, still, animation, video); err != nil {
		t.Fatalf("Validate() capability superset error = %v", err)
	}
}

func TestValidateEnvelopeHandlesEmptyAndRejectsDuplicateAndInvalidProfileIDs(t *testing.T) {
	still, animation, video := exactCapabilities()
	empty, err := ValidateEnvelope(nil, still, animation, video)
	if err != nil || !empty.Validated() || len(empty.ProfileIDs()) != 0 {
		t.Fatalf("empty definitions envelope = %#v, %v", empty, err)
	}
	definition := profile.StandardV1()
	if _, err := ValidateEnvelope([]profile.Definition{definition, definition}, still, animation, video); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("duplicate profile ID error = %v", err)
	}
	definition.ID = "not-a-uuid"
	if _, err := ValidateEnvelope([]profile.Definition{definition}, still, animation, video); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("invalid profile ID error = %v", err)
	}
}

func TestEnvelopeProfileIDsReturnsCopy(t *testing.T) {
	still, animation, video := exactCapabilities()
	envelope, err := ValidateEnvelope(profile.BundledDefinitions(), still, animation, video)
	if err != nil {
		t.Fatal(err)
	}
	ids := envelope.ProfileIDs()
	first := ids[0]
	ids[0] = "mutated"
	if got := envelope.ProfileIDs()[0]; got != first {
		t.Fatalf("envelope profile IDs were mutable: %q", got)
	}
}

func TestValidateRequiresStaticWebPStillBranch(t *testing.T) {
	definition := onlyRecipe(t, profile.StandardV1(), "image/webp")
	still, animation, video := fixedCapabilities()
	still.DecoderMIMETypes = []string{"image/webp"}
	animation.DecoderMIMETypes = []string{"image/webp"}
	animation.Encoders = []string{"animated-webp"}
	video.DecoderMIMETypes = nil
	video.OutputKinds = nil
	if err := Validate([]profile.Definition{definition}, still, animation, video); err != nil {
		t.Fatalf("Validate() exact static WebP branch error = %v", err)
	}
	still.DecoderMIMETypes = nil
	if err := Validate([]profile.Definition{definition}, still, animation, video); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Validate() without static WebP decoder error = %v", err)
	}
}

func TestValidateRejectsInvalidProfileDefinitionAndSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*profile.Definition, *profile.Parameters)
	}{
		{name: "processor", mutate: func(definition *profile.Definition, _ *profile.Parameters) { definition.Processor = "other" }},
		{name: "schema", mutate: func(definition *profile.Definition, _ *profile.Parameters) { definition.ParametersSchemaVersion = 2 }},
		{name: "still quality", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["image/jpeg"].StillOutput.Quality = 0
		}},
		{name: "still bit depth", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["image/jpeg"].StillOutput.BitDepth = 10
		}},
		{name: "animation quality", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["image/gif"].AnimationOutput.Quality = 0
		}},
		{name: "animation bit depth", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["image/gif"].AnimationOutput.BitDepth = 10
		}},
		{name: "video CRF", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["video/mp4"].VideoOutput.CRF = 64
		}},
		{name: "video bit depth", mutate: func(_ *profile.Definition, parameters *profile.Parameters) {
			parameters.Recipes["video/mp4"].VideoOutput.BitDepth = 8
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := profile.StandardV1()
			parameters, err := profile.DecodeParameters(definition.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&definition, &parameters)
			definition.Parameters, err = json.Marshal(parameters)
			if err != nil {
				t.Fatal(err)
			}
			still, animation, video := exactCapabilities()
			if err := Validate([]profile.Definition{definition}, still, animation, video); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func exactCapabilities() (stillprocessor.Capabilities, animationprocessor.Capabilities, videoprocessor.Capabilities) {
	stillMIMEs := []string{
		"image/bmp", "image/dng", "image/heic", "image/heif", "image/jpeg", "image/png", "image/webp",
		"image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef",
		"image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw",
	}
	still, animation, video := fixedCapabilities()
	still.DecoderMIMETypes = stillMIMEs
	animation.DecoderMIMETypes = []string{"image/gif", "image/webp"}
	animation.Encoders = []string{"animated-webp", "avif"}
	video.DecoderMIMETypes = []string{"video/mp4", "video/quicktime"}
	video.OutputKinds = []string{"first-frame-avif", "mp4-av1"}
	return still, animation, video
}

func fixedCapabilities() (stillprocessor.Capabilities, animationprocessor.Capabilities, videoprocessor.Capabilities) {
	still := stillprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "still-1", LibraryVersions: map[string]string{"libvips": "test"}, AVIFEncoder: "aom", ICCSHA256: testICC, Threads: 1}
	animation := animationprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "animation-1", LibraryVersions: map[string]string{"libwebp": "test"}, ICCSHA256: testICC, Threads: 1, BuildManifest: animationprocessor.BuildManifest}
	video := videoprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "video-1", LibraryVersions: map[string]string{"ffmpeg": "test"}, VideoEncoder: "libsvtav1", AudioEncoder: "aac-lc", VideoMuxer: "mp4", ToneMap: "zscale+hable", ICCSHA256: testICC, Threads: 1, BuildManifest: videoprocessor.BuildManifest}
	return still, animation, video
}

func cloneCapabilities(still stillprocessor.Capabilities, animation animationprocessor.Capabilities, video videoprocessor.Capabilities) (stillprocessor.Capabilities, animationprocessor.Capabilities, videoprocessor.Capabilities) {
	still.DecoderMIMETypes = slices.Clone(still.DecoderMIMETypes)
	animation.DecoderMIMETypes = slices.Clone(animation.DecoderMIMETypes)
	animation.Encoders = slices.Clone(animation.Encoders)
	video.DecoderMIMETypes = slices.Clone(video.DecoderMIMETypes)
	video.OutputKinds = slices.Clone(video.OutputKinds)
	return still, animation, video
}

func onlyRecipe(t *testing.T, definition profile.Definition, mimeType string) profile.Definition {
	t.Helper()
	parameters, err := profile.DecodeParameters(definition.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	parameters.Recipes = map[string]profile.Recipe{mimeType: parameters.Recipes[mimeType]}
	definition.InputMIMETypes = []string{mimeType}
	definition.Parameters, err = json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
