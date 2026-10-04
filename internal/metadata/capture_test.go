package metadata

import (
	"testing"
	"time"
)

func TestResolveCaptureEmbeddedOffset(t *testing.T) {
	candidates := []CaptureCandidate{
		{Name: "DateTimeOriginal", Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05", Offset: "+09:30", OffsetPresent: true, OffsetOK: true},
	}
	capture, err := ResolveCapture(candidates, "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2024, 1, 1, 17, 34, 5, 0, time.UTC)
	assertCaptureTime(t, capture, want)
	if capture.Source != TakenAtEmbeddedOffset || capture.Timezone != nil {
		t.Fatalf("source/timezone = %q/%v", capture.Source, capture.Timezone)
	}
	if capture.Selected == nil || capture.Selected.Name != "DateTimeOriginal" {
		t.Fatalf("selected = %#v", capture.Selected)
	}
}

func TestResolveCaptureOffsetAbsentUsesDefaultTimezone(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Name: "DateTimeOriginal", DateTime: "2024:01:02 03:04:05",
	}}, "Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2024, 1, 1, 18, 4, 5, 0, time.UTC))
	if capture.Source != TakenAtDefaultTimezone || capture.Timezone == nil || *capture.Timezone != "Asia/Tokyo" {
		t.Fatalf("source/timezone = %q/%v", capture.Source, capture.Timezone)
	}
}

func TestResolveCaptureInvalidCandidateFallsBack(t *testing.T) {
	candidates := []CaptureCandidate{
		{Name: "DateTimeOriginal", Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05", Offset: "+99:00", OffsetPresent: true, OffsetOK: true},
		{Name: "DateTimeDigitized", Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05", Offset: "+00:00", OffsetPresent: true, OffsetOK: true},
	}
	capture, err := ResolveCapture(candidates, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if capture.Selected == nil || capture.Selected.Name != "DateTimeDigitized" {
		t.Fatalf("selected = %#v", capture.Selected)
	}
	if got := capture.Candidates[0].Discarded; got != discardInvalidOffset {
		t.Fatalf("first discard reason = %q", got)
	}
	if candidates[0].Discarded != "" {
		t.Fatal("ResolveCapture mutated its input")
	}
}

func TestResolveCaptureEXIFAbsent(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{Name: "DateTimeOriginal"}}, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if capture.TakenAt != nil || capture.Selected != nil || capture.Timezone != nil || capture.Source != TakenAtUnknown {
		t.Fatalf("unexpected capture: %#v", capture)
	}
	if got := capture.Candidates[0].Discarded; got != discardMissingDateTime {
		t.Fatalf("discard reason = %q", got)
	}
}

func TestResolveCapturePreservesPriorityAndOrder(t *testing.T) {
	candidates := []CaptureCandidate{
		{Name: "stream", Kind: CaptureCandidateVideo, DateTime: "2024-06-01T10:00:00Z"},
		{Name: "container", Kind: CaptureCandidateVideo, DateTime: "2020-01-01T10:00:00Z"},
	}
	capture, err := ResolveCapture(candidates, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC))
	if capture.Candidates[0].Name != "stream" || capture.Candidates[1].Name != "container" {
		t.Fatalf("candidate order changed: %#v", capture.Candidates)
	}
	if got := capture.Candidates[1].Discarded; got != discardLowerPriority {
		t.Fatalf("second discard reason = %q", got)
	}
}

func TestResolveCaptureDSTFoldChoosesEarlierUTC(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Name: "DateTimeOriginal", DateTime: "2023:11:05 01:30:00",
	}}, "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2023, 11, 5, 5, 30, 0, 0, time.UTC))
	if capture.FoldDecision != foldEarlierUTC {
		t.Fatalf("fold decision = %q", capture.FoldDecision)
	}
}

func TestResolveCaptureDSTGapSkipsCandidate(t *testing.T) {
	candidates := []CaptureCandidate{
		{Name: "DateTimeOriginal", DateTime: "2023:03:12 02:30:00"},
		{Name: "DateTimeDigitized", DateTime: "2023:03:12 03:30:00"},
	}
	capture, err := ResolveCapture(candidates, "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2023, 3, 12, 7, 30, 0, 0, time.UTC))
	if got := capture.Candidates[0].Discarded; got != discardDSTGap {
		t.Fatalf("gap discard reason = %q", got)
	}
}

func TestResolveCaptureLordHoweNonHourFold(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Kind: CaptureCandidateEXIF, DateTime: "2023:04:02 01:45:00",
	}}, "Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2023, 4, 1, 14, 45, 0, 0, time.UTC))
	if capture.FoldDecision != foldEarlierUTC {
		t.Fatalf("fold decision = %q", capture.FoldDecision)
	}
}

func TestResolveCaptureLordHoweNonHourGap(t *testing.T) {
	candidates := []CaptureCandidate{
		{Kind: CaptureCandidateEXIF, DateTime: "2023:10:01 02:15:00"},
		{Kind: CaptureCandidateEXIF, DateTime: "2023:10:01 02:30:00"},
	}
	capture, err := ResolveCapture(candidates, "Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2023, 9, 30, 15, 30, 0, 0, time.UTC))
	if got := capture.Candidates[0].Discarded; got != discardDSTGap {
		t.Fatalf("gap discard reason = %q", got)
	}
}

func TestResolveCaptureApiaSkippedDay(t *testing.T) {
	candidates := []CaptureCandidate{
		{Kind: CaptureCandidateEXIF, DateTime: "2011:12:30 12:00:00"},
		{Kind: CaptureCandidateEXIF, DateTime: "2011:12:31 12:00:00"},
	}
	capture, err := ResolveCapture(candidates, "Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2011, 12, 30, 22, 0, 0, 0, time.UTC))
	if got := capture.Candidates[0].Discarded; got != discardDSTGap {
		t.Fatalf("skipped-day discard reason = %q", got)
	}
}

func TestResolveCaptureTimezoneConfigurationIsNotRetroactive(t *testing.T) {
	candidate := []CaptureCandidate{{Name: "DateTimeOriginal", DateTime: "2024:01:02 03:04:05"}}
	tokyo, err := ResolveCapture(candidate, "Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	utc, err := ResolveCapture(candidate, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, tokyo, time.Date(2024, 1, 1, 18, 4, 5, 0, time.UTC))
	assertCaptureTime(t, utc, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if tokyo.Timezone == nil || *tokyo.Timezone != "Asia/Tokyo" {
		t.Fatalf("original timezone changed: %v", tokyo.Timezone)
	}
}

func TestResolveCaptureExplicitOffsetIgnoresInvalidTimezone(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Kind: CaptureCandidateVideo, DateTime: "2024-01-02T03:04:05Z",
	}}, "Not/A_Real_Zone")
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureTime(t, capture, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
}

func TestResolveCaptureOffsetlessRejectsInvalidTimezone(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05",
	}}, "Not/A_Real_Zone")
	if err == nil {
		t.Fatalf("expected configuration error, got capture %#v", capture)
	}
}

func TestResolveCaptureFractionalVideoTimestamp(t *testing.T) {
	tests := []struct {
		name     string
		dateTime string
		want     time.Time
	}{
		{
			name:     "colon offset",
			dateTime: "2024-07-08T09:10:11.123456789+02:30",
			want:     time.Date(2024, 7, 8, 6, 40, 11, 123456789, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, err := ResolveCapture([]CaptureCandidate{{Name: "stream", Kind: CaptureCandidateVideo, DateTime: tt.dateTime}}, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			assertCaptureTime(t, capture, tt.want)
			if capture.Source != TakenAtEmbeddedOffset {
				t.Fatalf("source = %q", capture.Source)
			}
		})
	}
}

func TestResolveCaptureCandidateSpecificGrammar(t *testing.T) {
	tests := []struct {
		name      string
		candidate CaptureCandidate
		valid     bool
	}{
		{name: "EXIF grammar", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:07:08 09:10:11"}, valid: true},
		{name: "EXIF rejects video", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024-07-08T09:10:11Z"}},
		{name: "video grammar", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024-07-08T09:10:11+02:30"}, valid: true},
		{name: "video rejects EXIF", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024:07:08 09:10:11"}},
		{name: "offsetless video uses snapshot timezone", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024-07-08T09:10:11"}, valid: true},
		{name: "video rejects compact offset", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024-07-08T09:10:11-0430"}},
		{name: "video rejects unknown offset", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024-07-08T09:10:11-00:00"}},
		{name: "leap second is rejected", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2016-12-31T23:59:60Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, err := ResolveCapture([]CaptureCandidate{tt.candidate}, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if got := capture.TakenAt != nil; got != tt.valid {
				t.Fatalf("resolved = %v, want %v; candidate = %#v", got, tt.valid, capture.Candidates[0])
			}
		})
	}
}

func TestResolveCaptureGregorianAndOffsetEdges(t *testing.T) {
	tests := []struct {
		name      string
		candidate CaptureCandidate
		valid     bool
	}{
		{name: "Gregorian leap century", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2000:02:29 23:59:59"}, valid: true},
		{name: "Gregorian non-leap century", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "1900:02:29 12:00:00"}},
		{name: "invalid month", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:13:01 00:00:00"}},
		{name: "EXIF maximum offset", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:01:01 00:00:00", Offset: "+23:59", OffsetPresent: true, OffsetOK: true}, valid: true},
		{name: "EXIF offset hour overflow", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:01:01 00:00:00", Offset: "+24:00", OffsetPresent: true, OffsetOK: true}},
		{name: "EXIF offset minute overflow", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:01:01 00:00:00", Offset: "+01:60", OffsetPresent: true, OffsetOK: true}},
		{name: "EXIF compact offset", candidate: CaptureCandidate{Kind: CaptureCandidateEXIF, DateTime: "2024:01:01 00:00:00", Offset: "+0130", OffsetPresent: true, OffsetOK: true}},
		{name: "video maximum offset", candidate: CaptureCandidate{Kind: CaptureCandidateVideo, DateTime: "2024-01-01T00:00:00-23:59"}, valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, err := ResolveCapture([]CaptureCandidate{tt.candidate}, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			if got := capture.TakenAt != nil; got != tt.valid {
				t.Fatalf("resolved = %v, want %v; candidate = %#v", got, tt.valid, capture.Candidates[0])
			}
		})
	}
}

func TestResolveCapturePresentMalformedOffsetInvalidatesCandidate(t *testing.T) {
	candidates := []CaptureCandidate{
		{Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05", OffsetPresent: true},
		{Kind: CaptureCandidateEXIF, DateTime: "2024:01:02 03:04:05"},
	}
	capture, err := ResolveCapture(candidates, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if got := capture.Candidates[0].Discarded; got != discardInvalidOffset {
		t.Fatalf("discard reason = %q", got)
	}
	if capture.SelectedIndex == nil || *capture.SelectedIndex != 1 {
		t.Fatalf("selected index = %v", capture.SelectedIndex)
	}
}

func TestResolveCaptureSelectedIsStableCopy(t *testing.T) {
	capture, err := ResolveCapture([]CaptureCandidate{{
		Kind: CaptureCandidateVideo, Name: "stream", DateTime: "2024-01-02T03:04:05Z",
	}}, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if capture.Selected == nil || capture.SelectedIndex == nil || *capture.SelectedIndex != 0 {
		t.Fatalf("selection = %#v at %v", capture.Selected, capture.SelectedIndex)
	}
	capture.Candidates[0].Name = "mutated"
	if capture.Selected.Name != "stream" {
		t.Fatalf("selected aliases candidates: %#v", capture.Selected)
	}
}

func assertCaptureTime(t *testing.T, capture Capture, want time.Time) {
	t.Helper()
	if capture.TakenAt == nil {
		t.Fatal("TakenAt is nil")
	}
	if !capture.TakenAt.Equal(want) {
		t.Fatalf("TakenAt = %s, want %s", capture.TakenAt.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}
