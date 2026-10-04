package readapi

import (
	"encoding/hex"
	"errors"
)

var ErrInvalidUUIDv4 = errors.New("identifier must be a lowercase canonical UUIDv4")

// ParseUUIDv4 validates a lowercase canonical UUIDv4 and returns its raw bytes
// for cursor encoding and database boundaries.
func ParseUUIDv4(value string) ([16]byte, error) {
	var raw [16]byte
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return raw, ErrInvalidUUIDv4
	}
	if value[14] != '4' || value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return raw, ErrInvalidUUIDv4
	}

	var compact [32]byte
	position := 0
	for index := range len(value) {
		if value[index] == '-' {
			continue
		}
		compact[position] = value[index]
		position++
	}
	if _, err := hex.Decode(raw[:], compact[:]); err != nil {
		return [16]byte{}, ErrInvalidUUIDv4
	}
	if formatUUID(raw) != value {
		return [16]byte{}, ErrInvalidUUIDv4
	}
	return raw, nil
}

func IsUUIDv4(value string) bool {
	_, err := ParseUUIDv4(value)
	return err == nil
}

// FormatUUIDv4 returns the canonical string for raw UUIDv4 bytes. It rejects
// other UUID versions and non-RFC-4122 variants rather than relabeling them.
func FormatUUIDv4(raw [16]byte) (string, error) {
	if raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return "", ErrInvalidUUIDv4
	}
	return formatUUID(raw), nil
}

func formatUUID(raw [16]byte) string {
	var result [36]byte
	hex.Encode(result[0:8], raw[0:4])
	result[8] = '-'
	hex.Encode(result[9:13], raw[4:6])
	result[13] = '-'
	hex.Encode(result[14:18], raw[6:8])
	result[18] = '-'
	hex.Encode(result[19:23], raw[8:10])
	result[23] = '-'
	hex.Encode(result[24:36], raw[10:16])
	return string(result[:])
}
