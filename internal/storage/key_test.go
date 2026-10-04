package storage

import (
	"errors"
	"strings"
	"testing"
)

const (
	testOriginalID  = "01234567-89ab-4cde-8f01-23456789abcd"
	testTargetID    = "11111111-2222-4333-8444-555555555555"
	testRenditionID = "22222222-3333-4444-8555-666666666666"
	testAttemptID   = "33333333-4444-4555-8666-777777777777"
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
