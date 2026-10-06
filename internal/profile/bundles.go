package profile

import "encoding/json"

const (
	StandardV1ID  = "60000000-0000-4000-8000-000000000001"
	ThumbnailV1ID = "60000000-0000-4000-8000-000000000002"
)

// StandardV1Parameters returns an independent copy of the standard/v1 recipe set.
func StandardV1Parameters() Parameters {
	return bundledParameters(1920, 60, true)
}

// ThumbnailV1Parameters returns an independent copy of the thumbnail/v1 recipe set.
func ThumbnailV1Parameters() Parameters {
	return bundledParameters(640, 50, false)
}

// StandardV1 returns the bundled standard/v1 draft definition.
func StandardV1() Definition {
	return bundledDefinition(StandardV1ID, "standard", StandardV1Parameters())
}

// ThumbnailV1 returns the bundled thumbnail/v1 draft definition.
func ThumbnailV1() Definition {
	return bundledDefinition(ThumbnailV1ID, "thumbnail", ThumbnailV1Parameters())
}

// BundledDefinitions returns independent standard/v1 and thumbnail/v1 definitions.
func BundledDefinitions() []Definition {
	return []Definition{StandardV1(), ThumbnailV1()}
}

func bundledDefinition(id, key string, parameters Parameters) Definition {
	encoded, err := json.Marshal(parameters)
	if err != nil {
		panic(err)
	}
	return Definition{
		ID:                      id,
		Key:                     key,
		Version:                 1,
		InputMIMETypes:          candidateMIMETypes(),
		Processor:               Processor,
		ParametersSchemaVersion: ParametersSchemaVersion,
		Parameters:              encoded,
	}
}

func bundledParameters(maxLongEdge, stillQuality int, standard bool) Parameters {
	recipes := make(map[string]Recipe, len(candidateCapabilities))
	for _, capability := range candidateCapabilities {
		recipe := baseRecipe(capability.SourceMode, maxLongEdge)
		switch capability.SourceMode {
		case SourceStill:
			recipe.FramePolicy = FrameFirst
			recipe.Audio = "none"
			recipe.StreamSelection = "not-applicable"
			recipe.AnimationTiming = "not-applicable"
			recipe.AnimationLoop = "not-applicable"
			recipe.StillOutput = still(stillQuality)
		case SourceProbeAnimation:
			recipe.Audio = "none"
			recipe.StreamSelection = "not-applicable"
			recipe.StillOutput = still(stillQuality)
			if standard {
				recipe.FramePolicy = FrameAll
				recipe.AnimationTiming = "preserve"
				recipe.AnimationLoop = "preserve"
				recipe.AnimationOutput = &AnimationOutput{Format: "animated-webp", Quality: 80, BitDepth: 8}
			} else {
				recipe.FramePolicy = FrameFirst
				recipe.AnimationTiming = "first-frame"
				recipe.AnimationLoop = "discard"
			}
		case SourceVideo:
			recipe.StreamSelection = "primary-video"
			if standard {
				recipe.FramePolicy = FrameAll
				recipe.Audio = "aac-if-present"
				recipe.Color = "normalize-bt709-tone-map-hdr"
				recipe.AnimationTiming = "not-applicable"
				recipe.AnimationLoop = "not-applicable"
				recipe.VideoOutput = &VideoOutput{
					Container: "mp4", VideoCodec: "av1", CRF: 32, BitDepth: 10,
					Chroma: "4:2:0", AudioCodec: "aac", AudioBitrateKbps: 128,
				}
			} else {
				recipe.FramePolicy = FrameFirst
				recipe.Audio = "discard"
				recipe.AnimationTiming = "first-frame"
				recipe.AnimationLoop = "discard"
				recipe.StillOutput = still(stillQuality)
			}
		}
		recipes[capability.MIMEType] = recipe
	}
	return Parameters{EvidenceStatus: EvidenceProvisional, Recipes: recipes}
}

func baseRecipe(sourceMode SourceMode, maxLongEdge int) Recipe {
	dimensionRule := "preserve-aspect-no-crop-no-upscale-round-nearest"
	if sourceMode == SourceVideo {
		dimensionRule = "preserve-aspect-no-crop-no-upscale-even-round-down"
	}
	return Recipe{
		SourceMode: sourceMode, MaxLongEdge: maxLongEdge, AllowUpscale: false,
		Crop: "none", DimensionRule: dimensionRule,
		Orientation: "apply", Color: "normalize-srgb-tone-map-hdr",
		Metadata: "strip-after-normalization-keep-color-tags", Alpha: "preserve",
	}
}

func still(quality int) *StillOutput {
	return &StillOutput{Format: "avif", Quality: quality, BitDepth: 8}
}

func candidateMIMETypes() []string {
	result := make([]string, len(candidateCapabilities))
	for index, capability := range candidateCapabilities {
		result[index] = capability.MIMEType
	}
	return result
}
