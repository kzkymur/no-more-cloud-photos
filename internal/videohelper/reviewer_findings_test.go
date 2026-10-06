//go:build linux

package videohelper

import (
	"encoding/json"
	"io"
	"math"
	"math/big"
	"os"
	"slices"
	"strconv"
	"testing"
)

func TestSourceDecodeAndTransformUseXError(t *testing.T) {
	frames := frameJSON(0, 320, 180, frameTiming{0, 40}, frameTiming{40, 40})
	runner := &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}}
	e := engine{ffprobe: "/p/ffprobe", run: runner}
	if _, _, err := e.inspect("input", "video/mp4", maxLimits()); err != nil {
		t.Fatal(err)
	}
	if !containsSequence(runner.lastStream, "-v", "error", "-xerror") {
		t.Fatalf("frame decode args = %q", runner.lastStream)
	}

	input := writeTemp(t, "input")
	output := writeTemp(t, "")
	runner = &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}, writeOutput: []byte("mp4")}
	e = engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
	r := request{input: input, output: output, mime: "video/mp4", kind: "mp4-av1", icc: writeTemp(t, "icc"), maxLongEdge: 1920, bitDepth: 10, limits: maxLimits()}
	if err := e.transform(r, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !containsSequence(runner.lastFFmpeg, "-v", "error", "-xerror") {
		t.Fatalf("transform args = %q", runner.lastFFmpeg)
	}
	runner = &fakeRunner{runs: [][]byte{probeJSON("h264", "yuv420p", 320, 180, false)}, streams: []string{frames}, runErrorAt: 2, writeOutput: []byte("partial")}
	e.run = runner
	if err := e.transform(r, io.Discard); !isCode(err, "encode_failed") {
		t.Fatalf("fail-fast transform error = %v", err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() != 0 {
		t.Fatalf("failed transform output size = %d, %v", info.Size(), err)
	}
}

func TestTransformNeutralizesExactSelectedStreamOrientation(t *testing.T) {
	for _, test := range []struct {
		name      string
		rotation  interface{}
		rotateTag string
		wantFlag  bool
	}{
		{name: "rotated", rotation: float64(-90), wantFlag: true},
		{name: "identity matrix", rotation: float64(0), wantFlag: true},
		{name: "rotate tag", rotateTag: "180", wantFlag: true},
		{name: "absent", wantFlag: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := multistreamProbeJSON(test.rotation, test.rotateTag)
			frames := frameJSON(3, 4, 6, frameTiming{0, 40}, frameTiming{40, 40})
			runner := &fakeRunner{runs: [][]byte{metadata}, streams: []string{frames}, writeOutput: []byte("mp4")}
			e := engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
			r := request{input: writeTemp(t, "input"), output: writeTemp(t, ""), mime: "video/mp4", kind: "mp4-av1", icc: writeTemp(t, "icc"), maxLongEdge: 1920, bitDepth: 10, limits: maxLimits()}
			if err := e.transform(r, io.Discard); err != nil {
				t.Fatal(err)
			}
			flag := slices.Index(runner.lastFFmpeg, "-display_rotation:3")
			if (flag >= 0) != test.wantFlag {
				t.Fatalf("orientation args = %q", runner.lastFFmpeg)
			}
			if flag >= 0 && (flag+1 >= len(runner.lastFFmpeg) || runner.lastFFmpeg[flag+1] != "0") {
				t.Fatalf("orientation args = %q", runner.lastFFmpeg)
			}
			if slices.Contains(runner.lastFFmpeg, "-display_rotation:v:0") {
				t.Fatalf("ordinal orientation target retained: %q", runner.lastFFmpeg)
			}
		})
	}
}

func TestPQFrameMetadataIsSelectedAndConsistent(t *testing.T) {
	metadata := pqProbeJSON(nil)
	validFrames := frameJSONWithSideData(2, []map[string]interface{}{
		{"max_luminance": "1000/1"}, {"max_content": float64(600)},
	})
	runner := &fakeRunner{runs: [][]byte{metadata}, streams: []string{validFrames}}
	e := engine{ffprobe: "/p/ffprobe", run: runner}
	got, _, err := e.inspect("input", "video/mp4", maxLimits())
	if err != nil || !got.HDR || got.MasteringMaxNits != 1000 || got.MaxCLLNits != 600 {
		t.Fatalf("inspection = %#v, %v", got, err)
	}
	if !containsSequence(runner.lastStream, "-select_streams", "2") {
		t.Fatalf("frame probe not selected-stream-bound: %q", runner.lastStream)
	}

	runner.runs, runner.streams = [][]byte{metadata}, []string{frameJSONWithSideData(1, []map[string]interface{}{{"max_luminance": "1000/1"}, {"max_content": float64(600)}})}
	if _, _, err := e.inspect("input", "video/mp4", maxLimits()); !isCode(err, "unsupported_input") {
		t.Fatalf("secondary frame accepted: %v", err)
	}

	metadata = pqProbeJSON([]map[string]interface{}{{"max_luminance": "1000/1"}, {"max_content": float64(600)}})
	contradiction := frameJSONWithSideData(2, []map[string]interface{}{{"max_luminance": "1000/1"}, {"max_content": float64(500)}})
	runner.runs, runner.streams = [][]byte{metadata}, []string{contradiction}
	if _, _, err := e.inspect("input", "video/mp4", maxLimits()); !isCode(err, "unsupported_input") {
		t.Fatalf("contradictory frame HDR accepted: %v", err)
	}

	metadata = pqProbeJSON(nil)
	contradiction = frameJSONWithConflictingSideData(2)
	runner.runs, runner.streams = [][]byte{metadata}, []string{contradiction}
	if _, _, err := e.inspect("input", "video/mp4", maxLimits()); !isCode(err, "unsupported_input") {
		t.Fatalf("later contradictory frame HDR accepted: %v", err)
	}
}

func TestFrameFactsRequirePositiveReconciledDurations(t *testing.T) {
	tests := []struct {
		name           string
		frames         string
		streamDuration int64
		wantCode       string
	}{
		{"exact", rawFrameJSON(0, 2, 2, `40`, `40`, 40), 80, ""},
		{"missing", rawFrameJSON(0, 2, 2, `null`, `40`, 40), 80, "unsupported_input"},
		{"not parseable", rawFrameJSON(0, 2, 2, `"N/A"`, `40`, 40), 80, "unsupported_input"},
		{"zero", rawFrameJSON(0, 2, 2, `0`, `40`, 40), 80, "unsupported_input"},
		{"negative", rawFrameJSON(0, 2, 2, `-1`, `40`, 40), 80, "unsupported_input"},
		{"delta contradiction", rawFrameJSON(0, 2, 2, `39`, `40`, 40), 80, "unsupported_input"},
		{"stream contradiction", rawFrameJSON(0, 2, 2, `40`, `40`, 40), 81, "unsupported_input"},
		{"duration overflow", overflowFrameJSON(), 0, "resource_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{streams: []string{test.frames}}
			e := engine{ffprobe: "/p/ffprobe", run: runner}
			_, err := e.frameFacts("input", 0, 2, 2, rational{1, 1000}, test.streamDuration, maxLimits())
			if test.wantCode == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantCode != "" && !isCode(err, test.wantCode) {
				t.Fatalf("error = %v, want %s", err, test.wantCode)
			}
		})
	}
	runner := &fakeRunner{streams: []string{rawFrameJSON(0, 2, 2, `40`, `41`, 40)}}
	e := engine{ffprobe: "/p/ffprobe", run: runner}
	limit := maxLimits()
	limit.DurationUS = 81_000
	if _, err := e.frameFacts("input", 0, 2, 2, rational{1, 1000}, 81, limit); err != nil {
		t.Fatalf("exact duration boundary rejected: %v", err)
	}
	runner.streams = []string{rawFrameJSON(0, 2, 2, `40`, `41`, 40)}
	limit.DurationUS = 80_999
	if _, err := e.frameFacts("input", 0, 2, 2, rational{1, 1000}, 81, limit); !isCode(err, "resource_limit") {
		t.Fatalf("duration boundary +1 error = %v", err)
	}
}

func TestRationalGeometryFloorsWithoutUpscaleAtEvenBoundaries(t *testing.T) {
	width, height, err := fitRationalDimensions(big.NewRat(3, 2), big.NewRat(100, 1), 1920, false)
	if err != nil || width != 1 || height != 100 {
		t.Fatalf("thumbnail geometry = %dx%d, %v", width, height, err)
	}
	if _, _, err := fitRationalDimensions(big.NewRat(3, 2), big.NewRat(100, 1), 1920, true); !isCode(err, "unsupported_input") {
		t.Fatalf("unrepresentable even geometry accepted: %v", err)
	}
	width, height, err = fitRationalDimensions(big.NewRat(5, 2), big.NewRat(101, 1), 1920, true)
	if err != nil || width != 2 || height != 100 {
		t.Fatalf("even floor geometry = %dx%d, %v", width, height, err)
	}
	width, height, err = fitRationalDimensions(big.NewRat(4001, 2), big.NewRat(1001, 1), 1920, true)
	if err != nil || width != 1920 || height != 960 {
		t.Fatalf("scaled rational geometry = %dx%d, %v", width, height, err)
	}
}

func containsSequence(values []string, sequence ...string) bool {
	for index := 0; index+len(sequence) <= len(values); index++ {
		if slices.Equal(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func multistreamProbeJSON(rotation interface{}, rotateTag string) []byte {
	var document map[string]interface{}
	_ = json.Unmarshal(probeJSON("h264", "yuv420p", 2, 2, false), &document)
	base := document["streams"].([]interface{})[0].(map[string]interface{})
	base["disposition"] = map[string]int{"default": 0, "attached_pic": 1}
	ordinary := cloneJSONMap(base)
	ordinary["index"], ordinary["width"], ordinary["height"], ordinary["disposition"] = 1, 4, 6, map[string]int{"default": 0, "attached_pic": 0}
	selected := cloneJSONMap(ordinary)
	selected["index"], selected["disposition"], selected["duration_ts"] = 3, map[string]int{"default": 1, "attached_pic": 0}, 80
	if rotation != nil {
		selected["side_data_list"] = []map[string]interface{}{{"rotation": rotation}}
	}
	if rotateTag != "" {
		selected["tags"] = map[string]string{"rotate": rotateTag}
	}
	document["streams"] = []interface{}{base, ordinary, selected}
	encoded, _ := json.Marshal(document)
	return encoded
}

func pqProbeJSON(streamSideData []map[string]interface{}) []byte {
	var document map[string]interface{}
	_ = json.Unmarshal(probeJSON("hevc", "yuv420p10le", 320, 180, false), &document)
	primary := document["streams"].([]interface{})[0].(map[string]interface{})
	primary["index"], primary["color_primaries"], primary["color_transfer"], primary["color_space"] = 2, "bt2020", "smpte2084", "bt2020nc"
	primary["side_data_list"] = streamSideData
	document["streams"] = []interface{}{
		map[string]interface{}{"index": 0, "codec_type": "video", "disposition": map[string]int{"default": 0, "attached_pic": 1}},
		primary,
	}
	encoded, _ := json.Marshal(document)
	return encoded
}

func cloneJSONMap(source map[string]interface{}) map[string]interface{} {
	encoded, _ := json.Marshal(source)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func frameJSONWithSideData(streamIndex int, sideData []map[string]interface{}) string {
	frame := map[string]interface{}{"media_type": "video", "stream_index": streamIndex, "pts": 0, "duration": 80, "width": 320, "height": 180, "side_data_list": sideData}
	encoded, _ := json.Marshal(map[string]interface{}{"frames": []interface{}{frame}})
	return string(encoded)
}

func frameJSONWithConflictingSideData(streamIndex int) string {
	frames := []interface{}{
		map[string]interface{}{"media_type": "video", "stream_index": streamIndex, "pts": 0, "duration": 40, "width": 320, "height": 180, "side_data_list": []map[string]interface{}{{"max_luminance": "1000/1"}, {"max_content": float64(600)}}},
		map[string]interface{}{"media_type": "video", "stream_index": streamIndex, "pts": 40, "duration": 40, "width": 320, "height": 180, "side_data_list": []map[string]interface{}{{"max_luminance": "1000/1"}, {"max_content": float64(500)}}},
	}
	encoded, _ := json.Marshal(map[string]interface{}{"frames": frames})
	return string(encoded)
}

func rawFrameJSON(streamIndex, width, height int, firstDuration, secondDuration string, secondPTS int64) string {
	return `{"frames":[{"media_type":"video","stream_index":` + strconv.Itoa(streamIndex) + `,"pts":0,"duration":` + firstDuration + `,"width":` + strconv.Itoa(width) + `,"height":` + strconv.Itoa(height) + `},{"media_type":"video","stream_index":` + strconv.Itoa(streamIndex) + `,"pts":` + strconv.FormatInt(secondPTS, 10) + `,"duration":` + secondDuration + `,"width":` + strconv.Itoa(width) + `,"height":` + strconv.Itoa(height) + `}]}`
}

func overflowFrameJSON() string {
	return `{"frames":[{"media_type":"video","stream_index":0,"pts":` + strconv.FormatInt(math.MinInt64, 10) + `,"duration":` + strconv.FormatInt(math.MaxInt64, 10) + `,"width":2,"height":2},{"media_type":"video","stream_index":0,"pts":-1,"duration":1,"width":2,"height":2}]}`
}
