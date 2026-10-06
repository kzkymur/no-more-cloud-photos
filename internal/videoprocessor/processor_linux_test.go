//go:build linux

package videoprocessor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateVideoRecipes(t *testing.T) {
	standard := profile.StandardV1Parameters().Recipes["video/mp4"]
	thumbnail := profile.ThumbnailV1Parameters().Recipes["video/quicktime"]
	if err := validateRecipe(standard, "video/mp4"); err != nil {
		t.Fatalf("standard: %v", err)
	}
	if err := validateRecipe(thumbnail, "video/quicktime"); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	bad := standard
	copy := *bad.VideoOutput
	copy.VideoCodec = "h264"
	bad.VideoOutput = &copy
	if !errors.Is(validateRecipe(bad, "video/mp4"), ErrInvalid) {
		t.Fatal("H.264 fallback recipe accepted")
	}
}

func TestStrictProtocolRejectsDuplicateNestedField(t *testing.T) {
	data := []byte(`{"protocol":1,"ok":true,"error_code":"","result":{"helper_version":"x","helper_version":"y"}}`)
	var response capabilityResponse
	if decodeProtocolJSON(data, &response) == nil {
		t.Fatal("duplicate field accepted")
	}
}

func TestCapabilitiesClosedEvidence(t *testing.T) {
	p := newTestProcessor(t, helperScript(t, capabilityJSON(testDigest)))
	got, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.VideoEncoder != "libsvtav1" || got.AudioEncoder != "aac-lc" || got.BuildManifest != BuildManifest {
		t.Fatalf("capabilities = %#v", got)
	}
}

func TestTransformUsesDescriptorProtocolAndIndependentVerifier(t *testing.T) {
	inspection := validInspection()
	transform := fmt.Sprintf(`{"protocol":1,"ok":true,"error_code":"","result":{"kind":"mp4-av1","output_mime":"video/mp4","width":320,"height":180,"max_long_edge":1920,"threads":1,"crf":32,"quality":0,"bit_depth":10,"chroma":"4:2:0","audio_present":false,"audio_codec":"none","audio_bitrate_kbps":0,"source":%s,"audit":%s}}`, inspectionJSON(inspection), auditJSON(inspection, testDigest))
	verification := fmt.Sprintf(`{"protocol":1,"ok":true,"error_code":"","result":{"kind":"mp4-av1","container":"mp4","width":320,"height":180,"sample_aspect_ratio":{"numerator":1,"denominator":1},"video_codec":"av1","bit_depth":10,"chroma":"4:2:0","frame_count":2,"pts_delta_sha256":"%s","max_timing_error_ticks":1,"output_duration_us":80000,"max_duration_error_ticks":1,"rotation_degrees":0,"has_display_matrix":false,"has_rotate_metadata":false,"color_primaries":"bt709","color_transfer":"bt709","color_matrix":"bt709","color_range":"limited","audio_present":false,"audio_codec":"none","audio_profile":"none","fully_decoded_video":true,"fully_decoded_audio":false,"source":%s}}`, inspection.PTSDeltaSHA256, inspectionJSON(inspection))
	physical := filepath.Join(t.TempDir(), "physical.mp4")
	if err := os.WriteFile(physical, minimalMP4("av01", false), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
case "$1" in
transform) /bin/cat '` + physical + `' >&4; printf '%s' '` + transform + `' ;;
verify-output) printf '%s' '` + verification + `' ;;
*) exit 9 ;;
esac
`
	p := newTestProcessor(t, helperScript(t, script))
	input := regularFile(t, []byte("source"))
	defer input.Close()
	output := regularFile(t, nil)
	defer output.Close()
	recipe := profile.StandardV1Parameters().Recipes["video/mp4"]
	got, err := p.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "video/mp4", Recipe: recipe})
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputMIME != "video/mp4" || got.Width != 320 || got.Height != 180 || got.Audit.VideoEncoder != "libsvtav1" {
		t.Fatalf("result = %#v", got)
	}
	if offset, _ := output.Seek(0, 1); offset != 0 {
		t.Fatalf("output offset = %d", offset)
	}
}

func TestContainerRejectsFallbackAndMalformedLengths(t *testing.T) {
	for name, data := range map[string][]byte{
		"valid":         minimalMP4("av01", false),
		"h264 fallback": minimalMP4("avc1", false),
		"truncated":     minimalMP4("av01", false)[:20],
	} {
		t.Run(name, func(t *testing.T) {
			file := regularFile(t, data)
			defer file.Close()
			got := validateVideoContainer(file, int64(len(data)), "mp4-av1", false)
			if got != (name == "valid") {
				t.Fatalf("valid = %t", got)
			}
		})
	}
}

func minimalMP4(videoEntry string, audio bool) []byte {
	box := func(kind string, payload ...[]byte) []byte {
		var body []byte
		for _, part := range payload {
			body = append(body, part...)
		}
		result := make([]byte, 8+len(body))
		binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
		copy(result[4:8], kind)
		copy(result[8:], body)
		return result
	}
	entry := box(videoEntry)
	count := make([]byte, 8)
	binary.BigEndian.PutUint32(count[4:], 1)
	stsd := box("stsd", count, entry)
	moov := box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
	brand := append([]byte("isom\x00\x00\x02\x00isomiso6av01mp41"), []byte{}...)
	ftyp := box("ftyp", brand)
	if audio {
		audioEntry := box("mp4a")
		binary.BigEndian.PutUint32(count[4:], 2)
		stsd = box("stsd", count, entry, audioEntry)
		moov = box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
	}
	return bytes.Join([][]byte{ftyp, moov, box("mdat", []byte{1})}, nil)
}

func TestVerifierFailureClearsOutput(t *testing.T) {
	inspection := validInspection()
	transform := fmt.Sprintf(`{"protocol":1,"ok":true,"error_code":"","result":{"kind":"mp4-av1","output_mime":"video/mp4","width":320,"height":180,"max_long_edge":1920,"threads":1,"crf":32,"quality":0,"bit_depth":10,"chroma":"4:2:0","audio_present":false,"audio_codec":"none","audio_bitrate_kbps":0,"source":%s,"audit":%s}}`, inspectionJSON(inspection), auditJSON(inspection, testDigest))
	script := `#!/bin/sh
set -eu
case "$1" in
transform) printf 'partial' >&4; printf '%s' '` + transform + `' ;;
verify-output) printf '%s' '{"protocol":1,"ok":false,"error_code":"decode_failed","result":null}' ;;
esac
`
	p := newTestProcessor(t, helperScript(t, script))
	input := regularFile(t, []byte("source"))
	defer input.Close()
	output := regularFile(t, nil)
	defer output.Close()
	_, err := p.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "video/mp4", Recipe: profile.StandardV1Parameters().Recipes["video/mp4"]})
	if !errors.Is(err, ErrProcess) {
		t.Fatalf("error = %v", err)
	}
	info, _ := output.Stat()
	if info.Size() != 0 {
		t.Fatalf("output size = %d", info.Size())
	}
	if offset, _ := output.Seek(0, 1); offset != 0 {
		t.Fatalf("offset = %d", offset)
	}
}

func TestOperationTimeoutUsesPrivateCause(t *testing.T) {
	script := "#!/bin/sh\nsleep 5\n"
	p := newTestProcessorWithPolicy(t, helperScript(t, script), Policy{Timeout: 30 * time.Millisecond})
	input := regularFile(t, []byte("source"))
	defer input.Close()
	output := regularFile(t, nil)
	defer output.Close()
	_, err := p.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "video/mp4", Recipe: profile.StandardV1Parameters().Recipes["video/mp4"]})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v", err)
	}
}

func TestOperationDeadlinePreservesFirstCause(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	operation, cancelOperation := context.WithTimeoutCause(parent, time.Hour, errOperationTimeout)
	cancelParent()
	<-operation.Done()
	if err := mapOperationError(operation.Err(), parent, operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent-first error=%v", err)
	}
	cancelOperation()

	parent, cancelParent = context.WithCancel(context.Background())
	operation, cancelOperation = context.WithTimeoutCause(parent, time.Nanosecond, errOperationTimeout)
	<-operation.Done()
	cancelParent()
	if err := mapOperationError(operation.Err(), parent, operation); !errors.Is(err, ErrTimeout) {
		t.Fatalf("operation-first error=%v", err)
	}
	cancelOperation()
}

func TestOperationTimeoutReapsDetachedCodecDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	script := "#!/bin/sh\n/usr/bin/setsid /bin/sh -c 'printf %s $$ > \"" + pidFile + "\"; /bin/sleep 30' &\nwhile [ ! -s \"" + pidFile + "\" ]; do /bin/sleep 0.01; done\n/bin/sleep 30\n"
	p := newTestProcessorWithPolicy(t, helperScript(t, script), Policy{Timeout: 150 * time.Millisecond})
	input := regularFile(t, []byte("source"))
	defer input.Close()
	output := regularFile(t, nil)
	defer output.Close()
	_, err := p.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "video/mp4", Recipe: profile.StandardV1Parameters().Recipes["video/mp4"]})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error=%v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		process, _ := os.FindProcess(pid)
		if process.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached codec descendant %d survived", pid)
}

func validInspection() Inspection {
	return Inspection{TotalStreams: 1, VideoStreamIndex: 0, AudioPresent: false, AudioStreamIndex: -1, VideoCodec: "h264", CodedWidth: 320, CodedHeight: 180, DisplayWidth: 320, DisplayHeight: 180, SampleAspectRatio: Rational{1, 1}, RotationDegrees: 0, FrameCount: 2, DurationUS: 80000, EffectiveFPS: Rational{25, 1}, TimeBase: Rational{1, 1000}, FirstPTS: 0, LastPTS: 40, PTSDeltaSHA256: testDigest, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorMatrix: "bt709", ColorRange: "limited", HDR: false, SourcePeakNits: 0, AudioCodec: "none", AudioChannels: 0, AudioChannelLayout: "none", AudioSampleRate: 0, CumulativeDecodedPixels: 115200}
}

func inspectionJSON(i Inspection) string {
	return fmt.Sprintf(`{"total_streams":%d,"video_stream_index":%d,"audio_present":%t,"audio_stream_index":%d,"video_codec":"%s","coded_width":%d,"coded_height":%d,"display_width":%d,"display_height":%d,"sample_aspect_ratio":{"numerator":%d,"denominator":%d},"rotation_degrees":%d,"frame_count":%d,"duration_us":%d,"effective_fps":{"numerator":%d,"denominator":%d},"time_base":{"numerator":%d,"denominator":%d},"first_pts":%d,"last_pts":%d,"pts_delta_sha256":"%s","color_primaries":"%s","color_transfer":"%s","color_matrix":"%s","color_range":"%s","hdr":%t,"source_peak_nits":%d,"mastering_max_nits":%d,"max_cll_nits":%d,"audio_codec":"%s","audio_channels":%d,"audio_channel_layout":"%s","audio_sample_rate":%d,"cumulative_decoded_pixels":%d}`, i.TotalStreams, i.VideoStreamIndex, i.AudioPresent, i.AudioStreamIndex, i.VideoCodec, i.CodedWidth, i.CodedHeight, i.DisplayWidth, i.DisplayHeight, i.SampleAspectRatio.Numerator, i.SampleAspectRatio.Denominator, i.RotationDegrees, i.FrameCount, i.DurationUS, i.EffectiveFPS.Numerator, i.EffectiveFPS.Denominator, i.TimeBase.Numerator, i.TimeBase.Denominator, i.FirstPTS, i.LastPTS, i.PTSDeltaSHA256, i.ColorPrimaries, i.ColorTransfer, i.ColorMatrix, i.ColorRange, i.HDR, i.SourcePeakNits, i.MasteringMaxNits, i.MaxCLLNits, i.AudioCodec, i.AudioChannels, i.AudioChannelLayout, i.AudioSampleRate, i.CumulativeDecodedPixels)
}

func auditJSON(i Inspection, digest string) string {
	versions := `{"ffmpeg":"9.0.2","libsvtav1":"4.2.0","zimg":"3.0.6","libaom":"v3.8.2"}`
	return fmt.Sprintf(`{"tool_version":"nmcp-video-helper/1","build_manifest":"%s","library_versions":%s,"icc_sha256":"%s","demuxer":"mov","video_decoder":"h264","audio_decoder":"none","video_encoder":"libsvtav1","audio_encoder":"none","muxer":"mp4","selected_video_stream":%d,"selected_audio_stream":%d,"stream_selection":"default-first-then-index","rotation_source":"none","rotation_degrees_applied":0,"orientation":"identity","geometry":"display-aspect-square-pixel-even-floor-no-upscale","timing":"preserve-presentation-order-vfr-rebase-zero","video_filter_graph":"zscale=primaries=bt709:transfer=bt709:matrix=bt709:range=limited,scale=320:180:flags=lanczos,setsar=1,format=yuv420p10le,setpts=PTS-STARTPTS","audio_filter_graph":"none","input_color":"bt709/bt709/bt709/limited","output_color":"bt709-sdr-100nit","output_primaries":"bt709","output_transfer":"bt709","output_matrix":"bt709","output_range":"limited","hdr_disposition":"sdr-normalized","tone_map":"not-needed","target_nits":100,"source_peak_nits":0,"input_audio_layout":"none","output_audio_layout":"none","input_audio_sample_rate":0,"output_audio_sample_rate":0,"metadata":"strip-after-normalization-keep-color-tags"}`, BuildManifest, versions, digest, i.VideoStreamIndex, i.AudioStreamIndex)
}

func capabilityJSON(digest string) string {
	return fmt.Sprintf(`#!/bin/sh
printf '%%s' '{"protocol":1,"ok":true,"error_code":"","result":{"helper_version":"nmcp-video-helper/1","library_versions":{"ffmpeg":"9.0.2","libsvtav1":"4.2.0","zimg":"3.0.6","libaom":"v3.8.2"},"decoder_mime_types":["video/mp4","video/quicktime"],"output_kinds":["first-frame-avif","mp4-av1"],"video_encoder":"libsvtav1","audio_encoder":"aac-lc","video_muxer":"mp4","tone_map":"zscale+hable","icc_sha256":"%s","threads":1,"build_manifest":"%s"}}'
`, digest, BuildManifest)
}

func newTestProcessor(t *testing.T, helper string) *Processor {
	return newTestProcessorWithPolicy(t, helper, Policy{})
}
func newTestProcessorWithPolicy(t *testing.T, helper string, policy Policy) *Processor {
	t.Helper()
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit unavailable")
	}
	prlimit, err = filepath.Abs(prlimit)
	if err != nil {
		t.Fatal(err)
	}
	icc := filepath.Join(t.TempDir(), "sRGB.icc")
	content := []byte("icc")
	if err := os.WriteFile(icc, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	// Test helpers use the production digest in responses; rewrite their literal
	// only at the executable boundary so fixture builders stay readable.
	bytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), testDigest) {
		if err := os.WriteFile(helper, []byte(strings.ReplaceAll(string(bytes), testDigest, digest)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(Config{Helper: helper, Prlimit: prlimit, SRGBICC: icc, SRGBICCSHA256: digest, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func helperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
func regularFile(t *testing.T, content []byte) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "media-")
	if err != nil {
		t.Fatal(err)
	}
	if len(content) > 0 {
		if _, err := file.Write(content); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
	}
	return file
}
