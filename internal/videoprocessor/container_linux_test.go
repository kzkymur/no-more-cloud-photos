//go:build linux

package videoprocessor

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMP4TrackAndSampleEntryValidation(t *testing.T) {
	validVideo := testVisualEntry("av01", testBox("av1C", []byte{0x81, 0, 0, 0}))
	validAudio := testAudioEntry(testAACConfig(2))
	tests := map[string][]byte{
		"valid video":                  testMP4(testTrack("vide", validVideo)),
		"empty av01":                   testMP4(testTrack("vide", testBox("av01"))),
		"missing av1C":                 testMP4(testTrack("vide", testVisualEntry("av01"))),
		"short av1C":                   testMP4(testTrack("vide", testVisualEntry("av01", testBox("av1C", []byte{0x81})))),
		"invalid av1C marker":          testMP4(testTrack("vide", testVisualEntry("av01", testBox("av1C", []byte{1, 0, 0, 0})))),
		"video under audio handler":    testMP4(testTrack("soun", validVideo)),
		"missing handler":              testMP4(testTrack("", validVideo)),
		"two video entries":            testMP4(testTrack("vide", validVideo, validVideo)),
		"mixed entries in video track": testMP4(testTrack("vide", validVideo, validAudio)),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			file := regularFile(t, data)
			defer file.Close()
			got := validateVideoContainer(file, int64(len(data)), "mp4-av1", false)
			if got != (name == "valid video") {
				t.Fatalf("valid = %t", got)
			}
		})
	}
}

func TestMP4AACTrackValidation(t *testing.T) {
	video := testTrack("vide", testVisualEntry("av01", testBox("av1C", []byte{0x81, 0, 0, 0})))
	validAudio := testAudioEntry(testAACConfig(2))
	tests := map[string][]byte{
		"valid separate audio":      testMP4(video, testTrack("soun", validAudio)),
		"audio absent":              testMP4(video),
		"empty mp4a":                testMP4(video, testTrack("soun", testBox("mp4a"))),
		"missing esds":              testMP4(video, testTrack("soun", testAudioEntry(nil))),
		"malformed esds length":     testMP4(video, testTrack("soun", testAudioEntry(append(make([]byte, 4), 3, 0x80, 0x80, 0x80, 0x80)))),
		"non AAC-LC config":         testMP4(video, testTrack("soun", testAudioEntry(testAACConfig(1)))),
		"audio under video handler": testMP4(video, testTrack("vide", validAudio)),
		"duplicate audio tracks":    testMP4(video, testTrack("soun", validAudio), testTrack("soun", validAudio)),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			file := regularFile(t, data)
			defer file.Close()
			got := validateVideoContainer(file, int64(len(data)), "mp4-av1", true)
			if got != (name == "valid separate audio") {
				t.Fatalf("valid = %t", got)
			}
		})
	}
}

func minimalMP4(videoEntry string, audio bool) []byte {
	video := testTrack("vide", testVisualEntry(videoEntry, testBox("av1C", []byte{0x81, 0, 0, 0})))
	if audio {
		return testMP4(video, testTrack("soun", testAudioEntry(testAACConfig(2))))
	}
	return testMP4(video)
}

func testMP4(tracks ...[]byte) []byte {
	brand := []byte("isom\x00\x00\x02\x00isomiso6av01mp41")
	return bytes.Join([][]byte{testBox("ftyp", brand), testBox("moov", tracks...), testBox("mdat", []byte{1})}, nil)
}

func testTrack(handler string, entries ...[]byte) []byte {
	count := make([]byte, 8)
	binary.BigEndian.PutUint32(count[4:], uint32(len(entries)))
	stsd := testBox("stsd", append([][]byte{count}, entries...)...)
	children := [][]byte{}
	if handler != "" {
		hdlr := make([]byte, 24)
		copy(hdlr[8:12], handler)
		children = append(children, testBox("hdlr", hdlr))
	}
	children = append(children, testBox("minf", testBox("stbl", stsd)))
	return testBox("trak", testBox("mdia", children...))
}

func testVisualEntry(kind string, children ...[]byte) []byte {
	fields := make([]byte, 78)
	binary.BigEndian.PutUint16(fields[6:8], 1)
	binary.BigEndian.PutUint16(fields[24:26], 320)
	binary.BigEndian.PutUint16(fields[26:28], 180)
	binary.BigEndian.PutUint32(fields[28:32], 72<<16)
	binary.BigEndian.PutUint32(fields[32:36], 72<<16)
	binary.BigEndian.PutUint16(fields[40:42], 1)
	binary.BigEndian.PutUint16(fields[74:76], 24)
	binary.BigEndian.PutUint16(fields[76:78], 0xffff)
	return testBox(kind, append([][]byte{fields}, children...)...)
}

func testAudioEntry(esds []byte) []byte {
	fields := make([]byte, 28)
	binary.BigEndian.PutUint16(fields[6:8], 1)
	binary.BigEndian.PutUint16(fields[16:18], 2)
	binary.BigEndian.PutUint16(fields[18:20], 16)
	binary.BigEndian.PutUint32(fields[24:28], 48000<<16)
	if esds == nil {
		return testBox("mp4a", fields)
	}
	return testBox("mp4a", fields, testBox("esds", esds))
}

func testAACConfig(audioObjectType byte) []byte {
	asc := testDescriptor(0x05, []byte{audioObjectType<<3 | 1, 0x90})
	decoder := make([]byte, 13)
	decoder[0], decoder[1] = 0x40, 0x15
	decoder = append(decoder, asc...)
	es := append([]byte{0, 1, 0}, testDescriptor(0x04, decoder)...)
	return append(make([]byte, 4), testDescriptor(0x03, es)...)
}

func testDescriptor(tag byte, payload []byte) []byte {
	if len(payload) > 0x7f {
		panic("test descriptor too large")
	}
	return append([]byte{tag, byte(len(payload))}, payload...)
}

func testBox(kind string, payload ...[]byte) []byte {
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
