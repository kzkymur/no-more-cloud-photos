package readapi

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestChangesCursor(t *testing.T) {
	key := []byte(strings.Repeat("x", 32))
	codec, _ := NewCursorCodec(key)
	restarted, _ := NewCursorCodec(key)
	for _, position := range []int64{0, 1, 42, math.MaxInt64} {
		token, err := codec.EncodeChanges(position)
		if err != nil {
			t.Fatal(err)
		}
		got, err := restarted.DecodeChanges(token)
		if err != nil || got != position {
			t.Fatalf("position %d: %d %v", position, got, err)
		}
		if _, err := codec.DecodeJobs(token, JobsCursorFilter{}); err == nil {
			t.Fatal("changes accepted as jobs")
		}
		if _, err := codec.DecodeMedia(token, MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}); err == nil {
			t.Fatal("changes accepted as media")
		}
		other, _ := NewCursorCodec([]byte(strings.Repeat("y", 32)))
		if _, err := other.DecodeChanges(token); err == nil {
			t.Fatal("wrong key accepted")
		}
		if _, err := codec.DecodeChanges(token + "="); err == nil {
			t.Fatal("padding accepted")
		}
	}
	if _, err := codec.EncodeChanges(-1); err == nil {
		t.Fatal("negative accepted")
	}
	for _, token := range []string{"", "42", strings.Repeat("A", cursorEncodedSize)} {
		if _, err := codec.DecodeChanges(token); err == nil {
			t.Fatal("invalid accepted")
		}
	}
	wrongRoute, _ := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: "00000000-0000-4000-8000-000000000001"})
	if _, err := codec.DecodeChanges(wrongRoute); err == nil {
		t.Fatal("jobs accepted as changes")
	}
	// Even correctly MACed envelopes must reject unsupported field layouts.
	fingerprint := changesFingerprint()
	for _, token := range []string{
		codec.encode(CursorKindChangesV1, cursorClassNull, fingerprint, 1, [cursorUUIDSize]byte{}),
		codec.encode(CursorKindChangesV1, cursorClassValue, fingerprint, -1, [cursorUUIDSize]byte{}),
		codec.encode(CursorKindChangesV1, cursorClassValue, fingerprint, 1, [cursorUUIDSize]byte{1}),
		codec.encode(CursorKindChangesV1, cursorClassValue, [32]byte{}, 1, [cursorUUIDSize]byte{}),
	} {
		if _, err := codec.DecodeChanges(token); err == nil {
			t.Fatal("noncanonical signed envelope accepted")
		}
	}

}
