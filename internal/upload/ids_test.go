package upload

import (
	"bytes"
	"errors"
	"testing"
)

func TestNewUUIDv4UsesRandomBytesAndSetsRequiredBits(t *testing.T) {
	id, err := newUUIDv4(bytes.NewReader([]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0xff, 0x77,
		0xff, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}))
	if err != nil {
		t.Fatalf("newUUIDv4() error = %v", err)
	}
	if want := "00112233-4455-4f77-bf99-aabbccddeeff"; id != want {
		t.Fatalf("newUUIDv4() = %q, want %q", id, want)
	}
	if !IsUUIDv4(id) {
		t.Fatalf("IsUUIDv4(%q) = false", id)
	}
}

func TestNewUUIDv4PropagatesEntropyFailure(t *testing.T) {
	want := errors.New("entropy unavailable")
	if id, err := newUUIDv4(errorReader{want}); !errors.Is(err, want) || id != "" {
		t.Fatalf("newUUIDv4() = %q, %v; want empty ID, %v", id, err, want)
	}
}

func TestNewUUIDv4(t *testing.T) {
	first, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4() first error = %v", err)
	}
	second, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4() second error = %v", err)
	}
	if !IsUUIDv4(first) || !IsUUIDv4(second) || first == second {
		t.Fatalf("NewUUIDv4() returned %q and %q", first, second)
	}
}

func TestIsUUIDv4RejectsNoncanonicalOrWrongVariant(t *testing.T) {
	invalid := []string{
		"",
		"00112233-4455-4f77-bf99-aabbccddeef",
		"0011223344554f77bf99aabbccddeeff",
		"00112233-4455-3f77-bf99-aabbccddeeff",
		"00112233-4455-4f77-7f99-aabbccddeeff",
		"00112233-4455-4F77-BF99-AABBCCDDEEFF",
		"00112233-4455-4g77-bf99-aabbccddeeff",
	}
	for _, value := range invalid {
		if IsUUIDv4(value) {
			t.Errorf("IsUUIDv4(%q) = true", value)
		}
	}
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }
