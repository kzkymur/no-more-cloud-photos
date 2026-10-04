package readapi

import (
	"errors"
	"testing"
)

func TestUUIDv4RoundTrip(t *testing.T) {
	for _, value := range testIDs {
		raw, err := ParseUUIDv4(value)
		if err != nil {
			t.Fatalf("ParseUUIDv4(%q): %v", value, err)
		}
		formatted, err := FormatUUIDv4(raw)
		if err != nil {
			t.Fatalf("FormatUUIDv4(%q): %v", value, err)
		}
		if formatted != value || !IsUUIDv4(value) {
			t.Fatalf("round trip = %q, valid=%v, want %q", formatted, IsUUIDv4(value), value)
		}
	}
}

func TestUUIDv4RejectsAdversarialValues(t *testing.T) {
	invalid := []string{
		"", "00000000-0000-4000-8000-000000000001x", "00000000-0000-4000-8000-00000000001",
		"000000000000-4000-8000-000000000001", "00000000_0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-00000000000-", "00000000-0000-4000-8000-00000000000g",
		"00000000-0000-4000-7000-000000000001", "00000000-0000-4000-c000-000000000001",
		"00000000-0000-3000-8000-000000000001", "00000000-0000-5000-8000-000000000001",
		"00000000-0000-4000-8000-00000000000A", "00000000-0000-4000-A000-000000000001",
		"00000000-0000-4000-8000-000000000001\x00", " 00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000001 ", "00000000%2d0000-4000-8000-000000000001",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			if raw, err := ParseUUIDv4(value); !errors.Is(err, ErrInvalidUUIDv4) || raw != [16]byte{} || IsUUIDv4(value) {
				t.Fatalf("ParseUUIDv4(%q) = %x, %v", value, raw, err)
			}
		})
	}
}

func TestFormatUUIDv4RejectsWrongVersionAndVariant(t *testing.T) {
	raw, err := ParseUUIDv4(testIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	raw[6] = raw[6]&0x0f | 0x50
	if _, err := FormatUUIDv4(raw); !errors.Is(err, ErrInvalidUUIDv4) {
		t.Fatalf("version 5 error = %v", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8] & 0x3f
	if _, err := FormatUUIDv4(raw); !errors.Is(err, ErrInvalidUUIDv4) {
		t.Fatalf("NCS variant error = %v", err)
	}
}

func FuzzUUIDv4(f *testing.F) {
	for _, seed := range append(testIDs[:], "", "00000000-0000-4000-8000-00000000000g", "00000000-0000-4000-A000-000000000001") {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		raw, err := ParseUUIDv4(value)
		if err != nil {
			if IsUUIDv4(value) {
				t.Fatalf("IsUUIDv4 accepted rejected value %q", value)
			}
			return
		}
		formatted, err := FormatUUIDv4(raw)
		if err != nil || formatted != value || len(value) != 36 {
			t.Fatalf("round trip %q = %q, %v", value, formatted, err)
		}
	})
}
