package readapi

import (
	"testing"
)

func FuzzCursorDecodeNeverPanics(f *testing.F) {
	codec, err := NewCursorCodec(goldenKey)
	if err != nil {
		f.Fatal(err)
	}
	f.Add("")
	f.Add("not-a-cursor")
	f.Add(string(make([]byte, 1<<16)))
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = codec.DecodeMedia(token, MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude})
		_, _ = codec.DecodeJobs(token, JobsCursorFilter{})
	})
}

func FuzzCursorRoundTrip(f *testing.F) {
	f.Add(int64(0), uint64(0), false)
	f.Add(int64(1_700_000_000_123_456), uint64(1), true)
	f.Add(int64(-62_135_596_800_000_000), uint64(2), false)
	f.Fuzz(func(t *testing.T, micros int64, suffix uint64, nullTakenAt bool) {
		value, ok := timeFromMicros(micros)
		if !ok {
			t.Skip()
		}
		codec, err := NewCursorCodec(goldenKey)
		if err != nil {
			t.Fatal(err)
		}
		idBytes := [cursorUUIDSize]byte{
			0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0x4c, 0xde,
			0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}
		for index := 0; index < 8; index++ {
			idBytes[15-index] = byte(suffix >> (index * 8))
		}
		idBytes[8] = idBytes[8]&0x3f | 0x80
		id := formatCursorUUID(idBytes)

		jobsToken, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: value, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := codec.DecodeJobs(jobsToken, JobsCursorFilter{})
		if err != nil || jobs.ID != id || !jobs.CreatedAt.Equal(value) {
			t.Fatalf("jobs round trip = %#v, %v", jobs, err)
		}

		mediaPosition := MediaCursorPosition{ID: id}
		if !nullTakenAt {
			mediaPosition.TakenAt = &value
		}
		mediaFilter := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
		mediaToken, err := codec.EncodeMedia(mediaFilter, mediaPosition)
		if err != nil {
			t.Fatal(err)
		}
		media, err := codec.DecodeMedia(mediaToken, mediaFilter)
		if err != nil || media.ID != id || (media.TakenAt == nil) != nullTakenAt {
			t.Fatalf("media round trip = %#v, %v", media, err)
		}
		if media.TakenAt != nil && !media.TakenAt.Equal(value) {
			t.Fatalf("media time = %v, want %v", media.TakenAt, value)
		}
	})
}
