package upload

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestValidateIdempotencyKey(t *testing.T) {
	valid := []string{"!", "~", "a-key/is+opaque", strings.Repeat("x", 128)}
	for _, key := range valid {
		if err := ValidateIdempotencyKey(key); err != nil {
			t.Errorf("ValidateIdempotencyKey(%q) error = %v", key, err)
		}
	}

	invalid := []string{"", strings.Repeat("x", 129), "has space", "tab\t", "line\n", "del\x7f", "non-ascii-é"}
	for _, key := range invalid {
		if err := ValidateIdempotencyKey(key); err == nil {
			t.Errorf("ValidateIdempotencyKey(%q) unexpectedly succeeded", key)
		}
	}
}

func TestValidateIdempotencyKeyAcceptsExactlyVisibleASCII(t *testing.T) {
	for value := 0; value <= 0xff; value++ {
		key := string([]byte{byte(value)})
		err := ValidateIdempotencyKey(key)
		wantValid := value >= '!' && value <= '~'
		if (err == nil) != wantValid {
			t.Errorf("ValidateIdempotencyKey(byte(%#x)) error = %v, want valid %v", value, err, wantValid)
		}
	}
}

func TestNormalizeFilename(t *testing.T) {
	tests := []struct {
		name  string
		input *string
		want  *string
	}{
		{name: "absent"},
		{name: "empty", input: stringPointer("")},
		{name: "slash only", input: stringPointer("///")},
		{name: "dot", input: stringPointer("path/.///")},
		{name: "dot dot", input: stringPointer(`C:\fakepath\..`)},
		{name: "POSIX path", input: stringPointer("home/user/photo.jpg"), want: stringPointer("photo.jpg")},
		{name: "Windows path", input: stringPointer(`C:\fakepath\photo.jpg`), want: stringPointer("photo.jpg")},
		{name: "trailing separators", input: stringPointer("path/photo.jpg///"), want: stringPointer("photo.jpg")},
		{name: "NFC", input: stringPointer("path/e\u0301.jpg"), want: stringPointer("é.jpg")},
		{name: "preserves spaces and case", input: stringPointer("path/ Photo.JPG "), want: stringPointer(" Photo.JPG ")},
		{name: "preserves replacement rune", input: stringPointer("photo-�.jpg"), want: stringPointer("photo-�.jpg")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeFilename(test.input)
			if err != nil {
				t.Fatalf("NormalizeFilename() error = %v", err)
			}
			if !equalOptionalString(got, test.want) {
				t.Fatalf("NormalizeFilename() = %v, want %v", optionalString(got), optionalString(test.want))
			}
		})
	}
}

func TestNormalizeFilenameRejectsInvalidInput(t *testing.T) {
	invalidUTF8 := string([]byte{'a', 0xff, 'b'})
	tooLongASCII := strings.Repeat("a", MaxFilenameBytes+1)
	tooLongAfterNFC := strings.Repeat("e\u0301", 128)
	invalid := []string{
		invalidUTF8,
		"nul\x00name",
		"del\x7fname",
		"line\nname",
		"unicode-control\u0085name",
		"path/control\u0085/name.jpg",
		tooLongASCII,
		tooLongAfterNFC,
	}
	for _, filename := range invalid {
		if got, err := NormalizeFilename(&filename); err == nil {
			t.Errorf("NormalizeFilename(%q) = %v, want error", filename, optionalString(got))
		}
	}
}

func TestNormalizeFilenameRejectsEveryUnicodeControl(t *testing.T) {
	for character := rune(0); character <= utf8.MaxRune; character++ {
		if !unicode.IsControl(character) {
			continue
		}
		filename := "a" + string(character) + "b"
		if got, err := NormalizeFilename(&filename); err == nil {
			t.Errorf("NormalizeFilename() accepted control U+%04X as %v", character, optionalString(got))
		}
	}
}

func TestNormalizeFilenameAccepts255UTF8Bytes(t *testing.T) {
	filename := strings.Repeat("a", MaxFilenameBytes-2) + "é"
	got, err := NormalizeFilename(&filename)
	if err != nil {
		t.Fatalf("NormalizeFilename() error = %v", err)
	}
	if got == nil {
		t.Fatal("NormalizeFilename() = nil")
	}
	if *got != filename || len(*got) != MaxFilenameBytes {
		t.Fatalf("NormalizeFilename() = %v (%d bytes)", optionalString(got), len(*got))
	}
}

func FuzzNormalizeFilename(f *testing.F) {
	for _, seed := range []string{"photo.jpg", `C:\fakepath\photo.jpg`, "e\u0301.jpg", "", "a\x00b", string([]byte{0xff})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := NormalizeFilename(&input)
		if err != nil || got == nil {
			return
		}
		if !utf8.ValidString(*got) || !norm.NFC.IsNormalString(*got) {
			t.Fatalf("successful result is not valid NFC UTF-8: %q", *got)
		}
		if len(*got) > MaxFilenameBytes || strings.ContainsAny(*got, `/\`) || *got == "." || *got == ".." {
			t.Fatalf("successful result violates basename contract: %q", *got)
		}
		for _, character := range *got {
			if character == 0x7f || unicode.IsControl(character) {
				t.Fatalf("successful result contains control character U+%04X", character)
			}
		}
	})
}

func stringPointer(value string) *string { return &value }

func equalOptionalString(first, second *string) bool {
	return first == nil && second == nil || first != nil && second != nil && *first == *second
}

func optionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
