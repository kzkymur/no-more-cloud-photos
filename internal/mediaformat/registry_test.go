package mediaformat

import "testing"

func TestRegistryIsClosedUniqueAndSorted(t *testing.T) {
	formats := Formats()
	if len(formats) != 17 {
		t.Fatalf("Formats() length = %d, want 17", len(formats))
	}
	seenMIME := make(map[string]struct{}, len(formats))
	seenExtension := make(map[string]struct{}, len(formats))
	for index, format := range formats {
		if index > 0 && formats[index-1].MIMEType >= format.MIMEType {
			t.Fatalf("formats are not MIME-sorted at %q", format.MIMEType)
		}
		if _, exists := seenMIME[format.MIMEType]; exists {
			t.Fatalf("duplicate MIME %q", format.MIMEType)
		}
		if _, exists := seenExtension[format.Extension]; exists {
			t.Fatalf("duplicate extension %q", format.Extension)
		}
		seenMIME[format.MIMEType] = struct{}{}
		seenExtension[format.Extension] = struct{}{}
		if got, ok := Lookup(format.MIMEType); !ok || got != format {
			t.Fatalf("Lookup(%q) = %#v, %v", format.MIMEType, got, ok)
		}
	}
	if _, ok := Lookup("image/tiff"); ok {
		t.Fatal("generic TIFF unexpectedly registered")
	}
}
