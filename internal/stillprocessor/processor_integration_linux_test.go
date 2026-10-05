//go:build linux

package stillprocessor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func TestRealStillHelper(t *testing.T) {
	if os.Getenv("TEST_STILL_HELPER") != "1" {
		t.Skip("set TEST_STILL_HELPER=1 for native codec evidence")
	}
	helper := requiredTestPath(t, "TEST_STILL_HELPER_PATH")
	icc := requiredTestPath(t, "TEST_STILL_ICC_PATH")
	ffprobe := requiredTestPath(t, "TEST_FFPROBE_PATH")
	ffmpeg := requiredTestPath(t, "TEST_FFMPEG_PATH")
	iccBytes, err := os.ReadFile(icc)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(Config{
		Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc,
		SRGBICCSHA256: fmt.Sprintf("%x", sha256.Sum256(iccBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := processor.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.AVIFEncoder != "aom" || capabilities.Threads != 1 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	heic := readFixture(t, "TEST_STILL_HEIC_PATH", "7f8b363e4936c0666a25f64f3a92fda10bd8e5453be4592530b65a55dd98f3f2")
	webp := readFixture(t, "TEST_STILL_WEBP_PATH", "0858d0afcb2921ded36b05586204f2459d965feb7db54cb083e3cfa059589dd9")
	orientation6 := readFixture(t, "TEST_STILL_ORIENTATION6_PATH", "9b344e9f0c869d8637ea22e672df9451d8d3cc1d2d0b291af3b284e538e5f124")
	rawDNG := readFixture(t, "TEST_STILL_RAW_PATH", "f0d2fe47507fadf50008bddbc6bd2c5e39fddbe90cca6bca72dd360fa0e0eb38")
	pq := readGeneratedFixture(t, "TEST_STILL_PQ_PATH")
	hlg := readGeneratedFixture(t, "TEST_STILL_HLG_PATH")
	iccNCLX := readGeneratedFixture(t, "TEST_STILL_ICC_NCLX_PATH")

	for _, test := range []struct {
		name, mime, alpha, inputColor, toneMap, rawProcessing string
		width, height                                         int
		input                                                 []byte
		orientation6                                          bool
		referencePixel                                        []int
		largeRAW                                              bool
	}{
		{name: "JPEG odd opaque", mime: "image/jpeg", alpha: "opaque", width: 5, height: 3,
			input: encodeJPEG(t, 5, 3)},
		{name: "PNG odd alpha", mime: "image/png", alpha: "preserved", width: 3, height: 5,
			input: encodePNG(t, 3, 5, true)},
		{name: "PNG one pixel axis", mime: "image/png", alpha: "preserved", width: 1, height: 7,
			input: encodePNG(t, 1, 7, true)},
		{name: "BMP32 reserved byte opaque", mime: "image/bmp", alpha: "opaque", width: 3, height: 2,
			input: encodeBMP32(3, 2)},
		{name: "libheif real HEIC", mime: "image/heic", alpha: "opaque", input: heic},
		{name: "Google gallery static WebP", mime: "image/webp", alpha: "opaque", input: webp},
		{name: "real Exif orientation 6", mime: "image/jpeg", alpha: "opaque", width: 1800, height: 1200,
			input: orientation6, orientation6: true},
		{name: "real camera RAW", mime: "image/dng", alpha: "opaque", input: rawDNG,
			inputColor: "raw-camera-matrix", toneMap: "not-needed",
			rawProcessing: "camera-wb-camera-matrix-16bit-no-auto-bright", largeRAW: true},
		{name: "real PQ NCLX reference pixel", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: pq, inputColor: "nclx-pq", toneMap: "bt2446a-method-a", referencePixel: []int{127, 127, 127}},
		{name: "real HLG NCLX reference pixel", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: hlg, inputColor: "nclx-hlg", toneMap: "bt2446a-method-a", referencePixel: []int{100, 100, 100}},
		{name: "ICC and NCLX single normalization", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: iccNCLX, inputColor: "embedded-icc", toneMap: "not-needed", referencePixel: []int{64, 128, 192}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputPath := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(inputPath, test.input, 0o600); err != nil {
				t.Fatal(err)
			}
			input, err := os.Open(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			outputPath := filepath.Join(t.TempDir(), "output.avif")
			output, err := os.Create(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := processor.Transform(context.Background(), Request{
				Input: input, Output: output, MIMEType: test.mime,
				Recipe: profile.StandardV1Parameters().Recipes[test.mime],
			})
			if err != nil {
				_ = output.Close()
				t.Fatal(err)
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if result.Width <= 0 || result.Height <= 0 || result.Audit.Alpha != test.alpha ||
				(test.width != 0 && (result.Width != test.width || result.Height != test.height)) {
				t.Fatalf("result = %+v", result)
			}
			if test.inputColor != "" && (result.Audit.InputColor != test.inputColor || result.Audit.ToneMap != test.toneMap) {
				t.Fatalf("color audit = %+v", result.Audit)
			}
			if test.rawProcessing != "" && result.Audit.RawProcessing != test.rawProcessing {
				t.Fatalf("RAW audit = %+v", result.Audit)
			}
			if test.largeRAW && (result.Width != 1920 || result.Audit.SourceWidth <= result.Width || result.Audit.SourceHeight <= result.Height) {
				t.Fatalf("large RAW resize = %+v", result)
			}
			probeAVIF(t, ffprobe, outputPath, result.Width, result.Height)
			if test.orientation6 {
				assertOrientation6Pixels(t, ffmpeg, test.input, outputPath, result.Width, result.Height)
			}
			if test.referencePixel != nil {
				assertReferencePixel(t, ffmpeg, outputPath, result.Width, result.Height, test.referencePixel)
			}
		})
	}

	t.Run("corrupt real RAW fails closed", func(t *testing.T) {
		input, output := testFiles(t, rawDNG[:512])
		_, err := processor.Transform(context.Background(), Request{
			Input: input, Output: output, MIMEType: "image/dng",
			Recipe: profile.StandardV1Parameters().Recipes["image/dng"],
		})
		if !errors.Is(err, ErrDecode) {
			t.Fatalf("corrupt RAW error = %v", err)
		}
	})
}

func assertReferencePixel(t *testing.T, ffmpeg, outputPath string, width, height int, expected []int) {
	t.Helper()
	decoded, err := exec.Command(ffmpeg, "-v", "error", "-i", outputPath, "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg decode: %v", err)
	}
	offset := ((height/2)*width + width/2) * 3
	if len(decoded) < offset+3 {
		t.Fatalf("decoded bytes = %d", len(decoded))
	}
	for channel := range 3 {
		difference := int(decoded[offset+channel]) - expected[channel]
		if difference < 0 {
			difference = -difference
		}
		if difference > 24 {
			t.Fatalf("reference pixel channel %d = %d, want %d (+/-24)", channel, decoded[offset+channel], expected[channel])
		}
	}
}

func assertOrientation6Pixels(t *testing.T, ffmpeg string, sourceJPEG []byte, outputPath string, width, height int) {
	t.Helper()
	source, err := jpeg.Decode(bytes.NewReader(sourceJPEG))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ffmpeg, "-v", "error", "-i", outputPath, "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	decoded, err := command.Output()
	if err != nil {
		t.Fatalf("ffmpeg decode: %v", err)
	}
	if len(decoded) != width*height*3 {
		t.Fatalf("decoded bytes = %d", len(decoded))
	}
	for _, point := range [][2]int{{width / 4, height / 4}, {width / 2, height / 2}, {3 * width / 4, 3 * height / 4}} {
		x, y := point[0], point[1]
		r, g, b, _ := source.At(y, source.Bounds().Dy()-1-x).RGBA()
		offset := (y*width + x) * 3
		expected := []int{int(r >> 8), int(g >> 8), int(b >> 8)}
		for channel := range 3 {
			difference := int(decoded[offset+channel]) - expected[channel]
			if difference < 0 {
				difference = -difference
			}
			if difference > 60 {
				t.Fatalf("orientation pixel (%d,%d) channel %d difference = %d", x, y, channel, difference)
			}
		}
	}
}

func readFixture(t *testing.T, environment, expectedSHA256 string) []byte {
	t.Helper()
	data, err := os.ReadFile(requiredTestPath(t, environment))
	if err != nil {
		t.Fatal(err)
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(data)); actual != expectedSHA256 {
		t.Fatalf("%s SHA-256 = %s", environment, actual)
	}
	return data
}

func readGeneratedFixture(t *testing.T, environment string) []byte {
	t.Helper()
	data, err := os.ReadFile(requiredTestPath(t, environment))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requiredTestPath(t *testing.T, name string) string {
	t.Helper()
	path := os.Getenv(name)
	if !filepath.IsAbs(path) {
		t.Fatalf("%s must be absolute", name)
	}
	return path
}

func testImage(width, height int, alpha bool) image.Image {
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			a := uint8(255)
			if alpha && (x+y)%2 == 0 {
				a = 96
			}
			result.SetNRGBA(x, y, color.NRGBA{R: uint8(20 + x*30), G: uint8(30 + y*20), B: 180, A: a})
		}
	}
	return result
}

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, testImage(width, height, false), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func encodePNG(t *testing.T, width, height int, alpha bool) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, testImage(width, height, alpha)); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func encodeBMP32(width, height int) []byte {
	const headerSize = 54
	stride := width * 4
	result := make([]byte, headerSize+stride*height)
	copy(result, "BM")
	binary.LittleEndian.PutUint32(result[2:], uint32(len(result)))
	binary.LittleEndian.PutUint32(result[10:], headerSize)
	binary.LittleEndian.PutUint32(result[14:], 40)
	binary.LittleEndian.PutUint32(result[18:], uint32(width))
	binary.LittleEndian.PutUint32(result[22:], uint32(height))
	binary.LittleEndian.PutUint16(result[26:], 1)
	binary.LittleEndian.PutUint16(result[28:], 32)
	binary.LittleEndian.PutUint32(result[34:], uint32(stride*height))
	for offset := headerSize; offset < len(result); offset += 4 {
		result[offset], result[offset+1], result[offset+2], result[offset+3] = 180, 80, 20, 0
	}
	return result
}

func probeAVIF(t *testing.T, ffprobe, path string, width, height int) {
	t.Helper()
	command := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"stream=codec_name,width,height", "-of", "json", path)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var document struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Streams) != 1 || document.Streams[0].CodecName != "av1" ||
		document.Streams[0].Width != width || document.Streams[0].Height != height {
		t.Fatalf("ffprobe = %s", output)
	}
}
