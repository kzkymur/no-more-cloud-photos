//go:build linux

package stillprocessor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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

	for _, test := range []struct {
		name, mime, alpha string
		width, height     int
		input             []byte
	}{
		{name: "JPEG odd opaque", mime: "image/jpeg", alpha: "opaque", width: 5, height: 3,
			input: encodeJPEG(t, 5, 3)},
		{name: "PNG odd alpha", mime: "image/png", alpha: "preserved", width: 3, height: 5,
			input: encodePNG(t, 3, 5, true)},
		{name: "PNG one pixel axis", mime: "image/png", alpha: "preserved", width: 1, height: 7,
			input: encodePNG(t, 1, 7, true)},
		{name: "BMP32 reserved byte opaque", mime: "image/bmp", alpha: "opaque", width: 3, height: 2,
			input: encodeBMP32(3, 2)},
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
			if result.Width != test.width || result.Height != test.height || result.Audit.Alpha != test.alpha {
				t.Fatalf("result = %+v", result)
			}
			probeAVIF(t, ffprobe, outputPath, test.width, test.height)
		})
	}
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
