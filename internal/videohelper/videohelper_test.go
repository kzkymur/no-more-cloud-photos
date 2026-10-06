//go:build linux

package videohelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseRequestClosedCLIAndLimits(t *testing.T) {
	args := inspectArgs(maxLimits())
	request, err := parseRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	if request.input != "/proc/self/fd/3" || request.limits.DecodedPixels != maxDecodedPixels {
		t.Fatalf("request = %#v", request)
	}
	for _, mutate := range []func([]string) []string{
		func(a []string) []string { a[1] = "--input"; return a },
		func(a []string) []string { a[4] = "/tmp/input"; return a },
		func(a []string) []string { return append(a, "--extra", "1") },
	} {
		copyArgs := slices.Clone(args)
		copyArgs = mutate(copyArgs)
		if _, err := parseRequest(copyArgs); err == nil {
			t.Fatalf("accepted mutated args: %q", copyArgs)
		}
	}
}

func TestParseLimitsExactPlusOneAndOverflow(t *testing.T) {
	base := maxLimitValues()
	for name, value := range map[string]string{
		"--max-streams":                "33",
		"--max-dimension":              "16385",
		"--max-pixels-per-frame":       "268435457",
		"--max-duration-us":            "14400000001",
		"--max-fps-numerator":          "241",
		"--max-frames":                 "3456001",
		"--max-decoded-pixels":         "8000000000001",
		"--max-audio-channels":         "9",
		"--max-audio-sample-rate":      "192001",
		"--max-video-output-bytes":     "8589934593",
		"--max-thumbnail-output-bytes": "67108865",
		"--max-generated-output-bytes": "8589934593",
	} {
		values := cloneStrings(base)
		values[name] = value
		var got limits
		if err := parseLimits(values, &got); err == nil {
			t.Errorf("%s +1 accepted", name)
		}
	}
	values := cloneStrings(base)
	values["--max-decoded-pixels"] = "18446744073709551616"
	if err := parseLimits(values, &limits{}); err == nil {
		t.Fatal("uint64 overflow accepted")
	}
}

func TestPrimarySelectionDefaultThenIndexExcludesAttachedPicture(t *testing.T) {
	streams := []probeStream{
		{Index: 0, CodecType: "video", Disposition: disposition(1, 1)},
		{Index: 4, CodecType: "video", Disposition: disposition(1, 0)},
		{Index: 2, CodecType: "video", Disposition: disposition(1, 0)},
		{Index: 1, CodecType: "video"},
	}
	if got, ok := selectStream(streams, "video"); !ok || got != 2 {
		t.Fatalf("selected %d, %t", got, ok)
	}
}

func TestRotationResolution(t *testing.T) {
	stream := probeStream{Tags: map[string]string{"rotate": "90"}, SideDataList: []map[string]interface{}{{"rotation": float64(90)}}}
	if rotation, source, err := streamRotation(stream); err != nil || rotation != 90 || source != "display-matrix+rotate" {
		t.Fatalf("rotation = %d, %q, %v", rotation, source, err)
	}
	stream.Tags["rotate"] = "180"
	if _, _, err := streamRotation(stream); err == nil {
		t.Fatal("contradictory rotation accepted")
	}
	stream.Tags["rotate"] = "45"
	stream.SideDataList = nil
	if _, _, err := streamRotation(stream); err == nil {
		t.Fatal("non-orthogonal rotation accepted")
	}
}

func TestGeometryEvenFloorAndNoUpscale(t *testing.T) {
	tests := []struct {
		width, height, edge   int
		even                  bool
		wantWidth, wantHeight int
	}{
		{1921, 1081, 1920, true, 1920, 1080},
		{319, 181, 1920, true, 318, 180},
		{1, 1, 640, false, 1, 1},
	}
	for _, test := range tests {
		width, height, err := fitDimensions(test.width, test.height, test.edge, test.even)
		if err != nil || width != test.wantWidth || height != test.wantHeight {
			t.Errorf("fitDimensions(%d,%d) = %d,%d,%v", test.width, test.height, width, height, err)
		}
	}
	if _, _, err := fitDimensions(1, 1, 1920, true); err == nil {
		t.Fatal("unrepresentable even geometry accepted")
	}
}

func TestFrameFactsBoundedStreamingAndAccounting(t *testing.T) {
	frames := "media_type=video|pts=0|pkt_duration=40|width=2|height=2\n" +
		"media_type=video|pts=40|pkt_duration=40|width=2|height=2\n"
	runner := &fakeRunner{streams: []string{frames}}
	e := engine{ffprobe: "/p/ffprobe", run: runner}
	limit := maxLimits()
	limit.DecodedPixels = 8
	facts, err := e.frameFacts("input", 0, 2, 2, rational{1, 1000}, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.pts) != 2 || facts.durationUS != 80000 || facts.decodedPixels != 8 || facts.fps != (rational{25, 1}) {
		t.Fatalf("facts = %#v", facts)
	}
	runner.streams = []string{frames}
	limit.DecodedPixels = 7
	if _, err := e.frameFacts("input", 0, 2, 2, rational{1, 1000}, limit); !isCode(err, "resource_limit") {
		t.Fatalf("decoded pixels +1 error = %v", err)
	}
}

func TestDurationAccountingDoesNotFloorPastLimit(t *testing.T) {
	if duration, within, ok := durationMicroseconds(1, rational{1, 3}, 333333); !ok || within || duration != 333334 {
		t.Fatalf("duration = %d, within=%t, ok=%t", duration, within, ok)
	}
	if duration, within, ok := durationMicroseconds(1, rational{1, 4}, 250000); !ok || !within || duration != 250000 {
		t.Fatalf("exact duration = %d, within=%t, ok=%t", duration, within, ok)
	}
}

func TestInspectUsesProbeAndRejectsAmbiguousColor(t *testing.T) {
	metadata := probeJSON("h264", "yuv420p", 320, 180, false)
	frames := "media_type=video|pts=0|pkt_duration=40|width=320|height=180\nmedia_type=video|pts=40|pkt_duration=40|width=320|height=180\n"
	runner := &fakeRunner{runs: [][]byte{metadata}, streams: []string{frames}}
	e := engine{ffprobe: "/p/ffprobe", run: runner}
	got, _, err := e.inspect("input", "video/mp4", maxLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got.VideoStreamIndex != 0 || got.FrameCount != 2 || got.PTSDeltaSHA256 != hashDeltas([]int64{0, 40}) {
		t.Fatalf("inspection = %#v", got)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(metadata, &document); err != nil {
		t.Fatal(err)
	}
	streams := document["streams"].([]interface{})
	streams[0].(map[string]interface{})["color_primaries"] = "unknown"
	bad, _ := json.Marshal(document)
	runner.runs, runner.streams = [][]byte{bad}, []string{frames}
	if _, _, err := e.inspect("input", "video/mp4", maxLimits()); !isCode(err, "unsupported_input") {
		t.Fatalf("ambiguous color error = %v", err)
	}
}

func TestCapabilitiesRequirePinnedSiblingFeatures(t *testing.T) {
	icc := writeTemp(t, "icc")
	runner := &fakeRunner{runs: [][]byte{
		[]byte("ffprobe version 9.0.2"),
		[]byte("ffmpeg version 9.0.2"),
		[]byte("--disable-autodetect --disable-network --disable-gpl --disable-nonfree --enable-libsvtav1 --enable-libzimg --enable-libaom"),
		[]byte(" h264 hevc aac av1"),
		[]byte(" mov "),
		[]byte(" mp4 avif"),
		[]byte("libsvtav1 libaom-av1 aac"),
		[]byte("zscale tonemap"),
	}}
	e := engine{ffprobe: "/prefix/bin/ffprobe", ffmpeg: "/prefix/bin/ffmpeg", run: runner}
	var response bytes.Buffer
	if err := e.capabilities(request{icc: icc, threads: 1}, &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.String(), `"video_encoder":"libsvtav1"`) || !strings.Contains(response.String(), buildManifest) {
		t.Fatalf("capabilities = %s", response.String())
	}
	runner.runs = [][]byte{
		[]byte("ffprobe version 9.0.2"),
		[]byte("ffmpeg version 9.0.2"),
		[]byte("--disable-autodetect --disable-network --disable-gpl --disable-nonfree --enable-libsvtav1 --enable-libzimg --enable-libaom"),
		[]byte(" h264 hevc aac av1"),
		[]byte(" mov "),
		[]byte(" mp4 avif"),
		[]byte("libaom-av1 aac"),
		[]byte("zscale tonemap"),
	}
	if err := e.capabilities(request{icc: icc, threads: 1}, io.Discard); !isCode(err, "capability_failed") {
		t.Fatalf("missing SVT-AV1 error = %v", err)
	}
}

func TestTransformInvokesSVTAV1WithoutH264FallbackAndTruncatesFailure(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	icc := writeTemp(t, "icc")
	frames := "media_type=video|pts=0|pkt_duration=40|width=320|height=180\nmedia_type=video|pts=40|pkt_duration=40|width=320|height=180\n"
	runner := &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}, writeOutput: []byte("mp4")}
	e := engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
	r := request{command: "transform", input: input.Name(), output: output.Name(), mime: "video/mp4", kind: "mp4-av1", icc: icc, maxLongEdge: 1920, bitDepth: 10, threads: 1, limits: maxLimits()}
	var response bytes.Buffer
	if err := e.transform(r, &response); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.lastFFmpeg, " ")
	if !strings.Contains(joined, "-c:v libsvtav1") || strings.Contains(joined, "libx264") {
		t.Fatalf("ffmpeg args = %q", runner.lastFFmpeg)
	}
	if err := output.Truncate(0); err != nil {
		t.Fatal(err)
	}
	r.generatedBytesBefore = maxGenerated - 2
	runner = &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}, writeOutput: []byte("mp4")}
	e.run = runner
	if err := e.transform(r, io.Discard); !isCode(err, "output_too_large") {
		t.Fatalf("cumulative output error = %v", err)
	}
	r.generatedBytesBefore = 0
	runner = &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}, runErrorAt: 2, writeOutput: []byte("partial")}
	e.run = runner
	if err := e.transform(r, io.Discard); !isCode(err, "encode_failed") {
		t.Fatalf("error = %v", err)
	}
	info, err := output.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("failed output size = %d, %v", info.Size(), err)
	}
}

func TestTimingVerificationAllowsOneTickAndRejectsTwo(t *testing.T) {
	source := []int64{0, 40, 80}
	if got, err := timingErrorTicks(source, rational{1, 1000}, []int64{0, 39, 80}, rational{1, 1000}); err != nil || got != 1 {
		t.Fatalf("one tick = %d, %v", got, err)
	}
	if got, err := timingErrorTicks(source, rational{1, 1000}, []int64{0, 38, 80}, rational{1, 1000}); err != nil || got != 2 {
		t.Fatalf("two ticks = %d, %v", got, err)
	}
}

func TestVerifyOutputIndependentlyProbesAndFullyDecodes(t *testing.T) {
	sourceFrames := "media_type=video|pts=100|pkt_duration=40|width=320|height=180\nmedia_type=video|pts=140|pkt_duration=40|width=320|height=180\n"
	outputFrames := "media_type=video|pts=0|pkt_duration=40|width=320|height=180\nmedia_type=video|pts=40|pkt_duration=40|width=320|height=180\n"
	outputMetadata := probeJSON("av1", "yuv420p10le", 320, 180, false)
	var outputDocument map[string]interface{}
	if err := json.Unmarshal(outputMetadata, &outputDocument); err != nil {
		t.Fatal(err)
	}
	outputStream := outputDocument["streams"].([]interface{})[0].(map[string]interface{})
	outputStream["bits_per_raw_sample"] = "10"
	outputMetadata, _ = json.Marshal(outputDocument)
	runner := &fakeRunner{
		runs:    [][]byte{probeJSON("h264", "yuv420p", 320, 180, false), outputMetadata, nil},
		streams: []string{sourceFrames, outputFrames},
	}
	e := engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
	r := request{source: "source", output: "output", mime: "video/mp4", kind: "mp4-av1", limits: maxLimits()}
	var response bytes.Buffer
	if err := e.verify(r, &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runner.lastFFmpeg, " "), "-f null -") {
		t.Fatalf("decode args = %q", runner.lastFFmpeg)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			FullyDecodedVideo bool   `json:"fully_decoded_video"`
			PTSDeltaSHA256    string `json:"pts_delta_sha256"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Bytes(), &envelope); err != nil || !envelope.OK || !envelope.Result.FullyDecodedVideo || envelope.Result.PTSDeltaSHA256 != hashDeltas([]int64{100, 140}) {
		t.Fatalf("verification response = %s, %v", response.Bytes(), err)
	}
}

func TestAudioPolicyPreservesSupportedRateAndResamplesOtherwise(t *testing.T) {
	source := inspection{AudioPresent: true, AudioSampleRate: 44100}
	if rate, graph := outputAudio(source); rate != 44100 || graph != "asetpts=PTS-STARTPTS" {
		t.Fatalf("preserved audio = %d, %q", rate, graph)
	}
	source.AudioSampleRate = 192000
	if rate, graph := outputAudio(source); rate != 48000 || graph != "aresample=48000,asetpts=PTS-STARTPTS" {
		t.Fatalf("resampled audio = %d, %q", rate, graph)
	}
}

type fakeRunner struct {
	runs        [][]byte
	streams     []string
	runCalls    int
	runErrorAt  int
	writeOutput []byte
	lastFFmpeg  []string
}

func (f *fakeRunner) run(path string, args []string, _ int64) ([]byte, []byte, error) {
	f.runCalls++
	if strings.HasSuffix(path, "ffmpeg") {
		f.lastFFmpeg = slices.Clone(args)
		if len(f.writeOutput) > 0 && len(args) > 0 {
			_ = os.WriteFile(args[len(args)-1], f.writeOutput, 0o600)
		}
	}
	if f.runErrorAt == f.runCalls {
		return nil, nil, errors.New("fake command failure")
	}
	if len(f.runs) == 0 {
		return nil, nil, nil
	}
	result := f.runs[0]
	f.runs = f.runs[1:]
	return result, nil, nil
}

func (f *fakeRunner) stream(_ string, _ []string, consume func(io.Reader) error) error {
	if len(f.streams) == 0 {
		return errors.New("unexpected stream")
	}
	value := f.streams[0]
	f.streams = f.streams[1:]
	return consume(strings.NewReader(value))
}

func probeJSON(codec, pixFmt string, width, height int, audio bool) []byte {
	streams := []map[string]interface{}{{
		"index": 0, "codec_name": codec, "codec_type": "video", "profile": "High", "width": width, "height": height,
		"pix_fmt": pixFmt, "bits_per_raw_sample": "8", "sample_aspect_ratio": "1:1", "time_base": "1/1000",
		"color_primaries": "bt709", "color_transfer": "bt709", "color_space": "bt709", "color_range": "tv",
		"disposition": map[string]int{"default": 1, "attached_pic": 0}, "tags": map[string]string{}, "side_data_list": []interface{}{},
	}}
	if audio {
		streams = append(streams, map[string]interface{}{
			"index": 1, "codec_name": "aac", "codec_type": "audio", "profile": "LC", "channels": 2, "channel_layout": "stereo", "sample_rate": "48000",
			"disposition": map[string]int{"default": 1, "attached_pic": 0}, "tags": map[string]string{}, "side_data_list": []interface{}{},
		})
	}
	encoded, _ := json.Marshal(map[string]interface{}{"streams": streams, "format": map[string]string{"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}})
	return encoded
}

func disposition(defaultValue, attached int) struct {
	Default     int `json:"default"`
	AttachedPic int `json:"attached_pic"`
} {
	return struct {
		Default     int `json:"default"`
		AttachedPic int `json:"attached_pic"`
	}{defaultValue, attached}
}

func maxLimits() limits {
	return limits{maxStreams, maxDimension, maxFrames, maxAudioChannels, maxAudioRate, maxPixels, maxDecodedPixels, maxDurationUS, 240, 1, maxVideoBytes, maxThumbBytes, maxGenerated}
}

func maxLimitValues() map[string]string {
	l := maxLimits()
	return map[string]string{
		"--max-streams": strconv.Itoa(l.Streams), "--max-dimension": strconv.Itoa(l.Dimension), "--max-pixels-per-frame": strconv.FormatUint(l.Pixels, 10),
		"--max-duration-us": strconv.FormatInt(l.DurationUS, 10), "--max-fps-numerator": strconv.FormatInt(l.FPSNum, 10), "--max-fps-denominator": strconv.FormatInt(l.FPSDen, 10),
		"--max-frames": strconv.Itoa(l.Frames), "--max-decoded-pixels": strconv.FormatUint(l.DecodedPixels, 10), "--max-audio-channels": strconv.Itoa(l.AudioChannels),
		"--max-audio-sample-rate": strconv.Itoa(l.AudioRate), "--max-video-output-bytes": strconv.FormatInt(l.VideoBytes, 10), "--max-thumbnail-output-bytes": strconv.FormatInt(l.ThumbBytes, 10),
		"--max-generated-output-bytes": strconv.FormatInt(l.GeneratedBytes, 10),
	}
}

func inspectArgs(l limits) []string {
	values := maxLimitValues()
	args := []string{"inspect", "--protocol", "1", "--input", "/proc/self/fd/3", "--input-mime", "video/mp4"}
	for _, name := range limitNames {
		args = append(args, name, values[name])
	}
	return args
}

func cloneStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func isCode(err error, code string) bool {
	var protocolErr helperError
	return errors.As(err, &protocolErr) && protocolErr.code == code
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/file"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
