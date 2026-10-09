package reconciliation

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestCheckRecordsStorageErrorAndNeverSealsIncompleteScan(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff}
	checker, err := NewChecker(repository, failingScanner{err: context.Canceled})
	if err != nil {
		t.Fatal(err)
	}
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if !errors.Is(err, context.Canceled) || result.ReportID == "" {
		t.Fatalf("Check() = %+v, %v", result, err)
	}
	if repository.sealed {
		t.Fatal("incomplete storage scan was sealed")
	}
	if got := repository.sources["storage_scan"]; got == nil || *got != "storage_scan" {
		t.Fatalf("storage source outcome = %v", got)
	}
	_, databaseRecorded := repository.sources["database_references"]
	_, attemptsRecorded := repository.sources["attempt_owners"]
	if !databaseRecorded || !attemptsRecorded || repository.sources["database_references"] != nil || repository.sources["attempt_owners"] != nil {
		t.Fatalf("database source outcomes = %+v", repository.sources)
	}
}

func TestCheckRecordsDatabaseSnapshotErrorsWithoutScanningOrSealing(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{
		snapshot:    Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff},
		snapshotErr: errors.New("database unavailable"), now: cutoff,
	}
	scan := &countingScanner{}
	checker, _ := NewChecker(repository, scan)
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := checker.Check(ctx, ScopeAll)
	if err == nil || result.ReportID == "" || scan.calls != 0 || repository.sealed {
		t.Fatalf("Check() = %+v, %v; scan calls=%d sealed=%v", result, err, scan.calls, repository.sealed)
	}
	for _, source := range []string{"database_references", "attempt_owners"} {
		if got := repository.sources[source]; got == nil || *got != "database_snapshot" {
			t.Fatalf("%s source outcome = %v", source, got)
		}
	}
	if got := repository.sources["storage_scan"]; got == nil || *got != "database_snapshot" {
		t.Fatalf("storage source outcome = %v", got)
	}
}

func TestCheckResolvesSuccessfulFinalizationCommitReplyLoss(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{
		snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff,
		finalizeErr: errors.New("commit response lost"),
	}
	checker, _ := NewChecker(repository, &countingScanner{})
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 0 {
		t.Fatalf("Check() = %+v, %v", result, err)
	}
}

func TestClassifyDeterministicReferencesOrphansAndAttemptOwnership(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-storage.AttemptTempGrace)
	originalID := "01234567-89ab-4cde-8f01-23456789abcd"
	targetID := "12345678-9abc-4def-8012-3456789abcde"
	renditionID := "23456789-abcd-4ef0-8123-456789abcdef"
	attemptID := "3456789a-bcde-4f01-8234-56789abcdef0"
	jobID := "456789ab-cdef-4012-8345-6789abcdef01"
	mediaID := "56789abc-def0-4123-8456-789abcdef012"
	originalPath := "originals/01/" + originalID + "/original.jpg"
	renditionPath := "renditions/01/" + originalID + "/" + targetID + "/" + renditionID + ".webp"
	tempPath := "renditions/01/" + originalID + "/" + targetID + "/." + renditionID + ".webp." + attemptID + ".tmp"

	goodBytes := []byte("good")
	goodDigest := sha256.Sum256(goodBytes)
	badDigest := sha256.Sum256([]byte("bad"))
	goodSHA, badSHA := encodeDigest(goodDigest), encodeDigest(badDigest)
	goodSize := int64(len(goodBytes))
	snapshot := Snapshot{
		CutoffAt: cutoff,
		References: []Reference{
			{Kind: "original", SubjectID: originalID, MediaID: mediaID, RelativeKey: &originalPath, ExpectedState: stringPointer("referenced"), SizeBytes: &goodSize, SHA256: &goodSHA},
			{Kind: "transform_target", SubjectID: targetID, MediaID: mediaID, OriginalID: &originalID, JobID: &jobID, TargetID: &targetID},
		},
		Attempts: []AttemptOwner{{AttemptID: attemptID, Kind: "transform", Coverage: "native", OriginalID: originalID, JobID: &jobID, EventType: "released", EventOccurredAt: &old}},
	}
	observations := []storage.Observation{
		regularObservation(originalPath, goodSize, badDigest, old),
		regularObservation(renditionPath, goodSize, goodDigest, old),
		regularObservation(tempPath, goodSize, goodDigest, old),
	}
	findings, err := classify(snapshot, observations, cutoff, ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 3 {
		t.Fatalf("finding count = %d: %+v", len(findings), findings)
	}
	gotKinds := []string{findings[0].Kind, findings[1].Kind, findings[2].Kind}
	wantKinds := []string{"sha256_mismatch", "aged_attempt_temp", "final_orphan"}
	for i := range wantKinds {
		if gotKinds[i] != wantKinds[i] {
			t.Fatalf("finding kinds = %v, want %v", gotKinds, wantKinds)
		}
	}
	if findings[1].Reason != "terminal_owner" || findings[1].MediaID == nil || findings[1].JobID == nil || findings[1].TargetID == nil {
		t.Fatalf("attempt finding = %+v", findings[1])
	}
	if findings[0].ObservedSHA == nil || *findings[0].ObservedSHA != badSHA {
		t.Fatalf("SHA mismatch evidence = %+v", findings[0])
	}
}

func TestClassifyAmbiguousAttemptsRemainNonAuthoritative(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-storage.AttemptTempGrace)
	originalID := "01234567-89ab-4cde-8f01-23456789abcd"
	attemptID := "3456789a-bcde-4f01-8234-56789abcdef0"
	tempPath := "originals/01/" + originalID + "/.original." + attemptID + ".tmp"
	digest := sha256.Sum256([]byte("temp"))
	size := int64(4)
	for _, test := range []struct {
		name  string
		owner AttemptOwner
		want  string
	}{
		{name: "missing", want: "missing_owner"},
		{name: "legacy", owner: AttemptOwner{AttemptID: attemptID, Kind: "upload", Coverage: "legacy_active", OriginalID: originalID}, want: "legacy_owner"},
		{name: "reclaimed", owner: AttemptOwner{AttemptID: attemptID, Kind: "upload", Coverage: "native", OriginalID: originalID, TempRelativeKey: &tempPath, EventType: "reclaimed"}, want: "reclaimed_owner"},
		{name: "expired without terminal", owner: AttemptOwner{AttemptID: attemptID, Kind: "upload", Coverage: "native", OriginalID: originalID, TempRelativeKey: &tempPath, EventType: "heartbeat", LeaseExpiresAt: &old}, want: "expired_without_terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Snapshot{CutoffAt: cutoff}
			if test.owner.AttemptID != "" {
				snapshot.Attempts = []AttemptOwner{test.owner}
			}
			findings, err := classify(snapshot, []storage.Observation{regularObservation(tempPath, size, digest, old)}, cutoff, ScopeAll)
			if err != nil || len(findings) != 1 || findings[0].Kind != "aged_attempt_temp" || findings[0].Reason != test.want {
				t.Fatalf("classify() = %+v, %v; want reason %s", findings, err, test.want)
			}
		})
	}
}

func TestClassifyScopeFiltersFindingsButNotSourceCapture(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	mediaID := "56789abc-def0-4123-8456-789abcdef012"
	snapshot := Snapshot{CutoffAt: cutoff, References: []Reference{{Kind: "expired_media", SubjectID: mediaID, MediaID: mediaID}}}
	observation := storage.Observation{RelativeKey: "unexpected", Type: storage.ObservationRegular, Stable: true}
	for _, test := range []struct {
		scope Scope
		want  string
	}{
		{scope: ScopeFiles, want: "unexpected_path"},
		{scope: ScopeDB, want: "expired_media_candidate"},
	} {
		findings, err := classify(snapshot, []storage.Observation{observation}, cutoff, test.scope)
		if err != nil || len(findings) != 1 || findings[0].Kind != test.want {
			t.Fatalf("classify(scope=%s) = %+v, %v", test.scope, findings, err)
		}
	}
	findings, err := classify(snapshot, []storage.Observation{observation}, cutoff, ScopeAll)
	if err != nil || len(findings) != 2 {
		t.Fatalf("classify(scope=all) = %+v, %v", findings, err)
	}
}

func regularObservation(path string, size int64, digest [sha256.Size]byte, timestamp time.Time) storage.Observation {
	return storage.Observation{RelativeKey: path, Type: storage.ObservationRegular, Size: &size, SHA256: &digest, ModifiedAt: &timestamp, ChangedAt: &timestamp, Stable: true}
}

func encodeDigest(digest [sha256.Size]byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, sha256.Size*2)
	for i, value := range digest {
		result[i*2], result[i*2+1] = alphabet[value>>4], alphabet[value&15]
	}
	return string(result)
}

type recordingRepository struct {
	snapshot    Snapshot
	snapshotErr error
	now         time.Time
	sources     map[string]*string
	sealed      bool
	sealedCount int64
	sealedEnd   time.Time
	finalizeErr error
}

func (repository *recordingRepository) CaptureSnapshot(context.Context) (Snapshot, error) {
	return repository.snapshot, repository.snapshotErr
}
func (repository *recordingRepository) DatabaseNow(context.Context) (time.Time, error) {
	return repository.now, nil
}
func (repository *recordingRepository) BeginReport(context.Context, string, Scope, Snapshot, time.Time) error {
	repository.sources = make(map[string]*string)
	return nil
}
func (repository *recordingRepository) CompleteSource(_ context.Context, _, source string, code *string) error {
	if code == nil {
		repository.sources[source] = nil
		return nil
	}
	copy := *code
	repository.sources[source] = &copy
	return nil
}
func (repository *recordingRepository) ReportOutcome(context.Context, string) (ReportOutcome, error) {
	outcome := ReportOutcome{Sealed: repository.sealed}
	if repository.sealed {
		outcome.SealedFindingCount = &repository.sealedCount
		outcome.ScanEndedAt = &repository.sealedEnd
	}
	set := func(source string, result, errorCode **string) {
		code, ok := repository.sources[source]
		if !ok {
			return
		}
		if code == nil {
			*result = stringPointer("complete")
			return
		}
		*result = stringPointer("error")
		*errorCode = code
	}
	set("database_references", &outcome.DatabaseResult, &outcome.DatabaseError)
	set("attempt_owners", &outcome.AttemptResult, &outcome.AttemptError)
	set("storage_scan", &outcome.StorageResult, &outcome.StorageError)
	return outcome, nil
}
func (*recordingRepository) AppendFinding(context.Context, string, Finding) (int64, error) {
	return 1, nil
}
func (repository *recordingRepository) FinalizeReport(_ context.Context, _ string, count int64, ended time.Time) error {
	repository.sources["storage_scan"] = nil
	repository.sealed = true
	repository.sealedCount = count
	repository.sealedEnd = ended
	return repository.finalizeErr
}

type failingScanner struct{ err error }

func (scanner failingScanner) Scan(context.Context) ([]storage.Observation, error) {
	return nil, scanner.err
}

type countingScanner struct{ calls int }

func (scanner *countingScanner) Scan(context.Context) ([]storage.Observation, error) {
	scanner.calls++
	return nil, nil
}
