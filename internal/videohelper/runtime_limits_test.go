//go:build linux

package videohelper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"testing"
)

func TestRuntimeFixedCeilingsPinned(t *testing.T) {
	if maxStreams != 32 || maxDimension != 16_384 || maxPixels != 268_435_456 ||
		maxDurationUS != 14_400_000_000 || maxFrames != 3_456_000 ||
		maxDecodedPixels != 8_000_000_000_000 || maxAudioChannels != 8 || maxAudioRate != 192_000 {
		t.Fatalf("fixed runtime ceilings changed: %#v", maxLimits())
	}
	limit := maxLimits()
	if limit.FPSNum != 240 || limit.FPSDen != 1 {
		t.Fatalf("fixed effective FPS ceiling = %d/%d, want 240/1", limit.FPSNum, limit.FPSDen)
	}
}

func TestInspectRuntimeMetadataCeilings(t *testing.T) {
	ordinary := runtimeFrames{width: 1, height: 1, count: 1, duration: 1}

	t.Run("total streams fixed exact", func(t *testing.T) {
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 1, "1/1", maxStreams, runtimeAudio{}), ordinary, maxLimits())
		if got.TotalStreams != maxStreams {
			t.Fatalf("total streams = %d, want %d", got.TotalStreams, maxStreams)
		}
	})
	t.Run("total streams fixed plus one", func(t *testing.T) {
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 1, "1/1", maxStreams+1, runtimeAudio{}), ordinary, maxLimits(), "resource_limit")
	})

	t.Run("dimension fixed exact", func(t *testing.T) {
		frames := runtimeFrames{width: maxDimension, height: 1, count: 1, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(maxDimension, 1, 1, "1/1", 1, runtimeAudio{}), frames, maxLimits())
		if got.CodedWidth != maxDimension {
			t.Fatalf("coded width = %d, want %d", got.CodedWidth, maxDimension)
		}
	})
	t.Run("dimension fixed plus one", func(t *testing.T) {
		frames := runtimeFrames{width: maxDimension + 1, height: 1, count: 1, duration: 1}
		requireRuntimeInspectError(t, runtimeProbe(maxDimension+1, 1, 1, "1/1", 1, runtimeAudio{}), frames, maxLimits(), "resource_limit")
	})

	t.Run("pixels per frame fixed exact", func(t *testing.T) {
		frames := runtimeFrames{width: maxDimension, height: maxDimension, count: 1, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(maxDimension, maxDimension, 1, "1/1", 1, runtimeAudio{}), frames, maxLimits())
		if uint64(got.CodedWidth)*uint64(got.CodedHeight) != maxPixels {
			t.Fatalf("pixels per frame = %d, want %d", uint64(got.CodedWidth)*uint64(got.CodedHeight), maxPixels)
		}
	})
	t.Run("pixels per frame reduced exact and plus one", func(t *testing.T) {
		limit := maxLimits()
		limit.Pixels = 6
		requireRuntimeInspect(t, runtimeProbe(6, 1, 1, "1/1", 1, runtimeAudio{}), runtimeFrames{width: 6, height: 1, count: 1, duration: 1}, limit)
		requireRuntimeInspectError(t, runtimeProbe(7, 1, 1, "1/1", 1, runtimeAudio{}), runtimeFrames{width: 7, height: 1, count: 1, duration: 1}, limit, "resource_limit")
	})

	t.Run("audio channels fixed exact", func(t *testing.T) {
		audio := runtimeAudio{channels: maxAudioChannels, layout: "7.1", rate: 48_000}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 1, "1/1", 2, audio), ordinary, maxLimits())
		if got.AudioChannels != maxAudioChannels {
			t.Fatalf("audio channels = %d, want %d", got.AudioChannels, maxAudioChannels)
		}
	})
	t.Run("audio channels fixed plus one", func(t *testing.T) {
		audio := runtimeAudio{channels: maxAudioChannels + 1, layout: "unknown", rate: 48_000}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 1, "1/1", 2, audio), ordinary, maxLimits(), "unsupported_input")
	})
	t.Run("audio channels reduced exact and plus one", func(t *testing.T) {
		limit := maxLimits()
		limit.AudioChannels = 2
		requireRuntimeInspect(t, runtimeProbe(1, 1, 1, "1/1", 2, runtimeAudio{channels: 2, layout: "stereo", rate: 48_000}), ordinary, limit)
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 1, "1/1", 2, runtimeAudio{channels: 3, layout: "2.1", rate: 48_000}), ordinary, limit, "unsupported_input")
	})

	t.Run("audio sample rate fixed exact", func(t *testing.T) {
		audio := runtimeAudio{channels: 2, layout: "stereo", rate: maxAudioRate}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 1, "1/1", 2, audio), ordinary, maxLimits())
		if got.AudioSampleRate != maxAudioRate {
			t.Fatalf("audio sample rate = %d, want %d", got.AudioSampleRate, maxAudioRate)
		}
	})
	t.Run("audio sample rate fixed plus one", func(t *testing.T) {
		audio := runtimeAudio{channels: 2, layout: "stereo", rate: maxAudioRate + 1}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 1, "1/1", 2, audio), ordinary, maxLimits(), "unsupported_input")
	})
}

func TestFrameFactsRuntimeCeilings(t *testing.T) {
	t.Run("duration fixed exact", func(t *testing.T) {
		frames := runtimeFrames{width: 1, height: 1, count: 1, duration: maxDurationUS}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, maxDurationUS, "1/1000000", 1, runtimeAudio{}), frames, maxLimits())
		if got.DurationUS != maxDurationUS {
			t.Fatalf("duration = %d, want %d", got.DurationUS, maxDurationUS)
		}
	})
	t.Run("duration fixed plus one", func(t *testing.T) {
		frames := runtimeFrames{width: 1, height: 1, count: 1, duration: maxDurationUS + 1}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, maxDurationUS+1, "1/1000000", 1, runtimeAudio{}), frames, maxLimits(), "resource_limit")
	})

	t.Run("effective FPS fixed exact", func(t *testing.T) {
		frames := runtimeFrames{width: 1, height: 1, count: 1, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 1, "1/240", 1, runtimeAudio{}), frames, maxLimits())
		if got.EffectiveFPS != (rational{240, 1}) {
			t.Fatalf("effective FPS = %#v, want 240/1", got.EffectiveFPS)
		}
	})
	t.Run("effective FPS fixed plus one", func(t *testing.T) {
		frames := runtimeFrames{width: 1, height: 1, count: 1, duration: 1}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 1, "1/241", 1, runtimeAudio{}), frames, maxLimits(), "resource_limit")
	})

	t.Run("frame count reduced exact and plus one", func(t *testing.T) {
		limit := maxLimits()
		limit.Frames = 3
		exact := runtimeFrames{width: 1, height: 1, count: 3, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 3, "1/1", 1, runtimeAudio{}), exact, limit)
		if got.FrameCount != limit.Frames {
			t.Fatalf("frame count = %d, want %d", got.FrameCount, limit.Frames)
		}
		excess := runtimeFrames{width: 1, height: 1, count: 4, duration: 1}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 4, "1/1", 1, runtimeAudio{}), excess, limit, "resource_limit")
	})

	t.Run("decoded pixels fixed exact", func(t *testing.T) {
		const width, height, count = 16_000, 15_625, 32_000
		frames := runtimeFrames{width: width, height: height, count: count, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(width, height, count, "1/100", 1, runtimeAudio{}), frames, maxLimits())
		if got.CumulativeDecodedPixels != maxDecodedPixels {
			t.Fatalf("decoded pixels = %d, want %d", got.CumulativeDecodedPixels, maxDecodedPixels)
		}
	})
	t.Run("decoded pixels reduced exact and plus one", func(t *testing.T) {
		limit := maxLimits()
		limit.DecodedPixels = 3
		exact := runtimeFrames{width: 1, height: 1, count: 3, duration: 1}
		got := requireRuntimeInspect(t, runtimeProbe(1, 1, 3, "1/1", 1, runtimeAudio{}), exact, limit)
		if got.CumulativeDecodedPixels != limit.DecodedPixels {
			t.Fatalf("decoded pixels = %d, want %d", got.CumulativeDecodedPixels, limit.DecodedPixels)
		}
		excess := runtimeFrames{width: 1, height: 1, count: 4, duration: 1}
		requireRuntimeInspectError(t, runtimeProbe(1, 1, 4, "1/1", 1, runtimeAudio{}), excess, limit, "resource_limit")
	})
}

func TestFrameFactsRuntimeArithmeticOverflow(t *testing.T) {
	limit := maxLimits()
	tests := []struct {
		name   string
		frames string
	}{
		{
			name: "PTS subtraction",
			frames: `{"frames":[{"media_type":"video","stream_index":0,"pts":` + strconv.FormatInt(math.MinInt64, 10) + `,"duration":1,"width":1,"height":1},` +
				`{"media_type":"video","stream_index":0,"pts":` + strconv.FormatInt(math.MaxInt64, 10) + `,"duration":1,"width":1,"height":1}]}`,
		},
		{
			name: "duration addition",
			frames: `{"frames":[{"media_type":"video","stream_index":0,"pts":0,"duration":` + strconv.FormatInt(math.MaxInt64, 10) + `,"width":1,"height":1},` +
				`{"media_type":"video","stream_index":0,"pts":` + strconv.FormatInt(math.MaxInt64, 10) + `,"duration":1,"width":1,"height":1}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{streams: []string{test.frames}}
			e := engine{ffprobe: "/p/ffprobe", run: runner}
			if _, err := e.frameFacts("input", 0, 1, 1, rational{1, 1}, 0, limit); !isCode(err, "resource_limit") {
				t.Fatalf("overflow error = %v, want resource_limit", err)
			}
		})
	}

	t.Run("duration scaling", func(t *testing.T) {
		frames := runtimeFrames{width: 1, height: 1, count: 1, duration: math.MaxInt64}
		runner := &runtimeLimitRunner{metadata: runtimeProbe(1, 1, math.MaxInt64, strconv.FormatInt(math.MaxInt64, 10)+"/1", 1, runtimeAudio{}), frames: frames}
		e := engine{ffprobe: "/p/ffprobe", run: runner}
		if _, _, err := e.inspect("input", "video/mp4", limit); !isCode(err, "resource_limit") {
			t.Fatalf("duration scaling overflow error = %v, want resource_limit", err)
		}
	})
}

type runtimeAudio struct {
	channels int
	layout   string
	rate     int
}

type runtimeFrames struct {
	width, height int
	count         int
	duration      int64
}

type runtimeLimitRunner struct {
	metadata []byte
	frames   runtimeFrames
}

func (r *runtimeLimitRunner) run(string, []string, int64) ([]byte, []byte, error) {
	if r.metadata != nil {
		metadata := r.metadata
		r.metadata = nil
		return metadata, nil, nil
	}
	return nil, nil, nil
}

func (r *runtimeLimitRunner) stream(_ string, _ []string, consume func(io.Reader) error) error {
	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() {
		buffer := bufio.NewWriter(writer)
		_, err := io.WriteString(buffer, `{"frames":[`)
		for index := 0; err == nil && index < r.frames.count; index++ {
			if index != 0 {
				_, err = buffer.WriteString(",")
			}
			if err == nil {
				_, err = fmt.Fprintf(buffer, `{"media_type":"video","stream_index":0,"pts":%d,"duration":%d,"width":%d,"height":%d}`,
					int64(index)*r.frames.duration, r.frames.duration, r.frames.width, r.frames.height)
			}
		}
		if err == nil {
			_, err = io.WriteString(buffer, `]}`)
		}
		if err == nil {
			err = buffer.Flush()
		}
		_ = writer.CloseWithError(err)
		written <- err
	}()
	consumeErr := consume(reader)
	_ = reader.Close()
	writeErr := <-written
	if consumeErr != nil {
		return consumeErr
	}
	return writeErr
}

func requireRuntimeInspect(t *testing.T, metadata []byte, frames runtimeFrames, limit limits) inspection {
	t.Helper()
	runner := &runtimeLimitRunner{metadata: metadata, frames: frames}
	e := engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
	got, _, err := e.inspect("input", "video/mp4", limit)
	if err != nil {
		t.Fatalf("inspect rejected boundary corpus: %v", err)
	}
	return got
}

func requireRuntimeInspectError(t *testing.T, metadata []byte, frames runtimeFrames, limit limits, code string) {
	t.Helper()
	runner := &runtimeLimitRunner{metadata: metadata, frames: frames}
	e := engine{ffprobe: "/p/ffprobe", ffmpeg: "/p/ffmpeg", run: runner}
	if _, _, err := e.inspect("input", "video/mp4", limit); !isCode(err, code) {
		t.Fatalf("inspect error = %v, want %s", err, code)
	}
}

func runtimeProbe(width, height int, durationTicks int64, timeBase string, totalStreams int, audio runtimeAudio) []byte {
	streams := []map[string]interface{}{{
		"index": 0, "codec_name": "h264", "codec_type": "video", "profile": "High",
		"width": width, "height": height, "pix_fmt": "yuv420p", "bits_per_raw_sample": "8",
		"sample_aspect_ratio": "1:1", "time_base": timeBase, "duration_ts": durationTicks,
		"color_primaries": "bt709", "color_transfer": "bt709", "color_space": "bt709", "color_range": "tv",
		"disposition": map[string]int{"default": 1, "attached_pic": 0}, "tags": map[string]string{}, "side_data_list": []interface{}{},
	}}
	if audio.channels != 0 {
		streams = append(streams, map[string]interface{}{
			"index": 1, "codec_name": "aac", "codec_type": "audio", "channels": audio.channels,
			"channel_layout": audio.layout, "sample_rate": strconv.Itoa(audio.rate),
			"disposition": map[string]int{"default": 1, "attached_pic": 0},
		})
	}
	for len(streams) < totalStreams {
		streams = append(streams, map[string]interface{}{"index": len(streams), "codec_name": "bin_data", "codec_type": "data"})
	}
	encoded, _ := json.Marshal(map[string]interface{}{
		"streams": streams,
		"format":  map[string]string{"format_name": "mov,mp4,m4a,3gp,3g2,mj2"},
	})
	return encoded
}
