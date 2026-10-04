package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCanonicalRequestV1Golden(t *testing.T) {
	var contentSHA [sha256.Size]byte
	for index := range contentSHA {
		contentSHA[index] = byte(index)
	}
	filename := "é.jpg"
	wantBytes := mustDecodeHex(t,
		"4e4d43502d55504c4f41442d52455155455354"+ // NMCP-UPLOAD-REQUEST
			"01"+ // version
			"01"+"0000000000000020"+"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"+
			"02"+"0000000000000008"+"0102030405060708"+
			"04"+"0000000000000006"+"c3a92e6a7067")
	gotBytes := CanonicalRequestBytesV1(contentSHA, 0x0102030405060708, &filename)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("CanonicalRequestBytesV1() = %x, want %x", gotBytes, wantBytes)
	}

	wantHash := mustDecodeHash(t, "91d13f559a9d1656c002777e60a9af86a600f8ca55e396a96df091ade64902cf")
	if got := CanonicalRequestHashV1(contentSHA, 0x0102030405060708, &filename); got != wantHash {
		t.Fatalf("CanonicalRequestHashV1() = %x, want %x", got, wantHash)
	}
}

func TestCanonicalRequestV1NullFilenameGolden(t *testing.T) {
	var contentSHA [sha256.Size]byte
	wantBytes := mustDecodeHex(t,
		"4e4d43502d55504c4f41442d52455155455354"+
			"01"+
			"01"+"0000000000000020"+"0000000000000000000000000000000000000000000000000000000000000000"+
			"02"+"0000000000000008"+"0000000000000000"+
			"03"+"0000000000000000")
	gotBytes := CanonicalRequestBytesV1(contentSHA, 0, nil)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("CanonicalRequestBytesV1() = %x, want %x", gotBytes, wantBytes)
	}

	wantHash := mustDecodeHash(t, "427e1b1bbc329ee0f2d616c5a77b3fa799784b48c27f561ccdf056676d19c272")
	if got := CanonicalRequestHashV1(contentSHA, 0, nil); got != wantHash {
		t.Fatalf("CanonicalRequestHashV1() = %x, want %x", got, wantHash)
	}
}

func TestCanonicalRequestV1DistinguishesNullAndEmptyFilename(t *testing.T) {
	var contentSHA [sha256.Size]byte
	empty := ""
	nullBytes := CanonicalRequestBytesV1(contentSHA, 0, nil)
	emptyBytes := CanonicalRequestBytesV1(contentSHA, 0, &empty)
	if string(nullBytes) == string(emptyBytes) {
		t.Fatal("null and empty filename encodings are equal")
	}
	if CanonicalRequestHashV1(contentSHA, 0, nil) == CanonicalRequestHashV1(contentSHA, 0, &empty) {
		t.Fatal("null and empty filename hashes are equal")
	}
}

func TestLengthPrefixesPreventCanonicalAmbiguity(t *testing.T) {
	first := IdempotencyAdvisoryLock("ab", "c")
	second := IdempotencyAdvisoryLock("a", "bc")
	if first == second {
		t.Fatal("scope/key boundary was ambiguous")
	}

	var contentSHA [sha256.Size]byte
	nameA := "ab"
	nameB := "a"
	if string(CanonicalRequestBytesV1(contentSHA, 1, &nameA)) == string(CanonicalRequestBytesV1(contentSHA, 0x0162, &nameB)) {
		t.Fatal("canonical fields were ambiguous")
	}
}

func TestAdvisoryLockGoldenAndDomains(t *testing.T) {
	var contentSHA [sha256.Size]byte
	for index := range contentSHA {
		contentSHA[index] = byte(index)
	}
	if got, want := IdempotencyAdvisoryLock(IdempotencyScopeMediaUpload, "example-key"), int64(-5421086791864445937); got != want {
		t.Fatalf("IdempotencyAdvisoryLock() = %d, want %d", got, want)
	}
	if got, want := ContentSHA256AdvisoryLock(contentSHA), int64(5729185527926754934); got != want {
		t.Fatalf("ContentSHA256AdvisoryLock() = %d, want %d", got, want)
	}
	if got := IdempotencyAdvisoryLock("", string(contentSHA[:])); got == ContentSHA256AdvisoryLock(contentSHA) {
		t.Fatal("separate lock domains produced the same key")
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode golden hex: %v", err)
	}
	return decoded
}

func mustDecodeHash(t *testing.T, value string) [sha256.Size]byte {
	t.Helper()
	var result [sha256.Size]byte
	decoded := mustDecodeHex(t, value)
	if len(decoded) != len(result) {
		t.Fatalf("golden hash length = %d, want %d", len(decoded), len(result))
	}
	copy(result[:], decoded)
	return result
}
