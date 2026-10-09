package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
)

func TestEveryDetectedMIMEHasClosedOriginalExtension(t *testing.T) {
	for _, format := range mediaformat.Formats() {
		extension, err := OriginalExtensionForMIME(format.MIMEType)
		if err != nil {
			t.Fatalf("OriginalExtensionForMIME(%q): %v", format.MIMEType, err)
		}
		if got := originalExtensions[extension]; got != format.Extension {
			t.Fatalf("extension for %q = %q, want %q", format.MIMEType, got, format.Extension)
		}
	}
	if _, err := OriginalExtensionForMIME("image/tiff"); err == nil {
		t.Fatal("unknown MIME mapped to storage extension")
	}
}

const (
	testOriginalID   = "01234567-89ab-4cde-8f01-23456789abcd"
	testTargetID     = "11111111-2222-4333-8444-555555555555"
	testRenditionID  = "22222222-3333-4444-8555-666666666666"
	testAttemptID    = "33333333-4444-4555-8666-777777777777"
	testQuarantineID = "44444444-5555-4666-8777-888888888888"
)

func TestCanonicalKeys(t *testing.T) {
	originalID, err := ParseOriginalID(testOriginalID)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := ParseJobTargetID(testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	renditionID, err := ParseRenditionID(testRenditionID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := NewOriginalKey(originalID, OriginalJPEG)
	if err != nil {
		t.Fatal(err)
	}
	rendition, err := NewRenditionKey(originalID, targetID, renditionID, RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := original.String(), "originals/01/"+testOriginalID+"/original.jpg"; got != want {
		t.Fatalf("OriginalKey.String() = %q, want %q", got, want)
	}
	if got, want := rendition.String(), "renditions/01/"+testOriginalID+"/"+testTargetID+"/"+testRenditionID+".avif"; got != want {
		t.Fatalf("RenditionKey.String() = %q, want %q", got, want)
	}
	parsedOriginal, err := ParseOriginalKey(original.String())
	if err != nil || parsedOriginal.String() != original.String() {
		t.Fatalf("ParseOriginalKey() = %q, %v", parsedOriginal.String(), err)
	}
	parsedRendition, err := ParseRenditionKey(rendition.String())
	if err != nil || parsedRendition.String() != rendition.String() {
		t.Fatalf("ParseRenditionKey() = %q, %v", parsedRendition.String(), err)
	}
}

func TestQuarantineAndAttemptTempKeysAreClosed(t *testing.T) {
	originalID, _ := ParseOriginalID(testOriginalID)
	targetID, _ := ParseJobTargetID(testTargetID)
	renditionID, _ := ParseRenditionID(testRenditionID)
	attemptID, _ := ParseAttemptID(testAttemptID)
	quarantineID, _ := ParseQuarantineID(testQuarantineID)
	original, _ := NewOriginalKey(originalID, OriginalJPEG)
	rendition, _ := NewRenditionKey(originalID, targetID, renditionID, RenditionAVIF)
	quarantine, err := NewQuarantineKey(quarantineID)
	if err != nil || quarantine.String() != ".quarantine/"+testQuarantineID {
		t.Fatalf("NewQuarantineKey() = %q, %v", quarantine.String(), err)
	}
	if parsed, err := ParseQuarantineKey(quarantine.String()); err != nil || parsed.String() != quarantine.String() {
		t.Fatalf("ParseQuarantineKey() = %q, %v", parsed.String(), err)
	}
	constructors := []func() (AttemptTempKey, error){
		func() (AttemptTempKey, error) { return NewOriginalUploadTempKey(originalID, attemptID) },
		func() (AttemptTempKey, error) { return NewOriginalTempKey(original, attemptID) },
		func() (AttemptTempKey, error) { return NewRenditionTempKey(rendition, attemptID) },
	}
	want := []string{
		"originals/01/" + testOriginalID + "/.original." + testAttemptID + ".tmp",
		"originals/01/" + testOriginalID + "/.original.jpg." + testAttemptID + ".tmp",
		"renditions/01/" + testOriginalID + "/" + testTargetID + "/." + testRenditionID + ".avif." + testAttemptID + ".tmp",
	}
	wantKinds := []AttemptTempKind{OriginalUploadAttemptTemp, OriginalFinalAttemptTemp, RenditionFinalAttemptTemp}
	for index, construct := range constructors {
		key, err := construct()
		if err != nil || key.String() != want[index] || key.AttemptID().String() != testAttemptID ||
			key.Kind() != wantKinds[index] || key.OriginalID().String() != testOriginalID {
			t.Fatalf("temp constructor %d = %q, %v", index, key.String(), err)
		}
		parsed, err := ParseAttemptTempKey(key.String())
		if err != nil || parsed.String() != key.String() || parsed.AttemptID().String() != testAttemptID || parsed.Kind() != wantKinds[index] {
			t.Fatalf("ParseAttemptTempKey(%q) = %q, %v", key.String(), parsed.String(), err)
		}
		parsedTarget, hasTarget := parsed.JobTargetID()
		parsedRendition, hasRendition := parsed.RenditionID()
		if index == 2 && (!hasTarget || !hasRendition || parsedTarget.String() != testTargetID || parsedRendition.String() != testRenditionID) {
			t.Fatalf("rendition temp identity = %s/%t %s/%t", parsedTarget.String(), hasTarget, parsedRendition.String(), hasRendition)
		}
		if index != 2 && (hasTarget || hasRendition) {
			t.Fatalf("original temp exposed rendition identity: target=%t rendition=%t", hasTarget, hasRendition)
		}
	}
	for _, value := range []string{
		"quarantine/" + testQuarantineID,
		".quarantine/../" + testQuarantineID,
		want[0] + ".old",
		strings.Replace(want[0], ".original.", "original.", 1),
		strings.Replace(want[1], ".jpg.", ".exe.", 1),
		strings.Replace(want[2], testOriginalID, strings.ToUpper(testOriginalID), 1),
		"originals/02/" + testOriginalID + "/.original." + testAttemptID + ".tmp",
	} {
		if _, err := ParseQuarantineKey(value); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("ParseQuarantineKey(%q) error = %v", value, err)
		}
		if _, err := ParseAttemptTempKey(value); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("ParseAttemptTempKey(%q) error = %v", value, err)
		}
	}
}

func TestAttemptTempAgeIsCandidateOnlyAtFortyEightHours(t *testing.T) {
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		modified time.Time
		want     bool
	}{
		{name: "future", modified: now.Add(time.Nanosecond)},
		{name: "recent", modified: now.Add(-AttemptTempGrace + time.Nanosecond)},
		{name: "exact", modified: now.Add(-AttemptTempGrace), want: true},
		{name: "older", modified: now.Add(-AttemptTempGrace - time.Nanosecond), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsAgedAttemptTemp(test.modified, now); got != test.want {
				t.Fatalf("IsAgedAttemptTemp() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestKeyParsersRejectNoncanonicalAndTraversalInputs(t *testing.T) {
	validOriginal := "originals/01/" + testOriginalID + "/original.jpg"
	validRendition := "renditions/01/" + testOriginalID + "/" + testTargetID + "/" + testRenditionID + ".avif"
	invalid := []string{
		"", "/" + validOriginal, "../" + validOriginal, "originals/../" + testOriginalID + "/original.jpg",
		"originals//" + testOriginalID + "/original.jpg", strings.Replace(validOriginal, "/", "\\", 1),
		strings.Replace(validOriginal, "originals", "%6friginals", 1), strings.Replace(validOriginal, "01", "%2e%2e", 1),
		strings.Replace(validOriginal, "01", "02", 1), strings.ToUpper(validOriginal), validOriginal + "/",
		strings.Replace(validOriginal, ".jpg", ".exe", 1), strings.Replace(validOriginal, "original.jpg", "name.jpg", 1),
		"originals/01/" + testOriginalID + "/./original.jpg", "C:" + validOriginal, validOriginal + "\x00",
		strings.Replace(validRendition, ".avif", ".jpg", 1), validRendition + "/extra", strings.Replace(validRendition, testTargetID, "..", 1),
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseOriginalKey(value); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("ParseOriginalKey(%q) error = %v", value, err)
			}
			if _, err := ParseRenditionKey(value); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("ParseRenditionKey(%q) error = %v", value, err)
			}
		})
	}
}

func TestUUIDParsersRequireCanonicalV4(t *testing.T) {
	for _, value := range []string{
		strings.ToUpper(testOriginalID),
		"01234567-89ab-3cde-8f01-23456789abcd",
		"01234567-89ab-4cde-7f01-23456789abcd",
		"0123456789ab4cde8f0123456789abcd",
		"01234567-89ab-4cde-8f01-23456789abcg",
	} {
		if _, err := ParseOriginalID(value); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("ParseOriginalID(%q) error = %v", value, err)
		}
	}
	if _, err := NewOriginalKey(OriginalID{}, OriginalJPEG); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("zero OriginalID constructor error = %v", err)
	}
}
