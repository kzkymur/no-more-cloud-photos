package readapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

var goldenKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

func TestMediaCursorGoldenV1(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	zone := time.FixedZone("golden +09", 9*60*60)
	takenAt := time.Date(2026, time.January, 2, 3, 4, 5, 123456000, zone)
	filter := MediaCursorFilter{Profile: "thumbnail", Deleted: DeletedInclude}
	position := MediaCursorPosition{TakenAt: &takenAt, ID: "00112233-4455-4677-8899-aabbccddeeff"}

	token, err := codec.EncodeMedia(filter, position)
	if err != nil {
		t.Fatalf("EncodeMedia: %v", err)
	}
	const wantFingerprint = "d484b7fe3b19e45ba82ab674b6d1cc27c75dbd7ad95746b90b576344ea67c7ca"
	const wantPayload = "4e4d4350010100d484b7fe3b19e45ba82ab674b6d1cc27c75dbd7ad95746b90b576344ea67c7ca000647576b1e118000112233445546778899aabbccddeeff"
	const wantToken = "Tk1DUAEBANSEt_47GeRbqCq2dLbRzCfHXb162VdGuQtXY0TqZ8fKAAZHV2seEYAAESIzRFVGd4iZqrvM3e7_A_lpc9lJd5wdTZ-J8bkQhhGYE6FZjF_RxpS9FnAfPAw"
	assertFrozenFingerprint(t, "NMCP-CURSOR-FILTER-SHA256-V1\x00\x01\x01\x01\x09thumbnail\x02\x07include", wantFingerprint)
	assertGoldenToken(t, token, wantToken, wantPayload, wantFingerprint)

	decoded, err := codec.DecodeMedia(wantToken, filter)
	if err != nil {
		t.Fatalf("DecodeMedia golden: %v", err)
	}
	if decoded.ID != position.ID || decoded.TakenAt == nil || decoded.TakenAt.Location() != time.UTC || !decoded.TakenAt.Equal(takenAt) {
		t.Fatalf("DecodeMedia() = %#v, want %#v in UTC", decoded, position)
	}
}

func TestJobsCursorGoldenV1(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	status := "running"
	mediaID := "ffeeddcc-bbaa-4988-8776-554433221100"
	filter := JobsCursorFilter{Status: &status, MediaID: &mediaID}
	position := JobsCursorPosition{
		CreatedAt: time.Date(1999, time.December, 31, 23, 59, 59, 999999000, time.UTC),
		ID:        "12345678-9abc-4def-8123-456789abcdef",
	}

	token, err := codec.EncodeJobs(filter, position)
	if err != nil {
		t.Fatalf("EncodeJobs: %v", err)
	}
	const wantFingerprint = "e2c0c221dbb9f6d075dce48d03708cbd8e19db279db48f05dd43173ccce9b95b"
	const wantPayload = "4e4d4350010200e2c0c221dbb9f6d075dce48d03708cbd8e19db279db48f05dd43173ccce9b95b00035d013b37dfff123456789abc4def8123456789abcdef"
	const wantToken = "Tk1DUAECAOLAwiHbufbQddzkjQNwjL2OGdsnnbSPBd1DFzzM6blbAANdATs33_8SNFZ4mrxN74EjRWeJq83v7LKtjl0mF532cfOuLy_fBU09i3e-EH3X9GTgnWYgPEw"
	assertFrozenFingerprint(t, "NMCP-CURSOR-FILTER-SHA256-V1\x00\x01\x02\x01\x01\x07running\x02\x01\x24ffeeddcc-bbaa-4988-8776-554433221100", wantFingerprint)
	assertGoldenToken(t, token, wantToken, wantPayload, wantFingerprint)

	decoded, err := codec.DecodeJobs(wantToken, filter)
	if err != nil {
		t.Fatalf("DecodeJobs golden: %v", err)
	}
	if decoded.ID != position.ID || decoded.CreatedAt.Location() != time.UTC || !decoded.CreatedAt.Equal(position.CreatedAt) {
		t.Fatalf("DecodeJobs() = %#v, want %#v", decoded, position)
	}
}

func TestCursorGoldenNullMediaTuple(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	filter := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
	token, err := codec.EncodeMedia(filter, MediaCursorPosition{ID: "ffffffff-ffff-4fff-bfff-ffffffffffff"})
	if err != nil {
		t.Fatalf("EncodeMedia null: %v", err)
	}
	const wantToken = "Tk1DUAEBAfiXwwA-ZraNaGmN-KTXQp4hVDZe-dMAAf8D4z558ecmAAAAAAAAAAD_______9P_7__________-JICnu5zZp_lbntBfLv5dzVP6tnXk69Nz1bt0Jvu7_c"
	if token != wantToken {
		t.Fatalf("null media token = %q, want %q", token, wantToken)
	}
	decoded, err := codec.DecodeMedia(wantToken, filter)
	if err != nil || decoded.TakenAt != nil {
		t.Fatalf("DecodeMedia null = %#v, %v", decoded, err)
	}
}

func TestCursorTamperEveryByte(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	mediaFilter := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
	when := time.Date(2025, 7, 8, 9, 10, 11, 12000, time.UTC)
	mediaToken, err := codec.EncodeMedia(mediaFilter, MediaCursorPosition{TakenAt: &when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	jobsToken, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: when, ID: "bbbbbbbb-bbbb-4bbb-9bbb-bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"media": mediaToken, "jobs": jobsToken} {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatal(err)
		}
		for index := range raw {
			tampered := append([]byte(nil), raw...)
			tampered[index] ^= 0x80
			candidate := base64.RawURLEncoding.EncodeToString(tampered)
			var decodeErr error
			if name == "media" {
				_, decodeErr = codec.DecodeMedia(candidate, mediaFilter)
			} else {
				_, decodeErr = codec.DecodeJobs(candidate, JobsCursorFilter{})
			}
			if !errors.Is(decodeErr, ErrInvalidCursor) {
				t.Fatalf("%s byte %d tamper error = %v", name, index, decodeErr)
			}
		}
	}
}

func TestCursorRouteAndEffectiveFilterBinding(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	when := time.Date(2020, 2, 3, 4, 5, 6, 7000, time.UTC)
	mediaFilter := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
	mediaToken, err := codec.EncodeMedia(mediaFilter, MediaCursorPosition{TakenAt: &when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.DecodeJobs(mediaToken, JobsCursorFilter{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("media token on jobs route: %v", err)
	}
	for _, filter := range []MediaCursorFilter{
		{Profile: "thumbnail", Deleted: DeletedExclude},
		{Profile: "standard", Deleted: DeletedInclude},
		{Profile: "standard", Deleted: DeletedOnly},
	} {
		if _, err := codec.DecodeMedia(mediaToken, filter); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("changed media filter %#v: %v", filter, err)
		}
	}

	statusA, statusB := "queued", "queued"
	mediaA, mediaB := "bbbbbbbb-bbbb-4bbb-9bbb-bbbbbbbbbbbb", "bbbbbbbb-bbbb-4bbb-9bbb-bbbbbbbbbbbb"
	jobsA := JobsCursorFilter{Status: &statusA, MediaID: &mediaA}
	jobsB := JobsCursorFilter{Status: &statusB, MediaID: &mediaB}
	jobsToken, err := codec.EncodeJobs(jobsA, JobsCursorPosition{CreatedAt: when, ID: "cccccccc-cccc-4ccc-accc-cccccccccccc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.DecodeJobs(jobsToken, jobsB); err != nil {
		t.Fatalf("equivalent effective filters did not match: %v", err)
	}
	failed := "failed"
	otherMediaID := "dddddddd-dddd-4ddd-bddd-dddddddddddd"
	for _, filter := range []JobsCursorFilter{
		{},
		{Status: &failed, MediaID: &mediaB},
		{Status: &statusB, MediaID: &otherMediaID},
		{Status: &statusB},
		{MediaID: &mediaB},
	} {
		if _, err := codec.DecodeJobs(jobsToken, filter); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("changed jobs filter %#v: %v", filter, err)
		}
	}
	if _, err := codec.DecodeMedia(jobsToken, mediaFilter); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("jobs token on media route: %v", err)
	}

	// Limit is intentionally absent from both canonical filter types, so callers
	// can change page size while continuing with the same cursor.
	defaultA := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
	defaultB := MediaCursorFilter{Profile: DefaultProfileKey, Deleted: DeletedExclude}
	if _, err := codec.DecodeMedia(mediaToken, defaultB); err != nil || defaultA != defaultB {
		t.Fatalf("equivalent default media filters failed: %v", err)
	}
}

func TestCursorStrictCanonicalBase64AndLengths(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	token, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != cursorEncodedSize || strings.Contains(token, "=") {
		t.Fatalf("encoded token length/padding = %d/%q", len(token), token)
	}
	variants := []string{
		token + "=",
		token[:len(token)-1],
		token + "A",
		strings.Repeat("A", 1<<20),
		strings.Replace(token, "-", "+", 1),
		strings.Replace(token, "_", "/", 1),
		token[:len(token)-1] + "\n",
		withNonzeroBase64PadBits(t, token),
	}
	for _, candidate := range variants {
		if candidate == token {
			continue
		}
		if _, err := codec.DecodeJobs(candidate, JobsCursorFilter{}); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("noncanonical token length %d error = %v", len(candidate), err)
		}
	}
}

func TestCursorAuthenticatedSemanticInvariants(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	mediaFilter := MediaCursorFilter{Profile: "standard", Deleted: DeletedExclude}
	token, err := codec.EncodeMedia(mediaFilter, MediaCursorPosition{TakenAt: &when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}

	checks := map[string]func([]byte){
		"magic":        func(value []byte) { value[0] ^= 1 },
		"version":      func(value []byte) { value[len(CursorMagicV1)]++ },
		"kind":         func(value []byte) { value[len(CursorMagicV1)+1] = CursorKindJobsV1 },
		"class":        func(value []byte) { value[len(CursorMagicV1)+2] = 0x7f },
		"uuid version": func(value []byte) { value[cursorPayloadSize-cursorUUIDSize+6] &^= 0xf0 },
		"uuid variant": func(value []byte) { value[cursorPayloadSize-cursorUUIDSize+8] &^= 0xc0 },
	}
	for name, mutate := range checks {
		candidate := append([]byte(nil), raw...)
		mutate(candidate)
		resign(candidate, goldenKey)
		if _, err := codec.DecodeMedia(base64.RawURLEncoding.EncodeToString(candidate), mediaFilter); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("authenticated %s error = %v", name, err)
		}
	}

	nullWithTimestamp := append([]byte(nil), raw...)
	nullWithTimestamp[len(CursorMagicV1)+2] = cursorClassNull
	resign(nullWithTimestamp, goldenKey)
	if _, err := codec.DecodeMedia(base64.RawURLEncoding.EncodeToString(nullWithTimestamp), mediaFilter); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("null tuple with timestamp: %v", err)
	}

	jobsToken, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	jobsRaw, _ := base64.RawURLEncoding.DecodeString(jobsToken)
	jobsRaw[len(CursorMagicV1)+2] = cursorClassNull
	resign(jobsRaw, goldenKey)
	if _, err := codec.DecodeJobs(base64.RawURLEncoding.EncodeToString(jobsRaw), JobsCursorFilter{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("jobs reserved class: %v", err)
	}

	outOfRangeTime := append([]byte(nil), jobsRaw...)
	outOfRangeTime[len(CursorMagicV1)+2] = cursorClassValue
	binary.BigEndian.PutUint64(outOfRangeTime[cursorHeaderSize+sha256.Size:], uint64(1<<63-1))
	resign(outOfRangeTime, goldenKey)
	if _, err := codec.DecodeJobs(base64.RawURLEncoding.EncodeToString(outOfRangeTime), JobsCursorFilter{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("jobs out-of-range time: %v", err)
	}
}

func TestCursorUUIDFilterAndTimeValidation(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	validTime := time.Date(2024, 1, 2, 3, 4, 5, 6000, time.UTC)
	validID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	invalidIDs := []string{
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
		"aaaaaaaa-aaaa-3aaa-8aaa-aaaaaaaaaaaa",
		"aaaaaaaa-aaaa-4aaa-7aaa-aaaaaaaaaaaa",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"not-a-uuid",
	}
	for _, id := range invalidIDs {
		if _, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: validTime, ID: id}); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("ID %q error = %v", id, err)
		}
	}
	badTimes := []time.Time{
		validTime.Add(time.Nanosecond),
		time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, value := range badTimes {
		if _, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: value, ID: validID}); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("time %v error = %v", value, err)
		}
	}
	if _, err := codec.EncodeMedia(MediaCursorFilter{Deleted: DeletedExclude}, MediaCursorPosition{ID: validID}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("empty profile error = %v", err)
	}
	if _, err := codec.EncodeMedia(MediaCursorFilter{Profile: "Bad", Deleted: DeletedExclude}, MediaCursorPosition{ID: validID}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("noncanonical profile error = %v", err)
	}
	if _, err := codec.EncodeMedia(MediaCursorFilter{Profile: "standard"}, MediaCursorPosition{ID: validID}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unset deleted filter error = %v", err)
	}
	badStatus := "unknown"
	if _, err := codec.EncodeJobs(JobsCursorFilter{Status: &badStatus}, JobsCursorPosition{CreatedAt: validTime, ID: validID}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad status error = %v", err)
	}
	badMediaID := strings.ToUpper(validID)
	if _, err := codec.EncodeJobs(JobsCursorFilter{MediaID: &badMediaID}, JobsCursorPosition{CreatedAt: validTime, ID: validID}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad media filter ID error = %v", err)
	}
}

func TestCursorRoundTripUTCAndExtremes(t *testing.T) {
	codec := mustCursorCodec(t, goldenKey)
	id := "01234567-89ab-4def-8012-3456789abcde"
	for _, value := range []time.Time{
		time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.FixedZone("west", -7*60*60)),
	} {
		token, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: value, ID: id})
		if value.UTC().Year() > 9999 {
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("UTC overflow %v error = %v", value, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("EncodeJobs(%v): %v", value, err)
		}
		decoded, err := codec.DecodeJobs(token, JobsCursorFilter{})
		if err != nil || decoded.CreatedAt.Location() != time.UTC || !decoded.CreatedAt.Equal(value) {
			t.Fatalf("round trip %v = %#v, %v", value, decoded, err)
		}
	}
}

func TestCursorKeyRequirementsAndCopy(t *testing.T) {
	if _, err := NewCursorCodec(make([]byte, sha256.Size-1)); err == nil {
		t.Fatal("NewCursorCodec accepted short key")
	}
	key := append([]byte(nil), goldenKey...)
	codec := mustCursorCodec(t, key)
	for index := range key {
		key[index] ^= 0xff
	}
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	token, err := codec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{CreatedAt: when, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mustCursorCodec(t, goldenKey).DecodeJobs(token, JobsCursorFilter{}); err != nil {
		t.Fatalf("codec did not copy key: %v", err)
	}
	var nilCodec *CursorCodec
	if _, err := nilCodec.EncodeJobs(JobsCursorFilter{}, JobsCursorPosition{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("nil codec encode error = %v", err)
	}
	if _, err := nilCodec.DecodeJobs("", JobsCursorFilter{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("nil codec decode error = %v", err)
	}
}

func assertGoldenToken(t *testing.T, gotToken, wantToken, wantPayload, wantFingerprint string) {
	t.Helper()
	if gotToken != wantToken {
		raw, _ := base64.RawURLEncoding.DecodeString(gotToken)
		t.Fatalf("token = %q, want %q; payload=%x fingerprint=%x", gotToken, wantToken, raw[:cursorPayloadSize], raw[cursorHeaderSize:cursorHeaderSize+sha256.Size])
	}
	raw, err := base64.RawURLEncoding.DecodeString(wantToken)
	if err != nil {
		t.Fatalf("decode frozen token: %v", err)
	}
	if got := hex.EncodeToString(raw[:cursorPayloadSize]); got != wantPayload {
		t.Fatalf("payload = %s, want %s", got, wantPayload)
	}
	if got := hex.EncodeToString(raw[cursorHeaderSize : cursorHeaderSize+sha256.Size]); got != wantFingerprint {
		t.Fatalf("fingerprint = %s, want %s", got, wantFingerprint)
	}
}

func assertFrozenFingerprint(t *testing.T, canonical, want string) {
	t.Helper()
	digest := sha256.Sum256([]byte(canonical))
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("independent canonical filter fingerprint = %s, want %s", got, want)
	}
}

func resign(raw, key []byte) {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(CursorMACDomainV1))
	_, _ = mac.Write(raw[:cursorPayloadSize])
	copy(raw[cursorPayloadSize:], mac.Sum(nil))
}

func withNonzeroBase64PadBits(t *testing.T, token string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, token[len(token)-1])
	if index < 0 || index&0x03 != 0 {
		t.Fatalf("unexpected canonical final base64 character %q", token[len(token)-1])
	}
	return token[:len(token)-1] + string(alphabet[index+1])
}

func mustCursorCodec(t *testing.T, key []byte) *CursorCodec {
	t.Helper()
	codec, err := NewCursorCodec(key)
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	return codec
}
