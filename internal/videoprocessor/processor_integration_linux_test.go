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
	"strconv"
	"strings"
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
		name, env, mime                                                string
		wantCodec                                                      string
		wantAudio                                                      bool
		wantRotation, wantVideo, wantAudioIndex                        int
		standardWidth, standardHeight, thumbnailWidth, thumbnailHeight int
		outputAudioRate                                                int
		wantHDR                                                        bool
	}{
		{"avc odd audio", "TEST_VIDEO_AVC_PATH", "video/mp4", "h264", true, 0, 0, 1, 320, 180, 321, 181, 44100, false},
		{"hevc rotated no audio", "TEST_VIDEO_HEVC_ROTATED_PATH", "video/quicktime", "hevc", false, 90, 0, -1, 46, 62, 46, 62, 0, false},
		{"pq hdr", "TEST_VIDEO_PQ_PATH", "video/mp4", "hevc", false, 0, 0, -1, 320, 180, 320, 180, 0, true},
		{"hlg hdr", "TEST_VIDEO_HLG_PATH", "video/mp4", "hevc", false, 0, 0, -1, 320, 180, 320, 180, 0, true},
		{"multiple streams", "TEST_VIDEO_MULTISTREAM_PATH", "video/mp4", "h264", true, 0, 1, 3, 64, 48, 64, 48, 48000, false},
		{"vfr sar", "TEST_VIDEO_VFR_PATH", "video/mp4", "h264", false, 0, 0, -1, 320, 90, 320, 90, 0, false},
		{"audio resample", "TEST_VIDEO_RESAMPLE_PATH", "video/quicktime", "h264", true, 0, 0, 1, 80, 50, 80, 50, 48000, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := requiredAbsoluteEnv(t, test.env)
			independentInputSelection(t, ffprobe, path, test.wantCodec, test.wantVideo, test.wantAudioIndex, test.wantHDR, test.wantRotation)
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			inspection, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: test.mime})
			if err != nil {
				t.Fatal(err)
			}
			if inspection.VideoCodec != test.wantCodec || inspection.AudioPresent != test.wantAudio || inspection.RotationDegrees != test.wantRotation || inspection.VideoStreamIndex != test.wantVideo || inspection.AudioStreamIndex != test.wantAudioIndex || inspection.HDR != test.wantHDR {
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
					wantWidth, wantHeight := test.standardWidth, test.standardHeight
					if target.mime == "image/avif" {
						wantWidth, wantHeight = test.thumbnailWidth, test.thumbnailHeight
					}
					if result.Width != wantWidth || result.Height != wantHeight {
						t.Fatalf("dimensions = %dx%d, want %dx%d", result.Width, result.Height, wantWidth, wantHeight)
					}
					independentProbeAndDecode(t, ffprobe, ffmpeg, output.Name(), target.mime, test.wantAudio && target.mime == "video/mp4", wantWidth, wantHeight, test.outputAudioRate)
				})
			}
		})
	}
}

func independentInputSelection(t *testing.T, ffprobe, path, codec string, videoIndex, audioIndex int, hdr bool, rotation int) {
	t.Helper()
	data, err := exec.Command(ffprobe, "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("input ffprobe: %v", err)
	}
	var document struct {
		Streams []struct {
			Index         int    `json:"index"`
			CodecName     string `json:"codec_name"`
			CodecType     string `json:"codec_type"`
			ColorTransfer string `json:"color_transfer"`
			Disposition   struct {
				Default  int `json:"default"`
				Attached int `json:"attached_pic"`
			} `json:"disposition"`
			Tags     map[string]string `json:"tags"`
			SideData []map[string]any  `json:"side_data_list"`
		} `json:"streams"`
	}
	if json.Unmarshal(data, &document) != nil {
		t.Fatalf("input probe JSON: %s", data)
	}
	selectedVideo, selectedAudio := -1, -1
	videoDefault, audioDefault := false, false
	for _, stream := range document.Streams {
		if stream.CodecType == "video" && stream.Disposition.Attached == 0 {
			candidateDefault := stream.Disposition.Default == 1
			if selectedVideo < 0 || candidateDefault && !videoDefault || candidateDefault == videoDefault && stream.Index < selectedVideo {
				selectedVideo, videoDefault = stream.Index, candidateDefault
			}
		}
		if stream.CodecType == "audio" {
			candidateDefault := stream.Disposition.Default == 1
			if selectedAudio < 0 || candidateDefault && !audioDefault || candidateDefault == audioDefault && stream.Index < selectedAudio {
				selectedAudio, audioDefault = stream.Index, candidateDefault
			}
		}
	}
	if selectedVideo != videoIndex || selectedAudio != audioIndex {
		t.Fatalf("independent selection video=%d audio=%d", selectedVideo, selectedAudio)
	}
	for _, stream := range document.Streams {
		if stream.Index == videoIndex {
			if stream.CodecName != codec || (stream.ColorTransfer == "smpte2084" || stream.ColorTransfer == "arib-std-b67") != hdr {
				t.Fatalf("selected input = %#v", stream)
			}
			observed := 0
			for _, side := range stream.SideData {
				if value, ok := side["rotation"].(float64); ok {
					observed = int(value)
					if observed < 0 {
						observed += 360
					}
				}
			}
			if tag := stream.Tags["rotate"]; tag != "" {
				observed, _ = strconv.Atoi(tag)
			}
			if observed != rotation {
				t.Fatalf("rotation=%d want=%d", observed, rotation)
			}
			return
		}
	}
	t.Fatal("selected input stream missing")
}

func independentProbeAndDecode(t *testing.T, ffprobe, ffmpeg, path, mime string, audio bool, width, height, audioRate int) {
	t.Helper()
	command := exec.Command(ffprobe, "-v", "error", "-count_frames", "-show_streams", "-show_format", "-of", "json", path)
	data, err := command.Output()
	if err != nil {
		t.Fatalf("independent ffprobe: %v", err)
	}
	var document struct {
		Streams []struct {
			CodecName         string            `json:"codec_name"`
			CodecType         string            `json:"codec_type"`
			ColorPrimaries    string            `json:"color_primaries"`
			ColorTransfer     string            `json:"color_transfer"`
			ColorSpace        string            `json:"color_space"`
			Width             int               `json:"width"`
			Height            int               `json:"height"`
			PixelFormat       string            `json:"pix_fmt"`
			SampleAspectRatio string            `json:"sample_aspect_ratio"`
			Profile           string            `json:"profile"`
			SampleRate        string            `json:"sample_rate"`
			ChannelLayout     string            `json:"channel_layout"`
			ReadFrames        string            `json:"nb_read_frames"`
			Tags              map[string]string `json:"tags"`
			SideData          []map[string]any  `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			FormatName string            `json:"format_name"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
	}
	if json.Unmarshal(data, &document) != nil || len(document.Streams) == 0 {
		t.Fatalf("independent probe = %s", data)
	}
	video, audioCount := 0, 0
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			video++
			expectedTransfer, expectedPixel := "bt709", "yuv420p10le"
			if mime == "image/avif" {
				expectedTransfer, expectedPixel = "iec61966-2-1", "yuv420p"
			}
			if stream.CodecName != "av1" || stream.Width != width || stream.Height != height || stream.SampleAspectRatio != "1:1" || stream.PixelFormat != expectedPixel || stream.ColorPrimaries != "bt709" || stream.ColorTransfer != expectedTransfer || stream.ColorSpace != "bt709" || stream.ReadFrames == "0" || stream.Tags["rotate"] != "" || hasIndependentRotation(stream.SideData) {
				t.Fatalf("video stream = %#v", stream)
			}
		case "audio":
			audioCount++
			if stream.CodecName != "aac" || stream.Profile != "LC" || stream.SampleRate != strconv.Itoa(audioRate) || stream.ChannelLayout == "" {
				t.Fatalf("audio stream = %#v", stream)
			}
		}
	}
	if video != 1 || audioCount != map[bool]int{true: 1, false: 0}[audio] {
		t.Fatalf("streams video=%d audio=%d", video, audioCount)
	}
	if mime == "video/mp4" {
		if !strings.Contains(document.Format.FormatName, "mp4") || !strings.Contains(document.Format.Tags["compatible_brands"], "av01") {
			t.Fatalf("format = %#v", document.Format)
		}
	} else if !strings.Contains(document.Format.FormatName, "avif") && !strings.Contains(document.Format.FormatName, "mov") {
		t.Fatalf("AVIF format = %#v", document.Format)
	}
	decode := exec.Command(ffmpeg, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	if output, err := decode.CombinedOutput(); err != nil {
		t.Fatalf("independent decode: %v: %s", err, output)
	}
}

func hasIndependentRotation(sideData []map[string]any) bool {
	for _, side := range sideData {
		if _, ok := side["rotation"]; ok {
			return true
		}
	}
	return false
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
