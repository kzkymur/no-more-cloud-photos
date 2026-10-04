package readapi

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeFileBaseURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://files.example/files", "https://files.example/files/"},
		{"https://files.example/prefix/files/", "https://files.example/prefix/files/"},
		{"https://files.example/photo%20root/files", "https://files.example/photo%20root/files/"},
		{"https://[2001:db8::1]:8443/files", "https://[2001:db8::1]:8443/files/"},
	}
	for _, test := range tests {
		got, err := normalizeFileBaseURL(test.input)
		if err != nil || got != test.want {
			t.Errorf("normalizeFileBaseURL(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}

	invalid := []string{
		"", "http://files.example/files", "//files.example/files", "https://files.example", "https://files.example/",
		"https://user@files.example/files", "https://files.example/files?token=x", "https://files.example/files?",
		"https://files.example/files#fragment", "https://files.example/files#", "https://files.example/a/../files",
		"https://files.example/a//files", "https://files.example/files//", "https://files.example/%66iles",
		"https://files.example/files%2fother", " https://files.example/files", "https://files.example/files ",
	}
	for _, value := range invalid {
		if got, err := normalizeFileBaseURL(value); err == nil {
			t.Errorf("normalizeFileBaseURL(%q) = %q, want error", value, got)
		}
	}
}

func TestFileURLUsesConfiguredRootAndCanonicalTypedKey(t *testing.T) {
	service := &Service{fileBaseURL: "https://files.example/prefix/files/"}
	originalKey := "originals/10/10000000-0000-4000-8000-000000000005/original.jpg"
	got, err := service.originalURL(originalKey, testIDs[4])
	if err != nil || got != "https://files.example/prefix/files/"+originalKey {
		t.Fatalf("originalURL() = %q, %v", got, err)
	}
	if strings.Contains(got, "/files/files/") {
		t.Fatalf("URL added an implicit files segment: %q", got)
	}

	for _, key := range []string{"/etc/passwd", "originals/../../secret", "originals%2fsecret", "originals/10/not-an-id/original.jpg"} {
		if _, err := service.originalURL(key, testIDs[4]); !IsKind(err, KindInvariant) {
			t.Errorf("originalURL(%q) error = %#v, want invariant", key, err)
		}
	}
	if _, err := service.originalURL(originalKey, testIDs[0]); !IsKind(err, KindInvariant) {
		t.Fatalf("valid key for another original error = %#v, want invariant", err)
	}
	if _, err := service.renditionURL("/var/lib/nmcp/output.avif", testIDs[4], testIDs[3], testIDs[1]); !IsKind(err, KindInvariant) {
		t.Fatalf("absolute rendition path error = %#v, want invariant", err)
	}
}

func TestKeysetPredicatesEncodeExactDescendingOrders(t *testing.T) {
	mediaFragments := []string{
		"m.taken_at IS NULL AND m.id < $6::uuid",
		"m.taken_at < $5",
		"m.taken_at = $5 AND m.id < $6::uuid",
		"OR m.taken_at IS NULL",
	}
	for _, fragment := range mediaFragments {
		if !strings.Contains(mediaCursorPredicate, fragment) {
			t.Errorf("media predicate lacks %q: %s", fragment, mediaCursorPredicate)
		}
	}
	if !strings.Contains(jobsCursorPredicate, "(j.created_at, j.id) < ($5::timestamptz, $6::uuid)") {
		t.Fatalf("jobs predicate does not match descending tuple order: %s", jobsCursorPredicate)
	}
	if !strings.Contains(mediaListSQL, "ORDER BY m.taken_at DESC NULLS LAST,m.id DESC") || !strings.Contains(jobHeadersSQL, "ORDER BY j.created_at DESC,j.id DESC") {
		t.Fatal("list SQL order and keyset predicates diverged")
	}
}

func TestBuildRenditionRejectsNullableAndProvenanceInconsistency(t *testing.T) {
	service := &Service{fileBaseURL: "https://files.example/files/"}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("offset", 9*60*60))
	profileKey := "standard"
	mimeType := "image/avif"
	size := int64(42)
	version := 1
	sha := sixtyFourZeroes
	path := "renditions/10/10000000-0000-4000-8000-000000000005/00000000-0000-4000-b000-000000000004/00000000-0000-4000-9000-000000000002.avif"
	valid := nullableRendition{
		ID: &testIDs[1], MediaID: &testIDs[0], JobTargetID: &testIDs[3], OriginalID: &testIDs[4], ProfileKey: &profileKey,
		ProfileID: &testIDs[2], JoinedProfileKey: &profileKey, ProfileVersion: &version, MIMEType: &mimeType,
		SizeBytes: &size, SHA256: &sha, RelativePath: &path, CreatedAt: &now,
	}
	got, err := service.buildRendition(valid, testIDs[0], profileKey)
	if err != nil || got == nil || got.CreatedAt.Location() != time.UTC || got.FileURL != "https://files.example/files/"+path {
		t.Fatalf("buildRendition(valid) = %#v, %v", got, err)
	}

	partial := nullableRendition{MediaID: &testIDs[0]}
	if _, err := service.buildRendition(partial, "", ""); !IsKind(err, KindInvariant) {
		t.Fatalf("partial null row error = %#v, want invariant", err)
	}
	inconsistent := valid
	otherKey := "thumbnail"
	inconsistent.JoinedProfileKey = &otherKey
	if _, err := service.buildRendition(inconsistent, "", ""); !IsKind(err, KindInvariant) {
		t.Fatalf("provenance error = %#v, want invariant", err)
	}
	invalidPath := valid
	absolute := "/srv/private/output.avif"
	invalidPath.RelativePath = &absolute
	if _, err := service.buildRendition(invalidPath, "", ""); !IsKind(err, KindInvariant) {
		t.Fatalf("path error = %#v, want invariant", err)
	}
	wrongObjectPath := valid
	otherRenditionPath := "renditions/10/10000000-0000-4000-8000-000000000005/00000000-0000-4000-b000-000000000004/00000000-0000-4000-8000-000000000001.avif"
	wrongObjectPath.RelativePath = &otherRenditionPath
	if _, err := service.buildRendition(wrongObjectPath, "", ""); !IsKind(err, KindInvariant) {
		t.Fatalf("valid key for another rendition error = %#v, want invariant", err)
	}
}

func TestDatabaseErrorClassification(t *testing.T) {
	for _, code := range []string{"08006", "40001", "53000", "53100", "53200", "53300", "53400", "55P03", "57014", "57P01", "57P02", "57P03"} {
		err := classifyDatabaseError(&pgconn.PgError{Code: code})
		if !IsKind(err, KindDatabaseUnavailable) {
			t.Errorf("SQLSTATE %s classified as %#v", code, err)
		}
	}
	if err := classifyDatabaseError(&pgconn.PgError{Code: "23514"}); !IsKind(err, KindInvariant) {
		t.Fatalf("data SQLSTATE classified as %#v", err)
	}
	if err := classifyDatabaseError(errors.New("scan failed at /private/path")); !IsKind(err, KindInvariant) {
		t.Fatalf("scan failure classified as %#v", err)
	}
}
