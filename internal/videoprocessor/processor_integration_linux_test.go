//go:build linux

package videoprocessor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func TestVideoHelperRealCodecMatrix(t *testing.T) {
	if os.Getenv("TEST_VIDEO_HELPER") != "1" {
		t.Skip("set TEST_VIDEO_HELPER=1")
	}
	helper := requiredAbsoluteEnv(t, "TEST_VIDEO_HELPER_PATH")
	prlimit := requiredAbsoluteEnv(t, "TEST_VIDEO_PRLIMIT_PATH")
	icc := requiredAbsoluteEnv(t, "TEST_VIDEO_ICC_PATH")
	ffprobe := requiredAbsoluteEnv(t, "TEST_FFPROBE_PATH")
	ffmpeg := requiredAbsoluteEnv(t, "TEST_FFMPEG_PATH")
	digest := fileSHA256(t, icc)
	processor, err := New(Config{Helper: helper, Prlimit: prlimit, SRGBICC: icc, SRGBICCSHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := processor.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.VideoEncoder != "libsvtav1" || capabilities.AudioEncoder != "aac-lc" {
		t.Fatalf("capabilities = %#v", capabilities)
	}

	tests := []struct {
		name, env, mime string
		wantCodec       string
		wantAudio       bool
		wantRotation    int
	}{
		{"avc odd audio", "TEST_VIDEO_AVC_PATH", "video/mp4", "h264", true, 0},
		{"hevc rotated no audio", "TEST_VIDEO_HEVC_ROTATED_PATH", "video/quicktime", "hevc", false, 90},
		{"pq hdr", "TEST_VIDEO_PQ_PATH", "video/mp4", "hevc", false, 0},
		{"hlg hdr", "TEST_VIDEO_HLG_PATH", "video/mp4", "hevc", false, 0},
		{"multiple streams", "TEST_VIDEO_MULTISTREAM_PATH", "video/mp4", "h264", true, 0},
		{"vfr sar", "TEST_VIDEO_VFR_PATH", "video/mp4", "h264", false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := requiredAbsoluteEnv(t, test.env)
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			inspection, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: test.mime})
			if err != nil {
				t.Fatal(err)
			}
			if inspection.VideoCodec != test.wantCodec || inspection.AudioPresent != test.wantAudio || inspection.RotationDegrees != test.wantRotation {
				t.Fatalf("inspection = %#v", inspection)
			}
			for _, target := range []struct {
				name   string
				recipe profile.Recipe
				mime   string
			}{
				{"standard", profile.StandardV1Parameters().Recipes[test.mime], "video/mp4"},
				{"thumbnail", profile.ThumbnailV1Parameters().Recipes[test.mime], "image/avif"},
			} {
				t.Run(target.name, func(t *testing.T) {
					if _, err := input.Seek(0, 0); err != nil {
						t.Fatal(err)
					}
					output, err := os.CreateTemp(t.TempDir(), "output-*")
					if err != nil {
						t.Fatal(err)
					}
					defer output.Close()
					result, err := processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: test.mime, Recipe: target.recipe})
					if err != nil {
						t.Fatal(err)
					}
					if result.OutputMIME != target.mime || result.Source.VideoStreamIndex != inspection.VideoStreamIndex || result.Audit.Orientation != "identity" {
						t.Fatalf("result = %#v", result)
					}
					independentProbeAndDecode(t, ffprobe, ffmpeg, output.Name(), target.mime, test.wantAudio && target.mime == "video/mp4")
				})
			}
		})
	}
}

func independentProbeAndDecode(t *testing.T, ffprobe, ffmpeg, path, mime string, audio bool) {
	t.Helper()
	command := exec.Command(ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path)
	data, err := command.Output()
	if err != nil {
		t.Fatalf("independent ffprobe: %v", err)
	}
	var document struct {
		Streams []struct {
			CodecName      string `json:"codec_name"`
			CodecType      string `json:"codec_type"`
			ColorPrimaries string `json:"color_primaries"`
			ColorTransfer  string `json:"color_transfer"`
			ColorSpace     string `json:"color_space"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
		} `json:"streams"`
	}
	if json.Unmarshal(data, &document) != nil || len(document.Streams) == 0 {
		t.Fatalf("independent probe = %s", data)
	}
	video, audioCount := 0, 0
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			video++
			if stream.CodecName != "av1" || stream.Width <= 0 || stream.Height <= 0 {
				t.Fatalf("video stream = %#v", stream)
			}
		case "audio":
			audioCount++
			if stream.CodecName != "aac" {
				t.Fatalf("audio stream = %#v", stream)
			}
		}
	}
	if video != 1 || audioCount != map[bool]int{true: 1, false: 0}[audio] {
		t.Fatalf("streams video=%d audio=%d", video, audioCount)
	}
	decode := exec.Command(ffmpeg, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	if output, err := decode.CombinedOutput(); err != nil {
		t.Fatalf("independent decode: %v: %s", err, output)
	}
}

func requiredAbsoluteEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be absolute", name)
	}
	return value
}
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256Sum(data)
	return sum
}
func sha256Sum(data []byte) string { sum := sha256.Sum256(data); return fmt.Sprintf("%x", sum[:]) }
