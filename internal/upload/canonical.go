// Package upload provides pure primitives for upload request processing.
package upload

import (
	"crypto/sha256"
	"encoding/binary"
)

const (
	// CanonicalRequestMagicV1 and CanonicalRequestVersionV1 identify the exact
	// persisted request-hash encoding. They must not change after release.
	CanonicalRequestMagicV1   = "NMCP-UPLOAD-REQUEST"
	CanonicalRequestVersionV1 = byte(0x01)

	CanonicalRequestTagSHA256       = byte(0x01)
	CanonicalRequestTagSize         = byte(0x02)
	CanonicalRequestTagFilenameNull = byte(0x03)
	CanonicalRequestTagFilenameUTF8 = byte(0x04)

	IdempotencyScopeMediaUpload = "POST /media"

	IdempotencyLockDomainV1   = "NMCP-UPLOAD-IDEMPOTENCY-LOCK-V1"
	ContentSHA256LockDomainV1 = "NMCP-UPLOAD-CONTENT-SHA256-LOCK-V1"
)

// CanonicalRequestBytesV1 encodes, in order, the raw SHA-256, the size as
// eight-byte big endian, and either a null filename or its normalized UTF-8
// bytes. Each field is tag || u64-be-length || value.
func CanonicalRequestBytesV1(contentSHA256 [sha256.Size]byte, size uint64, filename *string) []byte {
	filenameLength := 0
	if filename != nil {
		filenameLength = len(*filename)
	}
	encoded := make([]byte, 0, len(CanonicalRequestMagicV1)+1+9+sha256.Size+9+8+9+filenameLength)
	encoded = append(encoded, CanonicalRequestMagicV1...)
	encoded = append(encoded, CanonicalRequestVersionV1)
	encoded = appendField(encoded, CanonicalRequestTagSHA256, contentSHA256[:])

	var sizeBytes [8]byte
	binary.BigEndian.PutUint64(sizeBytes[:], size)
	encoded = appendField(encoded, CanonicalRequestTagSize, sizeBytes[:])
	if filename == nil {
		return appendField(encoded, CanonicalRequestTagFilenameNull, nil)
	}
	return appendField(encoded, CanonicalRequestTagFilenameUTF8, []byte(*filename))
}

func CanonicalRequestHashV1(contentSHA256 [sha256.Size]byte, size uint64, filename *string) [sha256.Size]byte {
	return sha256.Sum256(CanonicalRequestBytesV1(contentSHA256, size, filename))
}

func appendField(destination []byte, tag byte, value []byte) []byte {
	destination = append(destination, tag)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}

func IdempotencyAdvisoryLock(scope, key string) int64 {
	encoded := make([]byte, 0, len(IdempotencyLockDomainV1)+16+len(scope)+len(key))
	encoded = append(encoded, IdempotencyLockDomainV1...)
	encoded = appendLengthValue(encoded, scope)
	encoded = appendLengthValue(encoded, key)
	return advisoryLock(encoded)
}

func ContentSHA256AdvisoryLock(contentSHA256 [sha256.Size]byte) int64 {
	encoded := make([]byte, 0, len(ContentSHA256LockDomainV1)+sha256.Size)
	encoded = append(encoded, ContentSHA256LockDomainV1...)
	encoded = append(encoded, contentSHA256[:]...)
	return advisoryLock(encoded)
}

func appendLengthValue(destination []byte, value string) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}

func advisoryLock(value []byte) int64 {
	digest := sha256.Sum256(value)
	// A 64-bit collision only causes extra serialization. Callers must still
	// compare the database scope/key or full SHA-256 to establish identity.
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
