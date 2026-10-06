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
		wantOrdinaryVideos, wantAttachedPictures                       int
	}{
		{"avc odd audio", "TEST_VIDEO_AVC_PATH", "video/mp4", "h264", true, 0, 0, 1, 320, 180, 321, 181, 44100, false, -1, -1},
		{"hevc rotated no audio", "TEST_VIDEO_HEVC_ROTATED_PATH", "video/quicktime", "hevc", false, 90, 0, -1, 96, 128, 96, 128, 0, false, -1, -1},
		{"pq hdr", "TEST_VIDEO_PQ_PATH", "video/mp4", "hevc", false, 0, 0, -1, 320, 180, 320, 180, 0, true, -1, -1},
		{"hlg hdr", "TEST_VIDEO_HLG_PATH", "video/mp4", "hevc", false, 0, 0, -1, 320, 180, 320, 180, 0, true, -1, -1},
		{"multiple streams", "TEST_VIDEO_MULTISTREAM_PATH", "video/mp4", "h264", true, 0, 0, 2, 64, 48, 64, 48, 48000, false, -1, -1},
		{"later default rotated stream", "TEST_VIDEO_MULTISTREAM_ROTATED_PATH", "video/mp4", "h264", false, 90, 1, -1, 80, 120, 80, 120, 0, false, 2, 1},
		{"vfr sar", "TEST_VIDEO_VFR_PATH", "video/mp4", "h264", false, 0, 0, -1, 320, 90, 320, 90, 0, false, -1, -1},
		{"audio resample", "TEST_VIDEO_RESAMPLE_PATH", "video/quicktime", "h264", true, 0, 0, 1, 80, 50, 80, 50, 48000, false, -1, -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := requiredAbsoluteEnv(t, test.env)
			independentInputSelection(t, ffprobe, path, test.wantCodec, test.wantVideo, test.wantAudioIndex, test.wantHDR, test.wantRotation, test.wantOrdinaryVideos, test.wantAttachedPictures)
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			inspection, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: test.mime})
			if err != nil {
				if diagnostic, diagnosticErr := exec.Command(ffprobe, "-v", "error", "-show_streams", "-show_format", "-show_frames", "-show_entries", "frame=media_type,pts,best_effort_timestamp,duration,pkt_duration,width,height:stream:format", "-of", "json", path).CombinedOutput(); diagnosticErr == nil {
					t.Logf("input diagnostic: %s", diagnostic)
				}
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
					if result.OutputMIME != target.mime || result.Source.VideoStreamIndex != inspection.VideoStreamIndex || result.Audit.SelectedVideoStream != test.wantVideo || result.Audit.Orientation != "identity" {
						t.Fatalf("result = %#v", result)
					}
					wantWidth, wantHeight := test.standardWidth, test.standardHeight
					if target.mime == "image/avif" {
						wantWidth, wantHeight = test.thumbnailWidth, test.thumbnailHeight
					}
					if result.Width != wantWidth || result.Height != wantHeight {
						t.Fatalf("dimensions = %dx%d, want %dx%d", result.Width, result.Height, wantWidth, wantHeight)
					}
					if (target.mime == "video/mp4" && result.OutputDurationUS <= 0) || (target.mime == "image/avif" && result.OutputDurationUS != 0) {
						t.Fatalf("verified output duration = %d for %s", result.OutputDurationUS, target.mime)
					}
					independentProbeAndDecode(t, ffprobe, ffmpeg, output.Name(), target.mime, test.wantAudio && target.mime == "video/mp4", wantWidth, wantHeight, test.outputAudioRate)
				})
			}
		})
	}

	t.Run("PQ later frame metadata absence fails closed", func(t *testing.T) {
		path := requiredAbsoluteEnv(t, "TEST_VIDEO_PQ_LATER_MISSING_PATH")
		assertFirstPresentLaterMissingHDR(t, ffprobe, path)
		input, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		if _, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "video/mp4"}); err == nil {
			t.Fatal("Inspect accepted PQ input with missing later-frame HDR evidence")
		}
	})

	for _, test := range []struct {
		name, env, mime string
		videoIndex      int
	}{
		{"avc", "TEST_VIDEO_AVC_PATH", "video/mp4", 0},
		{"hevc", "TEST_VIDEO_HEVC_ROTATED_PATH", "video/quicktime", 0},
	} {
		t.Run(test.name+" recoverable corruption fails closed", func(t *testing.T) {
			corrupted := corruptSelectedMiddleVideoPacket(t, ffprobe, requiredAbsoluteEnv(t, test.env), test.videoIndex)
			assertRecoverableOnlyWithoutXError(t, ffmpeg, corrupted, test.videoIndex)

			input, err := os.Open(corrupted)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := processor.Inspect(context.Background(), InspectRequest{Input: input, MIMEType: test.mime}); err == nil {
				t.Fatal("Inspect accepted recoverably corrupt input")
			}
			if err := input.Close(); err != nil {
				t.Fatal(err)
			}

			input, err = os.Open(corrupted)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := os.CreateTemp(t.TempDir(), "corrupt-output-*")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			_, err = processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: test.mime, Recipe: profile.StandardV1Parameters().Recipes[test.mime]})
			if err == nil {
				t.Fatal("Transform accepted recoverably corrupt input")
			}
			info, statErr := output.Stat()
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Size() != 0 {
				t.Fatalf("failed Transform output size = %d", info.Size())
			}
		})
	}
}

func assertFirstPresentLaterMissingHDR(t *testing.T, ffprobe, path string) {
	t.Helper()
	data, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_frames", "-show_entries", "frame=media_type:frame_side_data=max_luminance,max_content", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("probe HDR frames: %v", err)
	}
	var document struct {
		Frames []struct {
			SideData []map[string]interface{} `json:"side_data_list"`
		} `json:"frames"`
	}
	if json.Unmarshal(data, &document) != nil || len(document.Frames) < 2 {
		t.Fatalf("invalid HDR frame fixture: %s", data)
	}
	hasPair := func(sideData []map[string]interface{}) bool {
		mastering, cll := false, false
		for _, side := range sideData {
			_, masteringFact := side["max_luminance"]
			_, cllFact := side["max_content"]
			mastering = mastering || masteringFact
			cll = cll || cllFact
		}
		return mastering && cll
	}
	if !hasPair(document.Frames[0].SideData) {
		t.Fatalf("first frame lacks complete HDR evidence: %s", data)
	}
	for _, frame := range document.Frames[1:] {
		if !hasPair(frame.SideData) {
			return
		}
	}
	t.Fatalf("fixture lacks a later frame with missing HDR evidence: %s", data)
}

func independentInputSelection(t *testing.T, ffprobe, path, codec string, videoIndex, audioIndex int, hdr bool, rotation, wantOrdinaryVideos, wantAttachedPictures int) {
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
	ordinaryVideos, attachedPictures := 0, 0
	for _, stream := range document.Streams {
		if stream.CodecType == "video" && stream.Disposition.Attached == 0 {
			ordinaryVideos++
			candidateDefault := stream.Disposition.Default == 1
			if selectedVideo < 0 || candidateDefault && !videoDefault || candidateDefault == videoDefault && stream.Index < selectedVideo {
				selectedVideo, videoDefault = stream.Index, candidateDefault
			}
		}
		if stream.CodecType == "video" && stream.Disposition.Attached == 1 {
			attachedPictures++
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
	if wantOrdinaryVideos >= 0 && (ordinaryVideos != wantOrdinaryVideos || attachedPictures != wantAttachedPictures) {
		t.Fatalf("independent video stream identities ordinary=%d attached_pic=%d", ordinaryVideos, attachedPictures)
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

func corruptSelectedMiddleVideoPacket(t *testing.T, ffprobe, path string, videoIndex int) string {
	t.Helper()
	data, err := exec.Command(ffprobe, "-v", "error", "-select_streams", strconv.Itoa(videoIndex), "-show_packets", "-show_entries", "packet=stream_index,pos,size", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("packet ffprobe: %v", err)
	}
	var document struct {
		Packets []struct {
			StreamIndex int    `json:"stream_index"`
			Position    string `json:"pos"`
			Size        string `json:"size"`
		} `json:"packets"`
	}
	if json.Unmarshal(data, &document) != nil || len(document.Packets) < 3 {
		t.Fatalf("packet probe JSON: %s", data)
	}
	packet := document.Packets[len(document.Packets)/2]
	position, positionErr := strconv.ParseInt(packet.Position, 10, 64)
	size, sizeErr := strconv.ParseInt(packet.Size, 10, 64)
	const payloadOffset, corruptionBytes = int64(6), int64(128)
	if positionErr != nil || sizeErr != nil || packet.StreamIndex != videoIndex || position < 0 || size < payloadOffset+corruptionBytes {
		t.Fatalf("selected middle packet = %#v", packet)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start, end := position+payloadOffset, position+payloadOffset+corruptionBytes
	if end > int64(len(contents)) {
		t.Fatalf("selected middle packet range [%d,%d) exceeds input size %d", start, end, len(contents))
	}
	for index := start; index < end; index++ {
		contents[index] = 0x55
	}
	corrupted := filepath.Join(t.TempDir(), "corrupted"+filepath.Ext(path))
	if err := os.WriteFile(corrupted, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return corrupted
}

func assertRecoverableOnlyWithoutXError(t *testing.T, ffmpeg, path string, videoIndex int) {
	t.Helper()
	decodeArgs := []string{"-v", "error", "-i", path, "-map", "0:" + strconv.Itoa(videoIndex), "-f", "null", "-"}
	output, err := exec.Command(ffmpeg, decodeArgs...).CombinedOutput()
	if err != nil || len(output) == 0 {
		t.Fatalf("independent recovery without -xerror: %v: %s", err, output)
	}
	strictArgs := append([]string{"-v", "error", "-xerror"}, decodeArgs[2:]...)
	if output, err = exec.Command(ffmpeg, strictArgs...).CombinedOutput(); err == nil || len(output) == 0 {
		t.Fatalf("independent decode with -xerror did not fail: %v: %s", err, output)
	}
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
