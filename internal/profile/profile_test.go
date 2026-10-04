package profile

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

var expectedMIMETypes = []string{
	"image/bmp",
	"image/dng",
	"image/gif",
	"image/heic",
	"image/heif",
	"image/jpeg",
	"image/png",
	"image/webp",
	"image/x-canon-cr2",
	"image/x-canon-cr3",
	"image/x-fuji-raf",
	"image/x-nikon-nef",
	"image/x-olympus-orf",
	"image/x-panasonic-rw2",
	"image/x-sony-arw",
	"video/mp4",
	"video/quicktime",
}

func TestBundledDefinitionsAreValidDrafts(t *testing.T) {
	definitions := BundledDefinitions()
	if len(definitions) != 2 {
		t.Fatalf("BundledDefinitions() length = %d, want 2", len(definitions))
	}
	for _, definition := range definitions {
		t.Run(definition.Key, func(t *testing.T) {
			if err := ValidateDraft(definition); err != nil {
				t.Fatalf("ValidateDraft() error = %v", err)
			}
			if definition.Version != 1 || definition.Processor != Processor || definition.ParametersSchemaVersion != 1 {
				t.Fatalf("definition metadata = %#v", definition)
			}
			if !reflect.DeepEqual(definition.InputMIMETypes, expectedMIMETypes) {
				t.Fatalf("InputMIMETypes = %#v", definition.InputMIMETypes)
			}
			parameters := decodeForTest(t, definition)
			if parameters.EvidenceStatus != EvidenceProvisional {
				t.Fatalf("EvidenceStatus = %q", parameters.EvidenceStatus)
			}
		})
	}
	if definitions[0].ID != StandardV1ID || definitions[0].Key != "standard" {
		t.Fatalf("standard definition identity = %q/%q", definitions[0].ID, definitions[0].Key)
	}
	if definitions[1].ID != ThumbnailV1ID || definitions[1].Key != "thumbnail" {
		t.Fatalf("thumbnail definition identity = %q/%q", definitions[1].ID, definitions[1].Key)
	}
}

func TestBundledExactRecipeValues(t *testing.T) {
	standard := StandardV1Parameters()
	thumbnail := ThumbnailV1Parameters()
	for _, mimeType := range expectedMIMETypes {
		s := standard.Recipes[mimeType]
		th := thumbnail.Recipes[mimeType]
		if s.MaxLongEdge != 1920 || th.MaxLongEdge != 640 {
			t.Errorf("%s max edges = %d/%d", mimeType, s.MaxLongEdge, th.MaxLongEdge)
		}
		for name, recipe := range map[string]Recipe{"standard": s, "thumbnail": th} {
			if recipe.DimensionRule != "preserve-aspect-no-crop-no-upscale-even-round-down" || recipe.Metadata != "strip-after-normalization-keep-color-tags" {
				t.Errorf("%s %s normalization fields = %#v", mimeType, name, recipe)
			}
		}
		switch s.SourceMode {
		case SourceStill:
			assertRouting(t, mimeType+" standard", s, "normalize-srgb-tone-map-hdr", "not-applicable", "not-applicable", "not-applicable")
			assertRouting(t, mimeType+" thumbnail", th, "normalize-srgb-tone-map-hdr", "not-applicable", "not-applicable", "not-applicable")
			assertStill(t, mimeType+" standard", s.StillOutput, 60)
			assertStill(t, mimeType+" thumbnail", th.StillOutput, 50)
		case SourceProbeAnimation:
			assertRouting(t, mimeType+" standard", s, "normalize-srgb-tone-map-hdr", "not-applicable", "preserve", "preserve")
			assertRouting(t, mimeType+" thumbnail", th, "normalize-srgb-tone-map-hdr", "not-applicable", "first-frame", "discard")
			assertStill(t, mimeType+" standard", s.StillOutput, 60)
			if s.FramePolicy != FrameAll || s.AnimationOutput == nil || s.AnimationOutput.Format != "animated-webp" || s.AnimationOutput.Quality != 80 || s.AnimationOutput.BitDepth != 8 {
				t.Errorf("%s standard animation = %#v", mimeType, s)
			}
			assertStill(t, mimeType+" thumbnail", th.StillOutput, 50)
			if th.FramePolicy != FrameFirst || th.AnimationOutput != nil {
				t.Errorf("%s thumbnail animation = %#v", mimeType, th)
			}
		case SourceVideo:
			assertRouting(t, mimeType+" standard", s, "normalize-bt709-tone-map-hdr", "primary-video", "not-applicable", "not-applicable")
			assertRouting(t, mimeType+" thumbnail", th, "normalize-srgb-tone-map-hdr", "primary-video", "first-frame", "discard")
			video := s.VideoOutput
			if s.FramePolicy != FrameAll || s.Audio != "aac-if-present" || video == nil || video.Container != "mp4" || video.VideoCodec != "av1" || video.CRF != 32 || video.BitDepth != 10 || video.Chroma != "4:2:0" || video.AudioCodec != "aac" || video.AudioBitrateKbps != 128 {
				t.Errorf("%s standard video = %#v", mimeType, s)
			}
			if th.FramePolicy != FrameFirst || th.Audio != "discard" || th.VideoOutput != nil {
				t.Errorf("%s thumbnail video = %#v", mimeType, th)
			}
			assertStill(t, mimeType+" thumbnail", th.StillOutput, 50)
		}
	}
}

func TestCustomKeyUsesSameValidation(t *testing.T) {
	definition := StandardV1()
	definition.ID = "12345678-1234-4234-9234-123456789abc"
	definition.Key = "custom_profile-1"
	definition.Version = 9
	if err := ValidateDraft(definition); err != nil {
		t.Fatalf("ValidateDraft(custom) error = %v", err)
	}
}

func TestDefinitionVersionMatchesPostgreSQLIntegerRange(t *testing.T) {
	definition := StandardV1()
	definition.Version = math.MaxInt32
	if err := ValidateDraft(definition); err != nil {
		t.Fatalf("ValidateDraft(max int32) error = %v", err)
	}
	definition.Version = math.MaxInt32 + 1
	if err := ValidateDraft(definition); err == nil {
		t.Fatal("ValidateDraft(max int32 + 1) error = nil")
	}
}

func TestDefinitionValidationRejectsInvalidMetadataAndMIMEs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Definition)
	}{
		{name: "invalid UUID", mutate: func(d *Definition) { d.ID = "not-a-uuid" }},
		{name: "non-v4 UUID", mutate: func(d *Definition) { d.ID = "60000000-0000-3000-8000-000000000001" }},
		{name: "invalid key", mutate: func(d *Definition) { d.Key = "Custom" }},
		{name: "long key", mutate: func(d *Definition) { d.Key = "a" + strings.Repeat("b", 64) }},
		{name: "zero version", mutate: func(d *Definition) { d.Version = 0 }},
		{name: "empty MIME", mutate: func(d *Definition) { d.InputMIMETypes = nil }},
		{name: "duplicate MIME", mutate: func(d *Definition) { d.InputMIMETypes = append(d.InputMIMETypes, d.InputMIMETypes[0]) }},
		{name: "wildcard MIME", mutate: func(d *Definition) { d.InputMIMETypes[0] = "image/*" }},
		{name: "unknown MIME", mutate: func(d *Definition) { d.InputMIMETypes[0] = "image/tiff" }},
		{name: "noncanonical MIME", mutate: func(d *Definition) { d.InputMIMETypes[0] = "IMAGE/BMP" }},
		{name: "unknown processor", mutate: func(d *Definition) { d.Processor = "other" }},
		{name: "unknown schema", mutate: func(d *Definition) { d.ParametersSchemaVersion = 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := StandardV1()
			test.mutate(&definition)
			if err := ValidateDraft(definition); err == nil {
				t.Fatal("ValidateDraft() error = nil")
			}
		})
	}
}

func TestRecipeCoverageIsExact(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		definition, parameters := mutableStandard(t)
		delete(parameters.Recipes, definition.InputMIMETypes[0])
		definition.Parameters = encodeForTest(t, parameters)
		if err := ValidateDraft(definition); err == nil {
			t.Fatal("ValidateDraft() error = nil")
		}
	})
	t.Run("extra", func(t *testing.T) {
		definition, parameters := mutableStandard(t)
		parameters.Recipes["image/tiff"] = parameters.Recipes["image/jpeg"]
		definition.Parameters = encodeForTest(t, parameters)
		if err := ValidateDraft(definition); err == nil {
			t.Fatal("ValidateDraft() error = nil")
		}
	})
}

func TestStrictJSONRejectsUnknownMissingAndTrailingData(t *testing.T) {
	validRecipe, err := json.Marshal(StandardV1Parameters().Recipes["image/jpeg"])
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{
		`{"evidence_status":"provisional-unverified","recipes":{},"unknown":true}`,
		`{"recipes":{}}`,
		`{"evidence_status":"provisional-unverified","recipes":{}} {}`,
		`{"evidence_status":"provisional-unverified","recipes":{"image/jpeg":` + strings.TrimSuffix(string(validRecipe), "}") + `,"unknown":true}}}`,
		`{"evidence_status":"provisional-unverified","recipes":{"image/jpeg":{"source_mode":"still"}}}`,
		strings.Replace(string(encodeForTest(t, StandardV1Parameters())), `"crf":32,`, "", 1),
	}
	for index, raw := range tests {
		if _, err := DecodeParameters([]byte(raw)); err == nil {
			t.Errorf("case %d: DecodeParameters() error = nil", index)
		}
	}
}

func TestStrictJSONMatchesJSONBExponentCanonicalization(t *testing.T) {
	definition, parameters := mutableStandard(t)
	parameters.Recipes = map[string]Recipe{"image/jpeg": parameters.Recipes["image/jpeg"]}
	definition.InputMIMETypes = []string{"image/jpeg"}
	encoded := string(encodeForTest(t, parameters))
	encoded = strings.Replace(encoded, `"max_long_edge":1920`, `"max_long_edge":192e1`, 1)
	encoded = strings.Replace(encoded, `"quality":60`, `"quality":6e1`, 1)
	encoded = strings.Replace(encoded, `"bit_depth":8`, `"bit_depth":8e0`, 1)
	definition.Parameters = []byte(encoded)
	if err := ValidateDraft(definition); err != nil {
		t.Fatalf("ValidateDraft(jsonb-canonical exponent integers) error = %v", err)
	}
}

func TestStrictJSONRejectsCaseVariantUnknownField(t *testing.T) {
	definition, parameters := mutableStandard(t)
	parameters.Recipes = map[string]Recipe{"image/jpeg": parameters.Recipes["image/jpeg"]}
	definition.InputMIMETypes = []string{"image/jpeg"}
	encoded := strings.Replace(string(encodeForTest(t, parameters)), `"quality":60`, `"quality":60,"Quality":60`, 1)
	definition.Parameters = []byte(encoded)
	if err := ValidateDraft(definition); err == nil {
		t.Fatal("ValidateDraft(case-variant unknown field) error = nil")
	}
}

func TestStrictJSONRejectsDuplicateRecipesBeforeCollapse(t *testing.T) {
	recipe, err := json.Marshal(StandardV1Parameters().Recipes["image/jpeg"])
	if err != nil {
		t.Fatal(err)
	}
	definition := StandardV1()
	definition.InputMIMETypes = []string{"image/jpeg"}
	definition.Parameters = []byte(`{"evidence_status":"provisional-unverified","recipes":{"image/jpeg":` + string(recipe) + `},"recipes":{}}`)
	if err := ValidateDraft(definition); err == nil {
		t.Fatal("ValidateDraft(duplicate recipes with empty last value) error = nil")
	}
}

func TestStrictJSONRejectsDiscardedJSONBInvalidTokens(t *testing.T) {
	definition, parameters := mutableStandard(t)
	parameters.Recipes = map[string]Recipe{"image/jpeg": parameters.Recipes["image/jpeg"]}
	definition.InputMIMETypes = []string{"image/jpeg"}
	valid := string(encodeForTest(t, parameters))
	tests := [][]byte{
		[]byte(strings.Replace(valid, `"evidence_status":"provisional-unverified"`, `"evidence_status":"\u0000","evidence_status":"provisional-unverified"`, 1)),
		[]byte(strings.Replace(valid, `"evidence_status":"provisional-unverified"`, `"evidence_status":"\uD800","evidence_status":"provisional-unverified"`, 1)),
		[]byte(strings.Replace(valid, `"quality":60`, `"quality":1e1000000,"quality":60`, 1)),
	}
	invalidUTF8 := []byte(strings.Replace(valid, `"evidence_status":"provisional-unverified"`, `"evidence_status":"x","evidence_status":"provisional-unverified"`, 1))
	invalidUTF8[strings.Index(string(invalidUTF8), `"evidence_status":"x"`)+len(`"evidence_status":"`)] = 0xff
	tests = append(tests, invalidUTF8)
	for index, raw := range tests {
		definition.Parameters = raw
		if err := ValidateDraft(definition); err == nil {
			t.Errorf("case %d: ValidateDraft() error = nil", index)
		}
	}
}

func TestJSONBPreflightBoundsNesting(t *testing.T) {
	raw := []byte(strings.Repeat("[", maxParametersJSONDepth+1) + "0" + strings.Repeat("]", maxParametersJSONDepth+1))
	if err := validateJSONBRepresentable(raw); err == nil {
		t.Fatal("validateJSONBRepresentable(over-depth JSON) error = nil")
	}
}

func TestStrictJSONRejectsExponentBeyondPostgreSQLNumericRange(t *testing.T) {
	if _, err := decodeJSONBInteger(json.RawMessage(`0e1073741823`)); err != nil {
		t.Fatalf("decodeJSONBInteger(max PostgreSQL exponent) error = %v", err)
	}
	if _, err := decodeJSONBInteger(json.RawMessage(`0e1073741824`)); err == nil {
		t.Fatal("decodeJSONBInteger(over PostgreSQL exponent) error = nil")
	}
}

func TestStrictJSONRejectsNullRequiredValues(t *testing.T) {
	if _, err := DecodeParameters([]byte("null")); err == nil {
		t.Fatal("DecodeParameters() accepted null document")
	}
	tests := []struct {
		name     string
		mimeType string
		path     []string
	}{
		{name: "evidence status", path: []string{"evidence_status"}},
		{name: "recipes", path: []string{"recipes"}},
		{name: "recipe object", path: []string{"recipes", "image/jpeg"}},
	}
	for _, field := range []string{
		"source_mode", "frame_policy", "max_long_edge", "allow_upscale", "crop", "dimension_rule",
		"orientation", "color", "metadata", "alpha", "audio", "stream_selection", "animation_timing", "animation_loop",
	} {
		tests = append(tests, struct {
			name     string
			mimeType string
			path     []string
		}{name: "recipe " + field, mimeType: "image/jpeg", path: []string{"recipes", "image/jpeg", field}})
	}
	for _, field := range []string{"format", "quality", "bit_depth"} {
		tests = append(tests,
			struct {
				name     string
				mimeType string
				path     []string
			}{name: "still " + field, path: []string{"recipes", "image/jpeg", "still_output", field}},
			struct {
				name     string
				mimeType string
				path     []string
			}{name: "animation " + field, path: []string{"recipes", "image/gif", "animation_output", field}},
		)
	}
	for _, field := range []string{"container", "video_codec", "crf", "bit_depth", "chroma", "audio_codec", "audio_bitrate_kbps"} {
		tests = append(tests, struct {
			name     string
			mimeType string
			path     []string
		}{name: "video " + field, path: []string{"recipes", "video/mp4", "video_output", field}})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := setJSONPathToNull(t, StandardV1().Parameters, test.path...)
			if _, err := DecodeParameters(raw); err == nil {
				t.Fatal("DecodeParameters() accepted null")
			}
		})
	}
}

func TestStrictJSONAllowsNullOutputObjects(t *testing.T) {
	paths := [][]string{
		{"recipes", "image/jpeg", "still_output"},
		{"recipes", "image/gif", "animation_output"},
		{"recipes", "video/mp4", "video_output"},
	}
	for _, path := range paths {
		raw := setJSONPathToNull(t, StandardV1().Parameters, path...)
		if _, err := DecodeParameters(raw); err != nil {
			t.Fatalf("DecodeParameters() rejected nullable output %q: %v", path[len(path)-1], err)
		}
	}
}

func TestStrictJSONRejectsWrongTypes(t *testing.T) {
	tests := []struct {
		name  string
		path  []string
		value any
	}{
		{name: "evidence number", path: []string{"evidence_status"}, value: 1},
		{name: "recipes array", path: []string{"recipes"}, value: []any{}},
		{name: "source boolean", path: []string{"recipes", "image/jpeg", "source_mode"}, value: false},
		{name: "edge string", path: []string{"recipes", "image/jpeg", "max_long_edge"}, value: "1920"},
		{name: "upscale string", path: []string{"recipes", "image/jpeg", "allow_upscale"}, value: "false"},
		{name: "output array", path: []string{"recipes", "image/jpeg", "still_output"}, value: []any{}},
		{name: "quality string", path: []string{"recipes", "image/jpeg", "still_output", "quality"}, value: "60"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := setJSONPath(t, StandardV1().Parameters, test.value, test.path...)
			if _, err := DecodeParameters(raw); err == nil {
				t.Fatal("DecodeParameters() accepted wrong type")
			}
		})
	}
}

func TestInvalidRecipeValuesAndCombinations(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
		mutate   func(*Recipe)
	}{
		{name: "nonpositive edge", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.MaxLongEdge = 0 }},
		{name: "edge above int32", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.MaxLongEdge = math.MaxInt32 + 1 }},
		{name: "upscale", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.AllowUpscale = true }},
		{name: "crop", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Crop = "center" }},
		{name: "dimension rule", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.DimensionRule = "preserve-aspect" }},
		{name: "orientation", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Orientation = "keep" }},
		{name: "color", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Color = "none" }},
		{name: "metadata", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Metadata = "keep" }},
		{name: "alpha", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Alpha = "discard" }},
		{name: "still stream", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StreamSelection = "primary-video" }},
		{name: "still timing", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.AnimationTiming = "first-frame" }},
		{name: "still loop", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.AnimationLoop = "discard" }},
		{name: "source mismatch", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.SourceMode = SourceVideo }},
		{name: "still frame all", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.FramePolicy = FrameAll }},
		{name: "still audio", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.Audio = "discard" }},
		{name: "still missing", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StillOutput = nil }},
		{name: "still extra animation", mimeType: "image/jpeg", mutate: func(r *Recipe) {
			r.AnimationOutput = &AnimationOutput{Format: "animated-webp", Quality: 80, BitDepth: 8}
		}},
		{name: "still format", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StillOutput.Format = "jpeg" }},
		{name: "still quality low", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StillOutput.Quality = 0 }},
		{name: "still quality high", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StillOutput.Quality = 101 }},
		{name: "still bit depth", mimeType: "image/jpeg", mutate: func(r *Recipe) { r.StillOutput.BitDepth = 10 }},
		{name: "animation missing", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationOutput = nil }},
		{name: "animation timing", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationTiming = "first-frame" }},
		{name: "animation loop", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationLoop = "discard" }},
		{name: "animation format", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationOutput.Format = "gif" }},
		{name: "animation quality", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationOutput.Quality = 101 }},
		{name: "animation bit depth", mimeType: "image/gif", mutate: func(r *Recipe) { r.AnimationOutput.BitDepth = 10 }},
		{name: "probe video output", mimeType: "image/gif", mutate: func(r *Recipe) { r.VideoOutput = validVideo() }},
		{name: "video first wrong audio", mimeType: "video/mp4", mutate: func(r *Recipe) {
			r.FramePolicy = FrameFirst
			r.Audio = "aac-if-present"
			r.VideoOutput = nil
			r.StillOutput = &StillOutput{Format: "avif", Quality: 60, BitDepth: 8}
		}},
		{name: "video all still output", mimeType: "video/mp4", mutate: func(r *Recipe) { r.StillOutput = &StillOutput{Format: "avif", Quality: 60, BitDepth: 8} }},
		{name: "video missing output", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput = nil }},
		{name: "video color", mimeType: "video/mp4", mutate: func(r *Recipe) { r.Color = "normalize-srgb-tone-map-hdr" }},
		{name: "video stream", mimeType: "video/mp4", mutate: func(r *Recipe) { r.StreamSelection = "not-applicable" }},
		{name: "video container", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.Container = "webm" }},
		{name: "video codec", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.VideoCodec = "h264" }},
		{name: "video CRF low", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.CRF = -1 }},
		{name: "video CRF high", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.CRF = 64 }},
		{name: "video bit depth", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.BitDepth = 8 }},
		{name: "video chroma", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.Chroma = "4:4:4" }},
		{name: "video audio codec", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.AudioCodec = "opus" }},
		{name: "video bitrate", mimeType: "video/mp4", mutate: func(r *Recipe) { r.VideoOutput.AudioBitrateKbps = 256 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, parameters := mutableStandard(t)
			recipe := parameters.Recipes[test.mimeType]
			test.mutate(&recipe)
			parameters.Recipes[test.mimeType] = recipe
			definition.Parameters = encodeForTest(t, parameters)
			if err := ValidateDraft(definition); err == nil {
				t.Fatal("ValidateDraft() error = nil")
			}
		})
	}
}

func TestMaxLongEdgeAcceptsInt32Boundary(t *testing.T) {
	definition, parameters := mutableStandard(t)
	recipe := parameters.Recipes["image/jpeg"]
	recipe.MaxLongEdge = math.MaxInt32
	parameters.Recipes["image/jpeg"] = recipe
	definition.Parameters = encodeForTest(t, parameters)
	if err := ValidateDraft(definition); err != nil {
		t.Fatalf("ValidateDraft() error = %v", err)
	}
}

func TestActivationRequiresCertificationForEveryOutputKind(t *testing.T) {
	definition := StandardV1()
	registry := CandidateRegistry()
	if err := ValidateActivation(definition, registry); err == nil {
		t.Fatal("ValidateActivation() accepted uncertified registry")
	}

	withoutAnimation := certifyDefinition(t, definition, OutputAnimationWebP)
	if err := ValidateActivation(definition, withoutAnimation); err == nil {
		t.Fatal("ValidateActivation() accepted missing animation output certification")
	}

	registry = certifyDefinition(t, definition, "")
	if err := ValidateActivation(definition, registry); err != nil {
		t.Fatalf("ValidateActivation() error = %v", err)
	}
	if len(CandidateRegistry().Certifications()) != 0 {
		t.Fatal("certification mutated the default candidate registry")
	}
}

func TestWrongOutputCertificationCannotActivate(t *testing.T) {
	definition := ThumbnailV1()
	registry := certifyDefinitionSkipping(t, definition, "video/mp4", OutputStillAVIF)
	parameters := ThumbnailV1Parameters()
	certification := certificationFor("video/mp4", parameters.Recipes["video/mp4"], OutputVideoAV1, 640, 0, 63)
	var err error
	registry, err = registry.WithCertification(certification)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateActivation(definition, registry); err == nil {
		t.Fatal("ValidateActivation() accepted video certification for still-frame output")
	}
}

func TestCertificationEnvelopeBounds(t *testing.T) {
	definition := StandardV1()
	tests := []struct {
		name   string
		adjust func(*Certification)
	}{
		{name: "edge ceiling", adjust: func(c *Certification) { c.MaxLongEdgeCeiling = 1919 }},
		{name: "setting minimum", adjust: func(c *Certification) { c.SettingMin = 61 }},
		{name: "setting maximum", adjust: func(c *Certification) { c.SettingMax = 59 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := certifyDefinitionAdjusted(t, definition, "image/jpeg", OutputStillAVIF, test.adjust)
			if err := ValidateActivation(definition, registry); err == nil {
				t.Fatal("ValidateActivation() accepted recipe outside envelope")
			}
		})
	}
}

func TestCertificationValidation(t *testing.T) {
	valid := Certification{
		MIMEType: "image/jpeg", SourceMode: SourceStill, OutputKind: OutputStillAVIF,
		MaxLongEdgeCeiling: 1920, SettingMin: 50, SettingMax: 80, Evidence: "codec-fixture-1",
	}
	tests := []struct {
		name   string
		mutate func(*Certification)
	}{
		{name: "unknown MIME", mutate: func(c *Certification) { c.MIMEType = "image/tiff" }},
		{name: "wrong mode", mutate: func(c *Certification) { c.SourceMode = SourceVideo }},
		{name: "disallowed output", mutate: func(c *Certification) { c.OutputKind = OutputVideoAV1; c.SettingMin = 0; c.SettingMax = 63 }},
		{name: "unknown output", mutate: func(c *Certification) { c.OutputKind = "other" }},
		{name: "zero edge", mutate: func(c *Certification) { c.MaxLongEdgeCeiling = 0 }},
		{name: "large edge", mutate: func(c *Certification) { c.MaxLongEdgeCeiling = math.MaxInt32 + 1 }},
		{name: "low quality", mutate: func(c *Certification) { c.SettingMin = 0 }},
		{name: "high quality", mutate: func(c *Certification) { c.SettingMax = 101 }},
		{name: "reversed range", mutate: func(c *Certification) { c.SettingMin = 80; c.SettingMax = 50 }},
		{name: "empty evidence", mutate: func(c *Certification) { c.Evidence = "" }},
		{name: "blank evidence", mutate: func(c *Certification) { c.Evidence = "  " }},
		{name: "tab evidence", mutate: func(c *Certification) { c.Evidence = "\t\n" }},
		{name: "provisional evidence", mutate: func(c *Certification) { c.Evidence = " provisional-unverified " }},
		{name: "provisional tab evidence", mutate: func(c *Certification) { c.Evidence = "\nprovisional-unverified\t" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			certification := valid
			test.mutate(&certification)
			if _, err := CandidateRegistry().WithCertification(certification); err == nil {
				t.Fatal("WithCertification() error = nil")
			}
		})
	}

	video := Certification{
		MIMEType: "video/mp4", SourceMode: SourceVideo, OutputKind: OutputVideoAV1,
		MaxLongEdgeCeiling: 1920, SettingMin: 0, SettingMax: 63, Evidence: "codec-fixture-2",
	}
	if _, err := CandidateRegistry().WithCertification(video); err != nil {
		t.Fatalf("WithCertification(valid video) error = %v", err)
	}
}

func TestPublicResultsAreCopySafe(t *testing.T) {
	definition := StandardV1()
	definition.InputMIMETypes[0] = "changed"
	definition.Parameters[0] = 'x'
	if err := ValidateDraft(StandardV1()); err != nil {
		t.Fatalf("mutating returned definition affected bundle: %v", err)
	}

	parameters := StandardV1Parameters()
	recipe := parameters.Recipes["image/jpeg"]
	recipe.StillOutput.Quality = 1
	parameters.Recipes["image/jpeg"] = recipe
	if got := StandardV1Parameters().Recipes["image/jpeg"].StillOutput.Quality; got != 60 {
		t.Fatalf("mutating returned parameters changed quality to %d", got)
	}

	capabilities := CandidateRegistry().Capabilities()
	capabilities[0].MIMEType = "changed"
	if CandidateRegistry().Capabilities()[0].MIMEType == "changed" {
		t.Fatal("mutating capability result affected candidate registry")
	}

	registry := certifyDefinition(t, ThumbnailV1(), "")
	certifications := registry.Certifications()
	certifications[0].Evidence = "changed"
	if registry.Certifications()[0].Evidence == "changed" {
		t.Fatal("mutating certification result affected registry")
	}
}

func assertStill(t *testing.T, name string, output *StillOutput, quality int) {
	t.Helper()
	if output == nil || output.Format != "avif" || output.Quality != quality || output.BitDepth != 8 {
		t.Errorf("%s still output = %#v", name, output)
	}
}

func assertRouting(t *testing.T, name string, recipe Recipe, color, stream, timing, loop string) {
	t.Helper()
	if recipe.Color != color || recipe.StreamSelection != stream || recipe.AnimationTiming != timing || recipe.AnimationLoop != loop {
		t.Errorf("%s routing = color %q, stream %q, timing %q, loop %q", name, recipe.Color, recipe.StreamSelection, recipe.AnimationTiming, recipe.AnimationLoop)
	}
}

func setJSONPathToNull(t *testing.T, raw []byte, path ...string) []byte {
	t.Helper()
	return setJSONPath(t, raw, nil, path...)
}

func setJSONPath(t *testing.T, raw []byte, value any, path ...string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	current := document
	for _, component := range path[:len(path)-1] {
		next, ok := current[component].(map[string]any)
		if !ok {
			t.Fatalf("path component %q is not an object", component)
		}
		current = next
	}
	current[path[len(path)-1]] = value
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func certifyDefinition(t *testing.T, definition Definition, skipKind OutputKind) Registry {
	t.Helper()
	return certifyDefinitionSkipping(t, definition, "", skipKind)
}

func certifyDefinitionSkipping(t *testing.T, definition Definition, skipMIME string, skipKind OutputKind) Registry {
	t.Helper()
	parameters := decodeForTest(t, definition)
	registry := CandidateRegistry()
	for mimeType, recipe := range parameters.Recipes {
		for _, output := range recipeOutputs(recipe) {
			if output.kind == skipKind && (skipMIME == "" || mimeType == skipMIME) {
				continue
			}
			minimum, maximum := settingBounds(output.kind)
			certification := certificationFor(mimeType, recipe, output.kind, recipe.MaxLongEdge, minimum, maximum)
			var err error
			registry, err = registry.WithCertification(certification)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return registry
}

func certifyDefinitionAdjusted(t *testing.T, definition Definition, targetMIME string, targetKind OutputKind, adjust func(*Certification)) Registry {
	t.Helper()
	parameters := decodeForTest(t, definition)
	registry := CandidateRegistry()
	for mimeType, recipe := range parameters.Recipes {
		for _, output := range recipeOutputs(recipe) {
			minimum, maximum := settingBounds(output.kind)
			certification := certificationFor(mimeType, recipe, output.kind, recipe.MaxLongEdge, minimum, maximum)
			if mimeType == targetMIME && output.kind == targetKind {
				adjust(&certification)
			}
			var err error
			registry, err = registry.WithCertification(certification)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return registry
}

func certificationFor(mimeType string, recipe Recipe, outputKind OutputKind, maxLongEdge, minimum, maximum int) Certification {
	return Certification{
		MIMEType: mimeType, SourceMode: recipe.SourceMode, OutputKind: outputKind,
		MaxLongEdgeCeiling: maxLongEdge, SettingMin: minimum, SettingMax: maximum,
		Evidence: "test-codec-fixture",
	}
}

func mutableStandard(t *testing.T) (Definition, Parameters) {
	t.Helper()
	definition := StandardV1()
	return definition, decodeForTest(t, definition)
}

func decodeForTest(t *testing.T, definition Definition) Parameters {
	t.Helper()
	parameters, err := DecodeParameters(definition.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	return parameters
}

func encodeForTest(t *testing.T, parameters Parameters) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func validVideo() *VideoOutput {
	return &VideoOutput{Container: "mp4", VideoCodec: "av1", CRF: 32, BitDepth: 10, Chroma: "4:2:0", AudioCodec: "aac", AudioBitrateKbps: 128}
}
