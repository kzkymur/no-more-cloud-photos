// Package storage implements canonical Core storage keys and durable Linux
// filesystem operations beneath a pinned storage root.
package storage

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
)

var ErrInvalidKey = errors.New("invalid storage key")

type uuidV4 [16]byte

type OriginalID struct{ uuidV4 }
type JobTargetID struct{ uuidV4 }
type RenditionID struct{ uuidV4 }
type AttemptID struct{ uuidV4 }
type QuarantineID struct{ uuidV4 }

func ParseOriginalID(value string) (OriginalID, error) {
	id, err := parseUUIDv4(value)
	return OriginalID{id}, err
}

func ParseJobTargetID(value string) (JobTargetID, error) {
	id, err := parseUUIDv4(value)
	return JobTargetID{id}, err
}

func ParseRenditionID(value string) (RenditionID, error) {
	id, err := parseUUIDv4(value)
	return RenditionID{id}, err
}

func ParseAttemptID(value string) (AttemptID, error) {
	id, err := parseUUIDv4(value)
	return AttemptID{id}, err
}

func ParseQuarantineID(value string) (QuarantineID, error) {
	id, err := parseUUIDv4(value)
	return QuarantineID{id}, err
}

func parseUUIDv4(value string) (uuidV4, error) {
	var result uuidV4
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return result, ErrInvalidKey
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return result, ErrInvalidKey
	}
	for _, character := range value {
		if character == '-' {
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return result, ErrInvalidKey
		}
	}
	compact := strings.ReplaceAll(value, "-", "")
	if _, err := hex.Decode(result[:], []byte(compact)); err != nil {
		return uuidV4{}, ErrInvalidKey
	}
	return result, nil
}

func (id uuidV4) String() string {
	encoded := hex.EncodeToString(id[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func (id uuidV4) valid() bool {
	return id[6]>>4 == 4 && id[8]>>6 == 2
}

func (id OriginalID) String() string   { return id.uuidV4.String() }
func (id JobTargetID) String() string  { return id.uuidV4.String() }
func (id RenditionID) String() string  { return id.uuidV4.String() }
func (id AttemptID) String() string    { return id.uuidV4.String() }
func (id QuarantineID) String() string { return id.uuidV4.String() }

type OriginalExtension uint8

const (
	OriginalJPEG OriginalExtension = iota + 1
	OriginalPNG
	OriginalGIF
	OriginalHEIC
	OriginalHEIF
	OriginalWebP
	OriginalBMP
	OriginalMP4
	OriginalMOV
	OriginalDNG
	OriginalNEF
	OriginalCR2
	OriginalCR3
	OriginalARW
	OriginalRAF
	OriginalORF
	OriginalRW2
)

var originalExtensions = map[OriginalExtension]string{
	OriginalJPEG: "jpg", OriginalPNG: "png", OriginalGIF: "gif", OriginalHEIC: "heic",
	OriginalHEIF: "heif", OriginalWebP: "webp", OriginalBMP: "bmp", OriginalMP4: "mp4",
	OriginalMOV: "mov", OriginalDNG: "dng", OriginalNEF: "nef", OriginalCR2: "cr2",
	OriginalCR3: "cr3", OriginalARW: "arw", OriginalRAF: "raf", OriginalORF: "orf", OriginalRW2: "rw2",
}

// OriginalExtensionForMIME maps the shared content-detected registry to a
// closed storage extension. Upload wiring uses this only after metadata probe
// success; user filenames and declared MIME types never enter this mapping.
func OriginalExtensionForMIME(mimeType string) (OriginalExtension, error) {
	format, ok := mediaformat.Lookup(mimeType)
	if !ok {
		return 0, ErrInvalidKey
	}
	extension, ok := lookupOriginalExtension(format.Extension)
	if !ok {
		return 0, ErrInvalidKey
	}
	return extension, nil
}

type RenditionExtension uint8

const (
	RenditionAVIF RenditionExtension = iota + 1
	RenditionAnimatedWebP
	RenditionMP4
)

var renditionExtensions = map[RenditionExtension]string{
	RenditionAVIF: "avif", RenditionAnimatedWebP: "webp", RenditionMP4: "mp4",
}

type OriginalKey struct {
	originalID OriginalID
	extension  OriginalExtension
}

type RenditionKey struct {
	originalID  OriginalID
	targetID    JobTargetID
	renditionID RenditionID
	extension   RenditionExtension
}

type QuarantineKey struct {
	id QuarantineID
}

type AttemptTempKey struct {
	value       string
	attemptID   AttemptID
	kind        AttemptTempKind
	originalID  OriginalID
	targetID    JobTargetID
	renditionID RenditionID
}

type AttemptTempKind uint8

const (
	OriginalUploadAttemptTemp AttemptTempKind = iota + 1
	OriginalFinalAttemptTemp
	RenditionFinalAttemptTemp
)

const AttemptTempGrace = 48 * time.Hour

func NewQuarantineKey(id QuarantineID) (QuarantineKey, error) {
	if !id.uuidV4.valid() {
		return QuarantineKey{}, ErrInvalidKey
	}
	return QuarantineKey{id: id}, nil
}

func ParseQuarantineKey(value string) (QuarantineKey, error) {
	if invalidRawKey(value) {
		return QuarantineKey{}, ErrInvalidKey
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != ".quarantine" {
		return QuarantineKey{}, ErrInvalidKey
	}
	id, err := ParseQuarantineID(parts[1])
	if err != nil {
		return QuarantineKey{}, ErrInvalidKey
	}
	key, _ := NewQuarantineKey(id)
	if key.String() != value {
		return QuarantineKey{}, ErrInvalidKey
	}
	return key, nil
}

func (key QuarantineKey) String() string             { return ".quarantine/" + key.id.String() }
func (key QuarantineKey) QuarantineID() QuarantineID { return key.id }

func NewOriginalUploadTempKey(originalID OriginalID, attemptID AttemptID) (AttemptTempKey, error) {
	if !originalID.uuidV4.valid() || !attemptID.uuidV4.valid() {
		return AttemptTempKey{}, ErrInvalidKey
	}
	id := originalID.String()
	return AttemptTempKey{
		value:     fmt.Sprintf("originals/%s/%s/.original.%s.tmp", id[:2], id, attemptID.String()),
		attemptID: attemptID, kind: OriginalUploadAttemptTemp, originalID: originalID,
	}, nil
}

func NewOriginalTempKey(key OriginalKey, attemptID AttemptID) (AttemptTempKey, error) {
	if _, err := ParseOriginalKey(key.String()); err != nil || !attemptID.uuidV4.valid() {
		return AttemptTempKey{}, ErrInvalidKey
	}
	directories, finalName := splitCanonicalKey(key.String())
	return AttemptTempKey{
		value:     strings.Join(directories, "/") + "/." + finalName + "." + attemptID.String() + ".tmp",
		attemptID: attemptID, kind: OriginalFinalAttemptTemp, originalID: key.originalID,
	}, nil
}

func NewRenditionTempKey(key RenditionKey, attemptID AttemptID) (AttemptTempKey, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil || !attemptID.uuidV4.valid() {
		return AttemptTempKey{}, ErrInvalidKey
	}
	directories, finalName := splitCanonicalKey(key.String())
	return AttemptTempKey{
		value:     strings.Join(directories, "/") + "/." + finalName + "." + attemptID.String() + ".tmp",
		attemptID: attemptID, kind: RenditionFinalAttemptTemp, originalID: key.originalID,
		targetID: key.targetID, renditionID: key.renditionID,
	}, nil
}

func ParseAttemptTempKey(value string) (AttemptTempKey, error) {
	if invalidRawKey(value) {
		return AttemptTempKey{}, ErrInvalidKey
	}
	parts := strings.Split(value, "/")
	if len(parts) < 4 {
		return AttemptTempKey{}, ErrInvalidKey
	}
	leaf := parts[len(parts)-1]
	const suffix = ".tmp"
	if !strings.HasPrefix(leaf, ".") || !strings.HasSuffix(leaf, suffix) {
		return AttemptTempKey{}, ErrInvalidKey
	}
	withoutSuffix := strings.TrimSuffix(leaf, suffix)
	dot := strings.LastIndexByte(withoutSuffix, '.')
	if dot <= 1 {
		return AttemptTempKey{}, ErrInvalidKey
	}
	attemptID, err := ParseAttemptID(withoutSuffix[dot+1:])
	if err != nil {
		return AttemptTempKey{}, ErrInvalidKey
	}
	base := withoutSuffix[1:dot]
	var canonical AttemptTempKey
	if parts[0] == "originals" && len(parts) == 4 {
		originalID, parseErr := ParseOriginalID(parts[2])
		if parseErr != nil || parts[1] != parts[2][:2] {
			return AttemptTempKey{}, ErrInvalidKey
		}
		if base == "original" {
			canonical, err = NewOriginalUploadTempKey(originalID, attemptID)
		} else {
			finalKey := strings.Join(parts[:3], "/") + "/" + base
			var originalKey OriginalKey
			originalKey, err = ParseOriginalKey(finalKey)
			if err == nil {
				canonical, err = NewOriginalTempKey(originalKey, attemptID)
			}
		}
	} else if parts[0] == "renditions" && len(parts) == 5 {
		finalKey := strings.Join(parts[:4], "/") + "/" + base
		var renditionKey RenditionKey
		renditionKey, err = ParseRenditionKey(finalKey)
		if err == nil {
			canonical, err = NewRenditionTempKey(renditionKey, attemptID)
		}
	} else {
		err = ErrInvalidKey
	}
	if err != nil || canonical.value != value {
		return AttemptTempKey{}, ErrInvalidKey
	}
	return canonical, nil
}

func (key AttemptTempKey) String() string         { return key.value }
func (key AttemptTempKey) AttemptID() AttemptID   { return key.attemptID }
func (key AttemptTempKey) Kind() AttemptTempKind  { return key.kind }
func (key AttemptTempKey) OriginalID() OriginalID { return key.originalID }
func (key AttemptTempKey) JobTargetID() (JobTargetID, bool) {
	return key.targetID, key.kind == RenditionFinalAttemptTemp
}
func (key AttemptTempKey) RenditionID() (RenditionID, bool) {
	return key.renditionID, key.kind == RenditionFinalAttemptTemp
}

// IsAgedAttemptTemp is candidate classification only. Reconciliation must
// separately prove there is no live or recent owning attempt before repair may
// quarantine the object; age never authorizes unlinking.
func IsAgedAttemptTemp(modifiedAt, now time.Time) bool {
	return !modifiedAt.After(now) && !modifiedAt.Add(AttemptTempGrace).After(now)
}

func splitCanonicalKey(value string) ([]string, string) {
	parts := strings.Split(value, "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func NewOriginalKey(originalID OriginalID, extension OriginalExtension) (OriginalKey, error) {
	if _, ok := originalExtensions[extension]; !ok || !originalID.uuidV4.valid() {
		return OriginalKey{}, ErrInvalidKey
	}
	return OriginalKey{originalID: originalID, extension: extension}, nil
}

func NewRenditionKey(originalID OriginalID, targetID JobTargetID, renditionID RenditionID, extension RenditionExtension) (RenditionKey, error) {
	if _, ok := renditionExtensions[extension]; !ok || !originalID.uuidV4.valid() || !targetID.uuidV4.valid() || !renditionID.uuidV4.valid() {
		return RenditionKey{}, ErrInvalidKey
	}
	return RenditionKey{originalID: originalID, targetID: targetID, renditionID: renditionID, extension: extension}, nil
}

func (key OriginalKey) String() string {
	id := key.originalID.String()
	return fmt.Sprintf("originals/%s/%s/original.%s", id[0:2], id, originalExtensions[key.extension])
}

func (key OriginalKey) OriginalID() OriginalID       { return key.originalID }
func (key OriginalKey) Extension() OriginalExtension { return key.extension }

func (key RenditionKey) String() string {
	originalID := key.originalID.String()
	return fmt.Sprintf("renditions/%s/%s/%s/%s.%s", originalID[0:2], originalID, key.targetID.String(), key.renditionID.String(), renditionExtensions[key.extension])
}

func (key RenditionKey) OriginalID() OriginalID        { return key.originalID }
func (key RenditionKey) JobTargetID() JobTargetID      { return key.targetID }
func (key RenditionKey) RenditionID() RenditionID      { return key.renditionID }
func (key RenditionKey) Extension() RenditionExtension { return key.extension }

func ParseOriginalKey(value string) (OriginalKey, error) {
	if invalidRawKey(value) {
		return OriginalKey{}, ErrInvalidKey
	}
	parts := strings.Split(value, "/")
	if len(parts) != 4 || parts[0] != "originals" || !strings.HasPrefix(parts[3], "original.") {
		return OriginalKey{}, ErrInvalidKey
	}
	id, err := ParseOriginalID(parts[2])
	if err != nil || parts[1] != parts[2][0:2] {
		return OriginalKey{}, ErrInvalidKey
	}
	extension, ok := lookupOriginalExtension(strings.TrimPrefix(parts[3], "original."))
	if !ok {
		return OriginalKey{}, ErrInvalidKey
	}
	key, _ := NewOriginalKey(id, extension)
	if key.String() != value {
		return OriginalKey{}, ErrInvalidKey
	}
	return key, nil
}

func ParseRenditionKey(value string) (RenditionKey, error) {
	if invalidRawKey(value) {
		return RenditionKey{}, ErrInvalidKey
	}
	return parseRenditionKey(value)
}

func parseRenditionKey(value string) (RenditionKey, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] != "renditions" {
		return RenditionKey{}, ErrInvalidKey
	}
	originalID, err := ParseOriginalID(parts[2])
	if err != nil || parts[1] != parts[2][0:2] {
		return RenditionKey{}, ErrInvalidKey
	}
	targetID, err := ParseJobTargetID(parts[3])
	if err != nil {
		return RenditionKey{}, ErrInvalidKey
	}
	dot := strings.LastIndexByte(parts[4], '.')
	if dot <= 0 {
		return RenditionKey{}, ErrInvalidKey
	}
	renditionID, err := ParseRenditionID(parts[4][:dot])
	if err != nil {
		return RenditionKey{}, ErrInvalidKey
	}
	extension, ok := lookupRenditionExtension(parts[4][dot+1:])
	if !ok {
		return RenditionKey{}, ErrInvalidKey
	}
	key, _ := NewRenditionKey(originalID, targetID, renditionID, extension)
	if key.String() != value {
		return RenditionKey{}, ErrInvalidKey
	}
	return key, nil
}

func invalidRawKey(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\%\x00") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return true
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return true
		}
	}
	return false
}

func lookupOriginalExtension(value string) (OriginalExtension, bool) {
	for extension, canonical := range originalExtensions {
		if value == canonical {
			return extension, true
		}
	}
	return 0, false
}

func lookupRenditionExtension(value string) (RenditionExtension, bool) {
	for extension, canonical := range renditionExtensions {
		if value == canonical {
			return extension, true
		}
	}
	return 0, false
}
