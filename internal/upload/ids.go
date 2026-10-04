package upload

import (
	"crypto/rand"
	"encoding/hex"
	"io"
)

func NewUUIDv4() (string, error) {
	return newUUIDv4(rand.Reader)
}

func newUUIDv4(random io.Reader) (string, error) {
	var id [16]byte
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return formatUUID(id), nil
}

func IsUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	var id [16]byte
	encoded := make([]byte, 0, 32)
	for index := range len(value) {
		if value[index] != '-' {
			encoded = append(encoded, value[index])
		}
	}
	if _, err := hex.Decode(id[:], encoded); err != nil {
		return false
	}
	return formatUUID(id) == value
}

func formatUUID(id [16]byte) string {
	var result [36]byte
	hex.Encode(result[0:8], id[0:4])
	result[8] = '-'
	hex.Encode(result[9:13], id[4:6])
	result[13] = '-'
	hex.Encode(result[14:18], id[6:8])
	result[18] = '-'
	hex.Encode(result[19:23], id[8:10])
	result[23] = '-'
	hex.Encode(result[24:36], id[10:16])
	return string(result[:])
}
