//go:build linux

package animationprocessor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

type referenceWebP struct {
	Width             int      `json:"width"`
	Height            int      `json:"height"`
	Frames            int      `json:"frames"`
	TotalPlays        int      `json:"total_plays"`
	DurationsMS       []int    `json:"durations_ms"`
	TransparentPixels []uint64 `json:"transparent_pixels"`
	AlphaSums         []uint64 `json:"alpha_sums"`
	TimestampsMS      []int    `json:"timestamps_ms"`
	RGBSums           []uint64 `json:"rgb_sums"`
}

func TestNativeAnimationHelper(t *testing.T) {
	if os.Getenv("TEST_ANIMATION_HELPER") != "1" {
		t.Skip("set TEST_ANIMATION_HELPER=1 with the pinned native fixture environment")
	}
	helper := requiredTestPath(t, "TEST_ANIMATION_HELPER_PATH")
	icc := requiredTestPath(t, "TEST_STILL_ICC_PATH")
	webpFixture := requiredTestPath(t, "TEST_ANIMATION_WEBP_PATH")
	reference := requiredTestPath(t, "TEST_ANIMATION_WEBP_REFERENCE_PATH")
	avifReference := requiredTestPath(t, "TEST_STILL_AVIF_REFERENCE_PATH")
	gifBackground := requiredTestPath(t, "TEST_ANIMATION_GIF_BACKGROUND_PATH")
	gifPrevious := requiredTestPath(t, "TEST_ANIMATION_GIF_PREVIOUS_PATH")
	gifIncompressibleSingle := requiredTestPath(t, "TEST_ANIMATION_GIF_INCOMPRESSIBLE_SINGLE_PATH")
	gifIncompressibleMany := requiredTestPath(t, "TEST_ANIMATION_GIF_INCOMPRESSIBLE_MANY_PATH")
	processor, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc,
		SRGBICCSHA256: "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a"})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := processor.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.LibraryVersions["libwebp"] != "1.6.0" || capabilities.LibraryVersions["giflib"] != "5.2.2" {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	input, err := os.Open(webpFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	inspection, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/webp"})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Classification != ClassificationAnimation || inspection.Width != 6 || inspection.Height != 2 ||
		!slices.Equal(inspection.FrameDurationsMS, []int{40, 100, 250}) || !slices.Equal(inspection.ZeroDurationFrameIndices, []int{1}) || inspection.TotalPlays != 4 || !inspection.HasAlpha {
		t.Fatalf("WebP inspection = %+v", inspection)
	}

	standardOutput := createOutput(t, "standard.webp")
	result, err := processor.Transform(context.Background(), Request{Input: input, Output: standardOutput, MIMEType: "image/webp", Recipe: standardRecipe()})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputMIME != "image/webp" || result.Width != 6 || result.Height != 2 || result.Source.TotalPlays != 4 {
		t.Fatalf("standard result = %+v", result)
	}
	assertReferenceWebP(t, reference, standardOutput.Name(), referenceWebP{Width: 6, Height: 2, Frames: 3, TotalPlays: 4, DurationsMS: []int{40, 100, 250}, TransparentPixels: []uint64{8, 8, 4}, AlphaSums: []uint64{1020, 1532, 2040}, TimestampsMS: []int{40, 140, 390}})

	thumbnailOutput := createOutput(t, "thumbnail.avif")
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	thumbnail, err := processor.Transform(context.Background(), Request{Input: input, Output: thumbnailOutput, MIMEType: "image/webp", Recipe: thumbnailRecipe()})
	if err != nil {
		t.Fatal(err)
	}
	if thumbnail.OutputMIME != "image/avif" || thumbnail.Width != 6 || thumbnail.Height != 2 {
		t.Fatalf("thumbnail result = %+v", thumbnail)
	}
	raw := filepath.Join(t.TempDir(), "thumbnail.rgba")
	if output, err := exec.Command(avifReference, thumbnailOutput.Name(), raw).CombinedOutput(); err != nil {
		t.Fatalf("AVIF reference: %v: %s", err, output)
	}
	pixels, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pixels) != 6*2*4 {
		t.Fatalf("thumbnail RGBA length = %d", len(pixels))
	}
	transparent := false
	for offset := 3; offset < len(pixels); offset += 4 {
		transparent = transparent || pixels[offset] != 255
	}
	if !transparent {
		t.Fatal("thumbnail independent decode lost alpha")
	}

	for name, test := range map[string]struct {
		policy Policy
		want   error
	}{
		"frames exact":       {policy: Policy{Frames: 3}},
		"frames plus one":    {policy: Policy{Frames: 2}, want: ErrResourcePolicy},
		"duration exact":     {policy: Policy{DurationMS: 390}},
		"duration plus one":  {policy: Policy{DurationMS: 389}, want: ErrResourcePolicy},
		"dimension exact":    {policy: Policy{Dimension: 6}},
		"dimension plus one": {policy: Policy{Dimension: 5}, want: ErrResourcePolicy},
		"canvas exact":       {policy: Policy{CanvasPixels: 12}},
		"canvas plus one":    {policy: Policy{CanvasPixels: 11}, want: ErrResourcePolicy},
		"decoded exact":      {policy: Policy{CumulativeDecodedPixels: 36}},
		"decoded plus one":   {policy: Policy{CumulativeDecodedPixels: 35}, want: ErrResourcePolicy},
	} {
		t.Run(name, func(t *testing.T) {
			bounded, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc, SRGBICCSHA256: "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a", Policy: test.policy})
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(webpFixture)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			_, err = bounded.Inspect(context.Background(), InspectRequest{Input: file, MIMEType: "image/webp"})
			if !errors.Is(err, test.want) {
				t.Fatalf("Inspect() error = %v, want %v", err, test.want)
			}
		})
	}
	standardInfo, err := standardOutput.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for name, maximum := range map[string]int64{"output exact": standardInfo.Size(), "output plus one": standardInfo.Size() - 1} {
		t.Run(name, func(t *testing.T) {
			bounded, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc, SRGBICCSHA256: "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a", Policy: Policy{GeneratedOutputMaxBytes: maximum}})
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(webpFixture)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			output := createOutput(t, name+".webp")
			_, err = bounded.Transform(context.Background(), Request{Input: file, Output: output, MIMEType: "image/webp", Recipe: standardRecipe()})
			if name == "output exact" && err != nil {
				t.Fatal(err)
			}
			if name == "output plus one" && !errors.Is(err, ErrResourcePolicy) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}

	gifPath := filepath.Join(t.TempDir(), "single-transparent.gif")
	if err := os.WriteFile(gifPath, singleTransparentGIF, 0o600); err != nil {
		t.Fatal(err)
	}
	gif, err := os.Open(gifPath)
	if err != nil {
		t.Fatal(err)
	}
	defer gif.Close()
	gifInspection, err := processor.Inspect(context.Background(), InspectRequest{Input: gif, MIMEType: "image/gif"})
	if err != nil {
		t.Fatal(err)
	}
	if gifInspection.Classification != ClassificationAnimation || gifInspection.FrameCount != 1 || gifInspection.DurationMS != 100 || gifInspection.TotalPlays != 1 || !gifInspection.HasAlpha || !slices.Equal(gifInspection.ZeroDurationFrameIndices, []int{0}) {
		t.Fatalf("single GIF inspection = %+v", gifInspection)
	}
	gifOutput := createOutput(t, "single.webp")
	if _, err := processor.Transform(context.Background(), Request{Input: gif, Output: gifOutput, MIMEType: "image/gif", Recipe: standardRecipe()}); err != nil {
		t.Fatalf("transform single-frame transparent GIF while preserving ANMF timing: %v", err)
	}
	assertReferenceWebP(t, reference, gifOutput.Name(), referenceWebP{Width: 1, Height: 1, Frames: 1, TotalPlays: 1, DurationsMS: []int{100}, TransparentPixels: []uint64{1}, AlphaSums: []uint64{0}, TimestampsMS: []int{100}})
	staticWebP, err := os.Open(gifOutput.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer staticWebP.Close()
	staticInspection, err := processor.Inspect(context.Background(), InspectRequest{Input: staticWebP, MIMEType: "image/webp"})
	if err != nil || staticInspection.Classification != ClassificationStatic {
		t.Fatalf("single-frame WebP = %+v, %v", staticInspection, err)
	}
	if _, err := processor.Transform(context.Background(), Request{Input: staticWebP, Output: createOutput(t, "static-rejected.webp"), MIMEType: "image/webp", Recipe: standardRecipe()}); !errors.Is(err, ErrStaticInput) {
		t.Fatalf("static Transform() error = %v", err)
	}

	for name, test := range map[string]struct {
		repetitions  uint16
		present      bool
		delay        uint16
		wantPlays    int
		wantDuration int
		want         error
	}{
		"absent loop":          {delay: 7, wantPlays: 1, wantDuration: 70},
		"infinite loop":        {present: true, repetitions: 0, wantPlays: 0, wantDuration: 100},
		"finite loop":          {present: true, repetitions: 3, wantPlays: 4, wantDuration: 100},
		"maximum loop":         {present: true, repetitions: 65534, wantPlays: 65535, wantDuration: 100},
		"unrepresentable loop": {present: true, repetitions: 65535, want: ErrUnsupportedInput},
	} {
		t.Run(name, func(t *testing.T) {
			bytes := gifWithLoopAndDelay(test.present, test.repetitions, test.delay)
			path := filepath.Join(t.TempDir(), "loop.gif")
			if err := os.WriteFile(path, bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, err := processor.Inspect(context.Background(), InspectRequest{Input: file, MIMEType: "image/gif"})
			if !errors.Is(err, test.want) {
				t.Fatalf("Inspect() error = %v, want %v", err, test.want)
			}
			if test.want == nil && (got.TotalPlays != test.wantPlays || int(got.DurationMS) != test.wantDuration) {
				t.Fatalf("Inspect() = %+v", got)
			}
		})
	}
	for _, mimeType := range []string{"image/gif", "image/webp"} {
		path := filepath.Join(t.TempDir(), "corrupt")
		if err := os.WriteFile(path, []byte("truncated"), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := processor.Inspect(context.Background(), InspectRequest{Input: file, MIMEType: mimeType}); !errors.Is(err, ErrDecode) {
			t.Fatalf("corrupt %s error = %v", mimeType, err)
		}
	}
	for name, test := range map[string]struct {
		path               string
		transparent, alpha []uint64
	}{
		"background disposal": {path: gifBackground, transparent: []uint64{0, 0, 1}, alpha: []uint64{765, 765, 510}},
		"previous disposal":   {path: gifPrevious, transparent: []uint64{0, 0, 0}, alpha: []uint64{765, 765, 765}},
	} {
		t.Run(name, func(t *testing.T) {
			file, err := os.Open(test.path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, err := processor.Inspect(context.Background(), InspectRequest{Input: file, MIMEType: "image/gif"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.FrameDurationsMS, []int{40, 100, 250}) || got.TotalPlays != 1 {
				t.Fatalf("GIF inspection = %+v", got)
			}
			output := createOutput(t, name+".webp")
			if _, err := processor.Transform(context.Background(), Request{Input: file, Output: output, MIMEType: "image/gif", Recipe: standardRecipe()}); err != nil {
				t.Fatal(err)
			}
			decoded := assertReferenceWebP(t, reference, output.Name(), referenceWebP{Width: 3, Height: 1, Frames: 3, TotalPlays: 1, DurationsMS: []int{40, 100, 250}, TransparentPixels: test.transparent, AlphaSums: test.alpha, TimestampsMS: []int{40, 140, 390}})
			if len(decoded.RGBSums) != 9 {
				t.Fatalf("reference RGB sums = %v", decoded.RGBSums)
			}
			if name == "previous disposal" && decoded.RGBSums[6] <= decoded.RGBSums[7]+200 {
				t.Fatalf("restore-to-previous did not restore red canvas: RGB sums %v", decoded.RGBSums[6:9])
			}
			if name == "background disposal" && (decoded.RGBSums[6] <= decoded.RGBSums[7]+100 || decoded.RGBSums[8] <= decoded.RGBSums[7]+100) {
				t.Fatalf("background disposal composition RGB sums = %v", decoded.RGBSums[6:9])
			}
		})
	}
	t.Run("cumulative mux output budget", func(t *testing.T) {
		bounded, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc,
			SRGBICCSHA256: "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a",
			Policy:        Policy{GeneratedOutputMaxBytes: 64 << 10}})
		if err != nil {
			t.Fatal(err)
		}
		single, err := os.Open(gifIncompressibleSingle)
		if err != nil {
			t.Fatal(err)
		}
		defer single.Close()
		if _, err := bounded.Transform(context.Background(), Request{Input: single, Output: createOutput(t, "incompressible-single.webp"), MIMEType: "image/gif", Recipe: standardRecipe()}); err != nil {
			t.Fatalf("one incompressible frame must fit below the per-output limit: %v", err)
		}
		many, err := os.Open(gifIncompressibleMany)
		if err != nil {
			t.Fatal(err)
		}
		defer many.Close()
		if _, err := bounded.Transform(context.Background(), Request{Input: many, Output: createOutput(t, "incompressible-many.webp"), MIMEType: "image/gif", Recipe: standardRecipe()}); !errors.Is(err, ErrResourcePolicy) {
			t.Fatalf("cumulative mux limit error = %v, want %v", err, ErrResourcePolicy)
		}
	})
}

func assertReferenceWebP(t *testing.T, executable, path string, want referenceWebP) referenceWebP {
	t.Helper()
	output, err := exec.Command(executable, path).Output()
	if err != nil {
		t.Fatalf("WebP reference: %v", err)
	}
	var got referenceWebP
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Width != want.Width || got.Height != want.Height || got.Frames != want.Frames || got.TotalPlays != want.TotalPlays || !slices.Equal(got.DurationsMS, want.DurationsMS) {
		t.Fatalf("reference = %+v, want %+v", got, want)
	}
	if !slices.Equal(got.TimestampsMS, want.TimestampsMS) {
		t.Fatalf("reference timestamps = %v, want %v", got.TimestampsMS, want.TimestampsMS)
	}
	if len(got.TransparentPixels) != got.Frames {
		t.Fatalf("reference alpha = %+v", got.TransparentPixels)
	}
	if want.TransparentPixels != nil && (!slices.Equal(got.TransparentPixels, want.TransparentPixels) || !slices.Equal(got.AlphaSums, want.AlphaSums)) {
		t.Fatalf("reference alpha = pixels %v sums %v, want pixels %v sums %v", got.TransparentPixels, got.AlphaSums, want.TransparentPixels, want.AlphaSums)
	}
	return got
}

func requiredTestPath(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be absolute", name)
	}
	return value
}
func createOutput(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

var singleTransparentGIF = []byte{
	'G', 'I', 'F', '8', '9', 'a', 1, 0, 1, 0, 0x80, 0, 0, 0, 0, 0, 255, 255, 255,
	0x21, 0xf9, 4, 1, 0, 0, 0, 0, 0x2c, 0, 0, 0, 0, 1, 0, 1, 0, 0,
	2, 2, 0x44, 1, 0, 0x3b,
}

func gifWithLoopAndDelay(present bool, repetitions, delay uint16) []byte {
	result := append([]byte(nil), singleTransparentGIF...)
	for index := 0; index+7 < len(result); index++ {
		if result[index] == 0x21 && result[index+1] == 0xf9 && result[index+2] == 4 {
			result[index+4], result[index+5] = byte(delay), byte(delay>>8)
			break
		}
	}
	if !present {
		return result
	}
	extension := []byte{0x21, 0xff, 0x0b, 'N', 'E', 'T', 'S', 'C', 'A', 'P', 'E', '2', '.', '0', 0x03, 0x01, byte(repetitions), byte(repetitions >> 8), 0x00}
	withLoop := make([]byte, 0, len(result)+len(extension))
	withLoop = append(withLoop, result[:19]...)
	withLoop = append(withLoop, extension...)
	withLoop = append(withLoop, result[19:]...)
	return withLoop
}
