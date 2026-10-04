package upload

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const MaxFilenameBytes = 255

// NormalizeFilename normalizes optional untrusted display metadata. A nil,
// empty, path with no non-empty component, ".", or ".." result is nil.
func NormalizeFilename(filename *string) (*string, error) {
	if filename == nil || *filename == "" {
		return nil, nil
	}
	if !utf8.ValidString(*filename) {
		return nil, fmt.Errorf("filename is not valid UTF-8")
	}
	for _, character := range *filename {
		if character == 0 || character == 0x7f || unicode.IsControl(character) {
			return nil, fmt.Errorf("filename contains a control character")
		}
	}

	path := strings.ReplaceAll(*filename, `\`, "/")
	components := strings.Split(path, "/")
	var basename string
	for index := len(components) - 1; index >= 0; index-- {
		if components[index] != "" {
			basename = components[index]
			break
		}
	}
	if basename == "" || basename == "." || basename == ".." {
		return nil, nil
	}

	basename = norm.NFC.String(basename)
	if len(basename) > MaxFilenameBytes {
		return nil, fmt.Errorf("normalized filename exceeds %d UTF-8 bytes", MaxFilenameBytes)
	}
	return &basename, nil
}

func ValidateIdempotencyKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return fmt.Errorf("idempotency key must be 1-128 bytes")
	}
	for _, character := range []byte(key) {
		if character < '!' || character > '~' {
			return fmt.Errorf("idempotency key must contain only visible ASCII")
		}
	}
	return nil
}
