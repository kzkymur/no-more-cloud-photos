// Package readapi provides primitives shared by read repositories and HTTP handlers.
package readapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"
)

const (
	// These constants define the released cursor wire format. Changing a tuple
	// or field interpretation requires a new kind rather than reusing one.
	CursorMagicV1        = "NMCP"
	CursorVersionV1      = byte(0x01)
	CursorKindMediaV1    = byte(0x01)
	CursorKindJobsV1     = byte(0x02)
	CursorMACDomainV1    = "NMCP-CURSOR-HMAC-SHA256-V1\x00"
	CursorFilterDomainV1 = "NMCP-CURSOR-FILTER-SHA256-V1\x00"

	cursorHeaderSize      = len(CursorMagicV1) + 3
	cursorFingerprintSize = sha256.Size
	cursorTimestampSize   = 8
	cursorUUIDSize        = 16
	cursorPayloadSize     = cursorHeaderSize + cursorFingerprintSize + cursorTimestampSize + cursorUUIDSize
	cursorSize            = cursorPayloadSize + sha256.Size
	cursorEncodedSize     = (cursorSize*8 + 5) / 6

	cursorClassValue = byte(0x00)
	cursorClassNull  = byte(0x01)
)

// ErrInvalidCursor is returned for every invalid key, filter, position, or
// token. Decode deliberately exposes no detail that could be a validation
// oracle.
var ErrInvalidCursor = errors.New("invalid cursor")

type CursorCodec struct {
	key []byte
}

// MediaCursorFilter is the fully resolved result filter. HTTP defaults must be
// applied before use; Limit is deliberately not part of cursor identity.
type MediaCursorFilter struct {
	Profile string
	Deleted DeletedFilter
}

// MediaCursorPosition is a (taken_at DESC NULLS LAST, id DESC) keyset tuple.
type MediaCursorPosition struct {
	TakenAt *time.Time
	ID      string
}

// JobsCursorFilter contains the optional jobs result filters. Limit is
// deliberately not part of cursor identity.
type JobsCursorFilter struct {
	Status  *string
	MediaID *string
}

// JobsCursorPosition is a (created_at DESC, id DESC) keyset tuple.
type JobsCursorPosition struct {
	CreatedAt time.Time
	ID        string
}

// NewCursorCodec copies a secret key of at least 256 bits.
func NewCursorCodec(key []byte) (*CursorCodec, error) {
	if len(key) < sha256.Size {
		return nil, errors.New("cursor HMAC key must contain at least 32 bytes")
	}
	return &CursorCodec{key: append([]byte(nil), key...)}, nil
}

// EncodeMedia signs a media keyset position and its effective filter.
func (codec *CursorCodec) EncodeMedia(filter MediaCursorFilter, position MediaCursorPosition) (string, error) {
	if codec == nil || len(codec.key) < sha256.Size {
		return "", ErrInvalidCursor
	}
	fingerprint, ok := mediaFilterFingerprint(filter)
	if !ok {
		return "", ErrInvalidCursor
	}
	id, ok := parseUUIDv4(position.ID)
	if !ok {
		return "", ErrInvalidCursor
	}

	class := cursorClassNull
	var micros int64
	if position.TakenAt != nil {
		class = cursorClassValue
		var valid bool
		micros, valid = timestampMicros(*position.TakenAt)
		if !valid {
			return "", ErrInvalidCursor
		}
	}
	return codec.encode(CursorKindMediaV1, class, fingerprint, micros, id), nil
}

// DecodeMedia authenticates and decodes a media keyset position.
func (codec *CursorCodec) DecodeMedia(token string, filter MediaCursorFilter) (MediaCursorPosition, error) {
	fingerprint, ok := mediaFilterFingerprint(filter)
	if !ok {
		return MediaCursorPosition{}, ErrInvalidCursor
	}
	payload, ok := codec.authenticate(token)
	if !ok || !validHeader(payload, CursorKindMediaV1) || !hmac.Equal(payload[cursorHeaderSize:cursorHeaderSize+sha256.Size], fingerprint[:]) {
		return MediaCursorPosition{}, ErrInvalidCursor
	}

	class := payload[len(CursorMagicV1)+2]
	micros := int64(binary.BigEndian.Uint64(payload[cursorHeaderSize+sha256.Size : cursorHeaderSize+sha256.Size+8]))
	id, ok := decodeUUIDv4(payload[cursorPayloadSize-cursorUUIDSize : cursorPayloadSize])
	if !ok {
		return MediaCursorPosition{}, ErrInvalidCursor
	}

	position := MediaCursorPosition{ID: id}
	switch class {
	case cursorClassValue:
		value, valid := timeFromMicros(micros)
		if !valid {
			return MediaCursorPosition{}, ErrInvalidCursor
		}
		position.TakenAt = &value
	case cursorClassNull:
		if micros != 0 {
			return MediaCursorPosition{}, ErrInvalidCursor
		}
	default:
		return MediaCursorPosition{}, ErrInvalidCursor
	}
	return position, nil
}

// EncodeJobs signs a jobs keyset position and its effective filter.
func (codec *CursorCodec) EncodeJobs(filter JobsCursorFilter, position JobsCursorPosition) (string, error) {
	if codec == nil || len(codec.key) < sha256.Size {
		return "", ErrInvalidCursor
	}
	fingerprint, ok := jobsFilterFingerprint(filter)
	if !ok {
		return "", ErrInvalidCursor
	}
	id, ok := parseUUIDv4(position.ID)
	if !ok {
		return "", ErrInvalidCursor
	}
	micros, ok := timestampMicros(position.CreatedAt)
	if !ok {
		return "", ErrInvalidCursor
	}
	return codec.encode(CursorKindJobsV1, cursorClassValue, fingerprint, micros, id), nil
}

// DecodeJobs authenticates and decodes a jobs keyset position.
func (codec *CursorCodec) DecodeJobs(token string, filter JobsCursorFilter) (JobsCursorPosition, error) {
	fingerprint, ok := jobsFilterFingerprint(filter)
	if !ok {
		return JobsCursorPosition{}, ErrInvalidCursor
	}
	payload, ok := codec.authenticate(token)
	if !ok || !validHeader(payload, CursorKindJobsV1) || payload[len(CursorMagicV1)+2] != cursorClassValue || !hmac.Equal(payload[cursorHeaderSize:cursorHeaderSize+sha256.Size], fingerprint[:]) {
		return JobsCursorPosition{}, ErrInvalidCursor
	}

	micros := int64(binary.BigEndian.Uint64(payload[cursorHeaderSize+sha256.Size : cursorHeaderSize+sha256.Size+8]))
	createdAt, ok := timeFromMicros(micros)
	if !ok {
		return JobsCursorPosition{}, ErrInvalidCursor
	}
	id, ok := decodeUUIDv4(payload[cursorPayloadSize-cursorUUIDSize : cursorPayloadSize])
	if !ok {
		return JobsCursorPosition{}, ErrInvalidCursor
	}
	return JobsCursorPosition{CreatedAt: createdAt, ID: id}, nil
}

func (codec *CursorCodec) encode(kind, class byte, fingerprint [sha256.Size]byte, micros int64, id [cursorUUIDSize]byte) string {
	payload := make([]byte, 0, cursorSize)
	payload = append(payload, CursorMagicV1...)
	payload = append(payload, CursorVersionV1, kind, class)
	payload = append(payload, fingerprint[:]...)
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(micros))
	payload = append(payload, timestamp[:]...)
	payload = append(payload, id[:]...)

	mac := hmac.New(sha256.New, codec.key)
	_, _ = mac.Write([]byte(CursorMACDomainV1))
	_, _ = mac.Write(payload)
	payload = mac.Sum(payload)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func (codec *CursorCodec) authenticate(token string) ([]byte, bool) {
	if codec == nil || len(codec.key) < sha256.Size || len(token) != cursorEncodedSize {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != cursorSize || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return nil, false
	}
	payload := decoded[:cursorPayloadSize]
	mac := hmac.New(sha256.New, codec.key)
	_, _ = mac.Write([]byte(CursorMACDomainV1))
	_, _ = mac.Write(payload)
	if !hmac.Equal(decoded[cursorPayloadSize:], mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}

func validHeader(payload []byte, kind byte) bool {
	return len(payload) == cursorPayloadSize &&
		string(payload[:len(CursorMagicV1)]) == CursorMagicV1 &&
		payload[len(CursorMagicV1)] == CursorVersionV1 &&
		payload[len(CursorMagicV1)+1] == kind
}

func mediaFilterFingerprint(filter MediaCursorFilter) ([sha256.Size]byte, bool) {
	if !validCursorProfileKey(filter.Profile) {
		return [sha256.Size]byte{}, false
	}
	if !filter.Deleted.Valid() {
		return [sha256.Size]byte{}, false
	}
	canonical := make([]byte, 0, len(CursorFilterDomainV1)+6+len(filter.Profile)+len(filter.Deleted))
	canonical = append(canonical, CursorFilterDomainV1...)
	canonical = append(canonical, CursorVersionV1, CursorKindMediaV1)
	canonical = appendCanonicalString(canonical, 0x01, filter.Profile)
	canonical = appendCanonicalString(canonical, 0x02, string(filter.Deleted))
	return sha256.Sum256(canonical), true
}

func jobsFilterFingerprint(filter JobsCursorFilter) ([sha256.Size]byte, bool) {
	if filter.Status != nil && !JobStatus(*filter.Status).Valid() {
		return [sha256.Size]byte{}, false
	}
	if filter.MediaID != nil {
		if _, ok := parseUUIDv4(*filter.MediaID); !ok {
			return [sha256.Size]byte{}, false
		}
	}
	canonical := make([]byte, 0, len(CursorFilterDomainV1)+4+40)
	canonical = append(canonical, CursorFilterDomainV1...)
	canonical = append(canonical, CursorVersionV1, CursorKindJobsV1)
	canonical = appendOptionalCanonicalString(canonical, 0x01, filter.Status)
	canonical = appendOptionalCanonicalString(canonical, 0x02, filter.MediaID)
	return sha256.Sum256(canonical), true
}

func appendCanonicalString(destination []byte, tag byte, value string) []byte {
	destination = append(destination, tag, byte(len(value)))
	return append(destination, value...)
}

func appendOptionalCanonicalString(destination []byte, tag byte, value *string) []byte {
	destination = append(destination, tag)
	if value == nil {
		return append(destination, 0x00)
	}
	destination = append(destination, 0x01, byte(len(*value)))
	return append(destination, (*value)...)
}

func validCursorProfileKey(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range []byte(value[1:]) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func timestampMicros(value time.Time) (int64, bool) {
	utc := value.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 || utc.Nanosecond()%1000 != 0 {
		return 0, false
	}
	return utc.UnixMicro(), true
}

func timeFromMicros(micros int64) (time.Time, bool) {
	value := time.Unix(micros/1_000_000, (micros%1_000_000)*1_000).UTC()
	if value.Year() < 0 || value.Year() > 9999 || value.UnixMicro() != micros {
		return time.Time{}, false
	}
	return value, true
}

func parseUUIDv4(value string) ([cursorUUIDSize]byte, bool) {
	var id [cursorUUIDSize]byte
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return id, false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return id, false
	}
	var compact [32]byte
	for source, target := 0, 0; source < len(value); source++ {
		if value[source] == '-' {
			continue
		}
		compact[target] = value[source]
		target++
	}
	if _, err := hex.Decode(id[:], compact[:]); err != nil || formatCursorUUID(id) != value {
		return [cursorUUIDSize]byte{}, false
	}
	return id, true
}

func decodeUUIDv4(value []byte) (string, bool) {
	if len(value) != cursorUUIDSize {
		return "", false
	}
	var id [cursorUUIDSize]byte
	copy(id[:], value)
	if id[6]>>4 != 4 || id[8]>>6 != 2 {
		return "", false
	}
	return formatCursorUUID(id), true
}

func formatCursorUUID(id [cursorUUIDSize]byte) string {
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
