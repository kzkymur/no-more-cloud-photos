//go:build linux

package animationprocessor

import (
	"context"
	"encoding/json"
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
	assertReferenceWebP(t, reference, standardOutput.Name(), referenceWebP{Width: 6, Height: 2, Frames: 3, TotalPlays: 4, DurationsMS: []int{40, 100, 250}})

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
		t.Fatal(err)
	}
	assertReferenceWebP(t, reference, gifOutput.Name(), referenceWebP{Width: 1, Height: 1, Frames: 1, TotalPlays: 1, DurationsMS: []int{100}})
}

func assertReferenceWebP(t *testing.T, executable, path string, want referenceWebP) {
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
	if len(got.TransparentPixels) != got.Frames || !slices.ContainsFunc(got.TransparentPixels, func(value uint64) bool { return value > 0 }) {
		t.Fatalf("reference alpha = %+v", got.TransparentPixels)
	}
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
