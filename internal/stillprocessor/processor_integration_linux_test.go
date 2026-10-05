//go:build linux

package stillprocessor

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	rawReference := requiredTestPath(t, "TEST_STILL_RAW_REFERENCE_PATH")
	iccBytes, err := os.ReadFile(icc)
	if err != nil {
		t.Fatal(err)
	}
	p3ICC, err := os.ReadFile(requiredTestPath(t, "TEST_STILL_P3_ICC_PATH"))
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
	for _, mimeType := range capabilities.DecoderMIMETypes {
		if len(mimeType) > 6 && (mimeType == "image/x-canon-cr2" || mimeType == "image/x-canon-cr3" ||
			mimeType == "image/x-fuji-raf" || mimeType == "image/x-nikon-nef" || mimeType == "image/x-olympus-orf" ||
			mimeType == "image/x-panasonic-rw2" || mimeType == "image/x-sony-arw") {
			t.Fatalf("unproven RAW capability advertised: %s", mimeType)
		}
	}
	heic := readFixture(t, "TEST_STILL_HEIC_PATH", "7f8b363e4936c0666a25f64f3a92fda10bd8e5453be4592530b65a55dd98f3f2")
	webp := readFixture(t, "TEST_STILL_WEBP_PATH", "0858d0afcb2921ded36b05586204f2459d965feb7db54cb083e3cfa059589dd9")
	orientation6 := readFixture(t, "TEST_STILL_ORIENTATION6_PATH", "9b344e9f0c869d8637ea22e672df9451d8d3cc1d2d0b291af3b284e538e5f124")
	rawDNG := readFixture(t, "TEST_STILL_RAW_PATH", "f0d2fe47507fadf50008bddbc6bd2c5e39fddbe90cca6bca72dd360fa0e0eb38")
	pq := readGeneratedFixture(t, "TEST_STILL_PQ_PATH")
	hlg := readGeneratedFixture(t, "TEST_STILL_HLG_PATH")
	iccNCLX := readGeneratedFixture(t, "TEST_STILL_ICC_NCLX_PATH")
	invalidICC := readGeneratedFixture(t, "TEST_STILL_INVALID_ICC_PATH")
	heifCollection := readGeneratedFixture(t, "TEST_STILL_HEIF_COLLECTION_PATH")

	for _, test := range []struct {
		name, mime, alpha, inputColor, toneMap, rawProcessing string
		width, height                                         int
		input                                                 []byte
		orientation6                                          bool
		referencePixel                                        []int
		largeRAW                                              bool
		rawReference                                          bool
		alphaReference                                        bool
		bmpReference                                          bool
		thumbnail                                             bool
		quality                                               int
	}{
		{name: "JPEG odd opaque", mime: "image/jpeg", alpha: "opaque", width: 5, height: 3,
			input: encodeJPEG(t, 5, 3)},
		{name: "JPEG thumbnail 640 edge", mime: "image/jpeg", alpha: "opaque", width: 640, height: 497,
			input: encodeJPEG(t, 1000, 777), thumbnail: true, quality: 50},
		{name: "PNG odd alpha", mime: "image/png", alpha: "preserved", width: 3, height: 5,
			input: encodePNG(t, 3, 5, true)},
		{name: "PNG one pixel axis", mime: "image/png", alpha: "preserved", width: 1, height: 7,
			input: encodePNG(t, 1, 7, true)},
		{name: "PNG 16-bit ICC gradient", mime: "image/png", alpha: "opaque", width: 257, height: 3,
			input: encodePNG16ICC(t, p3ICC), inputColor: "embedded-icc", toneMap: "not-needed",
			referencePixel: displayP3GradientReference(128)},
		{name: "PNG alpha resize and unpremultiply", mime: "image/png", alpha: "preserved", width: 1920, height: 4,
			input: encodeAlphaResizePNG(t), alphaReference: true},
		{name: "BMP32 reserved byte opaque", mime: "image/bmp", alpha: "opaque", width: 3, height: 2,
			input: encodeBMP32(3, 2)},
		{name: "BMP24 padded bottom-up", mime: "image/bmp", alpha: "opaque", width: 33, height: 20,
			input: encodeBMP24(33, 20, false), bmpReference: true},
		{name: "BMP24 padded top-down", mime: "image/bmp", alpha: "opaque", width: 33, height: 20,
			input: encodeBMP24(33, 20, true), bmpReference: true},
		{name: "libheif real HEIC", mime: "image/heic", alpha: "opaque", input: heic},
		{name: "Google gallery static WebP", mime: "image/webp", alpha: "opaque", input: webp},
		{name: "real Exif orientation 6", mime: "image/jpeg", alpha: "opaque", width: 1800, height: 1200,
			input: orientation6, orientation6: true},
		{name: "real camera RAW", mime: "image/dng", alpha: "opaque", input: rawDNG,
			inputColor: "raw-camera-matrix", toneMap: "not-needed",
			rawProcessing: "camera-wb-camera-matrix-16bit-no-auto-bright", largeRAW: true, rawReference: true},
		{name: "real PQ NCLX reference pixel", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: pq, inputColor: "nclx-pq", toneMap: "bt2446a-method-a", referencePixel: []int{127, 127, 127}},
		{name: "real HLG NCLX reference pixel", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: hlg, inputColor: "nclx-hlg", toneMap: "bt2446a-method-a", referencePixel: []int{100, 100, 100}},
		{name: "wide-gamut ICC and NCLX single normalization", mime: "image/heif", alpha: "opaque", width: 32, height: 32,
			input: iccNCLX, inputColor: "embedded-icc", toneMap: "not-needed", referencePixel: []int{215, 93, 31}},
		{name: "HEIF collection selects explicit primary", mime: "image/heif", alpha: "opaque", width: 16, height: 16,
			input: heifCollection, referencePixel: []int{20, 220, 20}},
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
			recipe := profile.StandardV1Parameters().Recipes[test.mime]
			if test.thumbnail {
				recipe = profile.ThumbnailV1Parameters().Recipes[test.mime]
			}
			result, err := processor.Transform(context.Background(), Request{
				Input: input, Output: output, MIMEType: test.mime,
				Recipe: recipe,
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
			if test.quality != 0 && (result.Quality != test.quality || result.MaxLongEdge != 640 || result.BitDepth != 8) {
				t.Fatalf("thumbnail encoding = %+v", result)
			}
			probeAVIF(t, ffprobe, outputPath, result.Width, result.Height)
			if test.orientation6 {
				assertOrientation6Pixels(t, ffmpeg, test.input, outputPath, result.Width, result.Height)
			}
			if test.referencePixel != nil {
				assertReferencePixel(t, ffmpeg, outputPath, result.Width, result.Height, test.referencePixel)
			}
			if test.alphaReference {
				assertAVIF444(t, ffprobe, outputPath)
				assertAlphaResizePixels(t, ffmpeg, outputPath, result.Width, result.Height)
			}
			if test.rawReference {
				assertRAWReference(t, rawReference, ffmpeg, inputPath, outputPath, result.Width, result.Height)
			}
			if test.bmpReference {
				assertBMPPixels(t, ffmpeg, outputPath, result.Width, result.Height)
			}
		})
	}

	for name, malformed := range map[string][]byte{
		"truncated padded BMP":    encodeBMP24(3, 2, false)[:64],
		"overflow BMP dimensions": malformedBMPOverflow(),
	} {
		t.Run(name+" fails closed", func(t *testing.T) {
			input, output := testFiles(t, malformed)
			_, err := processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/bmp",
				Recipe: profile.StandardV1Parameters().Recipes["image/bmp"]})
			if !errors.Is(err, ErrDecode) {
				t.Fatalf("malformed BMP error = %v", err)
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

	t.Run("invalid embedded ICC fails closed", func(t *testing.T) {
		input, output := testFiles(t, invalidICC)
		_, err := processor.Transform(context.Background(), Request{
			Input: input, Output: output, MIMEType: "image/heif",
			Recipe: profile.StandardV1Parameters().Recipes["image/heif"],
		})
		if !errors.Is(err, ErrDecode) {
			t.Fatalf("invalid ICC error = %v", err)
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

func decodeAVIFPixels(t *testing.T, ffmpeg, outputPath, pixelFormat string, channels int) []byte {
	t.Helper()
	decoded, err := exec.Command(ffmpeg, "-v", "error", "-i", outputPath, "-f", "rawvideo", "-pix_fmt", pixelFormat, "pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg %s decode: %v", pixelFormat, err)
	}
	if len(decoded)%channels != 0 {
		t.Fatalf("decoded %s bytes = %d", pixelFormat, len(decoded))
	}
	return decoded
}

func assertAlphaResizePixels(t *testing.T, ffmpeg, outputPath string, width, height int) {
	t.Helper()
	decoded := decodeAVIFPixels(t, ffmpeg, outputPath, "rgba", 4)
	if len(decoded) != width*height*4 {
		t.Fatalf("decoded RGBA bytes = %d, want %d", len(decoded), width*height*4)
	}
	assert := func(x int, expected [4]int, tolerance int) {
		offset := ((height/2)*width + x) * 4
		for channel := range 4 {
			difference := int(decoded[offset+channel]) - expected[channel]
			if difference < 0 {
				difference = -difference
			}
			if difference > tolerance {
				t.Fatalf("RGBA at x=%d channel=%d is %d, want %d (+/-%d)", x, channel, decoded[offset+channel], expected[channel], tolerance)
			}
		}
	}
	assert(width/6, [4]int{20, 40, 220, 255}, 28)
	assert(width/2, [4]int{20, 220, 40, 96}, 32)
	assert(5*width/6, [4]int{0, 0, 0, 0}, 12)
	transition := false
	for x := 2*width/3 - 4; x <= 2*width/3+4; x++ {
		offset := ((height/2)*width + x) * 4
		alpha := decoded[offset+3]
		if alpha > 8 && alpha < 120 {
			transition = true
			if int(decoded[offset]) > int(decoded[offset+1])+24 {
				t.Fatalf("straight-alpha red fringe at x=%d: rgba=%v", x, decoded[offset:offset+4])
			}
		}
	}
	if !transition {
		t.Fatal("alpha resize did not expose a semitransparent boundary pixel")
	}
}

func assertAVIF444(t *testing.T, ffprobe, outputPath string) {
	t.Helper()
	output, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=pix_fmt", "-of", "default=nw=1:nk=1", outputPath).Output()
	if err != nil {
		t.Fatalf("ffprobe alpha chroma: %v", err)
	}
	format := strings.TrimSpace(string(output))
	if !strings.Contains(format, "444") && !strings.HasPrefix(format, "gbr") {
		t.Fatalf("alpha AVIF pixel format = %q, want 4:4:4", format)
	}
}

func assertBMPPixels(t *testing.T, ffmpeg, outputPath string, width, height int) {
	t.Helper()
	decoded := decodeAVIFPixels(t, ffmpeg, outputPath, "rgb24", 3)
	if len(decoded) != width*height*3 {
		t.Fatalf("decoded BMP bytes = %d", len(decoded))
	}
	for _, point := range []struct {
		x, y int
		rgb  [3]int
	}{{width / 4, height / 4, [3]int{220, 20, 20}}, {3 * width / 4, height / 4, [3]int{220, 180, 20}},
		{width / 4, 3 * height / 4, [3]int{20, 20, 220}}, {3 * width / 4, 3 * height / 4, [3]int{20, 180, 220}}} {
		offset := (point.y*width + point.x) * 3
		for channel := range 3 {
			difference := int(decoded[offset+channel]) - point.rgb[channel]
			if difference < 0 {
				difference = -difference
			}
			if difference > 36 {
				t.Fatalf("BMP pixel (%d,%d) channel %d = %d, want %d", point.x, point.y, channel, decoded[offset+channel], point.rgb[channel])
			}
		}
	}
}

func assertRAWReference(t *testing.T, reference, ffmpeg, inputPath, outputPath string, width, height int) {
	t.Helper()
	referencePath := filepath.Join(t.TempDir(), "raw-reference.rgb")
	command := exec.Command(reference, inputPath, referencePath, fmt.Sprint(width), fmt.Sprint(height))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("RAW reference: %v: %s", err, output)
	}
	want, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAVIFPixels(t, ffmpeg, outputPath, "rgb24", 3)
	if len(want) != width*height*3 || len(got) != len(want) {
		t.Fatalf("RAW reference bytes want=%d got=%d", len(want), len(got))
	}
	total, samples := 0, 0
	for y := height / 10; y < height; y += max(1, height/7) {
		for x := width / 10; x < width; x += max(1, width/7) {
			offset := (y*width + x) * 3
			for channel := range 3 {
				difference := int(got[offset+channel]) - int(want[offset+channel])
				if difference < 0 {
					difference = -difference
				}
				if difference > 80 {
					t.Fatalf("RAW reference pixel (%d,%d) channel %d difference = %d", x, y, channel, difference)
				}
				total += difference
				samples++
			}
		}
	}
	if samples == 0 || total/samples > 24 {
		t.Fatalf("RAW mean absolute difference = %d over %d samples", total/max(1, samples), samples)
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

func encodePNG16ICC(t *testing.T, icc []byte) []byte {
	t.Helper()
	image := image.NewNRGBA64(image.Rect(0, 0, 257, 3))
	for y := range 3 {
		for x := range 257 {
			image.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x*251 + (x%7)*13), G: uint16(65535 - x*199), B: uint16(10000 + x*113), A: 65535})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(icc); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte("nmcp\x00\x00"), compressed.Bytes()...)
	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk, uint32(len(data)))
	copy(chunk[4:], "iCCP")
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(chunk[4:8+len(data)]))
	result := encoded.Bytes()
	if len(result) < 33 || string(result[12:16]) != "IHDR" {
		t.Fatal("unexpected PNG encoding")
	}
	withICC := make([]byte, 0, len(result)+len(chunk))
	withICC = append(withICC, result[:33]...)
	withICC = append(withICC, chunk...)
	withICC = append(withICC, result[33:]...)
	return withICC
}

func displayP3GradientReference(x int) []int {
	encoded := [3]float64{
		float64(x*251+(x%7)*13) / 65535,
		float64(65535-x*199) / 65535,
		float64(10000+x*113) / 65535,
	}
	linear := [3]float64{}
	for channel, value := range encoded {
		if value <= 0.04045 {
			linear[channel] = value / 12.92
		} else {
			linear[channel] = math.Pow((value+0.055)/1.055, 2.4)
		}
	}
	xyz := [3]float64{
		0.48657095*linear[0] + 0.26566769*linear[1] + 0.19821729*linear[2],
		0.22897456*linear[0] + 0.69173852*linear[1] + 0.07928691*linear[2],
		0.04511338*linear[1] + 1.04394437*linear[2],
	}
	srgb := [3]float64{
		3.2404542*xyz[0] - 1.5371385*xyz[1] - 0.4985314*xyz[2],
		-0.9692660*xyz[0] + 1.8760108*xyz[1] + 0.0415560*xyz[2],
		0.0556434*xyz[0] - 0.2040259*xyz[1] + 1.0572252*xyz[2],
	}
	result := make([]int, 3)
	for channel, value := range srgb {
		value = min(1.0, max(0.0, value))
		if value <= 0.0031308 {
			value *= 12.92
		} else {
			value = 1.055*math.Pow(value, 1/2.4) - 0.055
		}
		result[channel] = int(math.Round(value * 255))
	}
	return result
}

func encodeAlphaResizePNG(t *testing.T) []byte {
	t.Helper()
	const width, height = 2000, 4
	image := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			pixel := color.NRGBA{R: 20, G: 40, B: 220, A: 255}
			if x >= width/3 && x < 2*width/3 {
				pixel = color.NRGBA{R: 20, G: 220, B: 40, A: 96}
			} else if x >= 2*width/3 {
				pixel = color.NRGBA{R: 220, G: 20, B: 40, A: 0}
			}
			image.SetNRGBA(x, y, pixel)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image); err != nil {
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

func encodeBMP24(width, height int, topDown bool) []byte {
	const headerSize = 54
	stride := (width*3 + 3) &^ 3
	result := make([]byte, headerSize+stride*height)
	copy(result, "BM")
	binary.LittleEndian.PutUint32(result[2:], uint32(len(result)))
	binary.LittleEndian.PutUint32(result[10:], headerSize)
	binary.LittleEndian.PutUint32(result[14:], 40)
	binary.LittleEndian.PutUint32(result[18:], uint32(width))
	encodedHeight := int32(height)
	if topDown {
		encodedHeight = -encodedHeight
	}
	binary.LittleEndian.PutUint32(result[22:], uint32(encodedHeight))
	binary.LittleEndian.PutUint16(result[26:], 1)
	binary.LittleEndian.PutUint16(result[28:], 24)
	binary.LittleEndian.PutUint32(result[34:], uint32(stride*height))
	for row := range height {
		logicalRow := row
		if !topDown {
			logicalRow = height - 1 - row
		}
		for column := range width {
			offset := headerSize + row*stride + column*3
			green := byte(20)
			if column >= width/2 {
				green = 180
			}
			if logicalRow < height/2 {
				result[offset], result[offset+1], result[offset+2] = 20, green, 220
			} else {
				result[offset], result[offset+1], result[offset+2] = 220, green, 20
			}
		}
		for padding := width * 3; padding < stride; padding++ {
			result[headerSize+row*stride+padding] = 0xa5
		}
	}
	return result
}

func malformedBMPOverflow() []byte {
	result := make([]byte, 54)
	copy(result, "BM")
	binary.LittleEndian.PutUint32(result[2:], uint32(len(result)))
	binary.LittleEndian.PutUint32(result[10:], 54)
	binary.LittleEndian.PutUint32(result[14:], 40)
	binary.LittleEndian.PutUint32(result[18:], uint32(1<<31-1))
	binary.LittleEndian.PutUint32(result[22:], uint32(1<<31-1))
	binary.LittleEndian.PutUint16(result[26:], 1)
	binary.LittleEndian.PutUint16(result[28:], 32)
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
