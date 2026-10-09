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

func TestCheckPreservesIndependentDatabaseSourceOutcomes(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff, attemptErr: errors.New("attempt reader failed")}
	scan := &countingScanner{}
	checker, _ := NewChecker(repository, scan)
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err == nil || result.ReportID == "" || scan.calls != 0 || repository.sealed {
		t.Fatalf("Check() = %+v, %v; scans=%d sealed=%v", result, err, scan.calls, repository.sealed)
	}
	if code, ok := repository.sources["database_references"]; !ok || code != nil {
		t.Fatalf("database outcome = %v, %v", code, ok)
	}
	if code := repository.sources["attempt_owners"]; code == nil || *code != "attempt_owners" {
		t.Fatalf("attempt outcome = %v", code)
	}
	if code := repository.sources["storage_scan"]; code == nil || *code != "database_source_failed" {
		t.Fatalf("storage outcome = %v", code)
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

func TestCheckRetriesIdempotentBeginAfterReplyLoss(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff, beginReplyLoss: true}
	checker, _ := NewChecker(repository, &countingScanner{})
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err != nil || !result.Sealed || repository.beginCalls != 2 {
		t.Fatalf("Check() = %+v, %v; begin calls=%d", result, err, repository.beginCalls)
	}
}

func TestCheckRetriesIdempotentFindingAfterReplyLoss(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff, appendReplyLoss: true}
	checker, _ := NewChecker(repository, staticObservationScanner{observations: []storage.Observation{{RelativeKey: "unexpected", Type: storage.ObservationRegular, Stable: true}}})
	ids := []string{"01234567-89ab-4cde-8f01-23456789abcd", "12345678-9abc-4def-8012-3456789abcde"}
	checker.newID = func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 1 || repository.appendCalls != 2 {
		t.Fatalf("Check() = %+v, %v; append calls=%d", result, err, repository.appendCalls)
	}
}

func TestCheckReplaysSourceCompletionAfterPrecommitAndOutcomeFailures(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff, completeFailures: 1, outcomeFailures: 1}
	checker, _ := NewChecker(repository, &countingScanner{})
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err != nil || !result.Sealed || repository.completeCalls != 3 {
		t.Fatalf("Check() = %+v, %v; completion calls=%d", result, err, repository.completeCalls)
	}
}

func TestCheckConfirmsSourceCompletionReplyLossWithoutDuplicate(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repository := &recordingRepository{snapshot: Snapshot{StartedAt: cutoff, CutoffAt: cutoff, EndedAt: cutoff}, now: cutoff, completeReplyLosses: 1}
	checker, _ := NewChecker(repository, &countingScanner{})
	checker.newID = func() (string, error) { return "01234567-89ab-4cde-8f01-23456789abcd", nil }
	result, err := checker.Check(context.Background(), ScopeAll)
	if err != nil || !result.Sealed || repository.completeCalls != 2 {
		t.Fatalf("Check() = %+v, %v; completion calls=%d", result, err, repository.completeCalls)
	}
}

func TestSourceCompletionUsesIndependentConfirmationAndReplayBudgets(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository *recordingRepository
		wantCalls  int
	}{
		{name: "write budget exhausted after commit", repository: &recordingRepository{sources: make(map[string]*string), completeExhaustAfterCommit: 1}, wantCalls: 1},
		{name: "confirmation budget exhausted then exact replay", repository: &recordingRepository{sources: make(map[string]*string), completeReplyLosses: 1, blockOutcomeAttempts: 1}, wantCalls: 2},
		{name: "replay budget exhausted after commit then final confirmation", repository: &recordingRepository{sources: make(map[string]*string), completeFailures: 1, blockOutcomeAttempts: 1, completeExhaustAfterCommit: 1}, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker, _ := NewChecker(test.repository, &countingScanner{})
			checker.drainBudget = 10 * time.Millisecond
			if err := checker.completeSourceDetached(context.Background(), "01234567-89ab-4cde-8f01-23456789abcd", "database_references", nil); err != nil {
				t.Fatal(err)
			}
			code, recorded := test.repository.sources["database_references"]
			if test.repository.completeCalls != test.wantCalls || !recorded || code != nil {
				t.Fatalf("completion calls/sources = %d/%+v", test.repository.completeCalls, test.repository.sources)
			}
		})
	}
}

func TestMissingSourceRecoveryUsesIndependentBudgets(t *testing.T) {
	repository := &recordingRepository{sources: make(map[string]*string), blockSource: "database_references"}
	checker, _ := NewChecker(repository, &countingScanner{})
	checker.drainBudget = 10 * time.Millisecond
	err := checker.recordMissingSourceErrors(context.Background(), "01234567-89ab-4cde-8f01-23456789abcd", "check_failed")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recordMissingSourceErrors() error = %v", err)
	}
	if repository.sources["attempt_owners"] == nil || repository.sources["storage_scan"] == nil {
		t.Fatalf("later source recovery was starved: %+v", repository.sources)
	}
}

func TestMissingSourceRecoveryDoesNotDependOnOutcomeRead(t *testing.T) {
	repository := &recordingRepository{sources: make(map[string]*string), outcomeFailures: 1}
	checker, _ := NewChecker(repository, &countingScanner{})
	err := checker.recordMissingSourceErrors(context.Background(), "01234567-89ab-4cde-8f01-23456789abcd", "check_failed")
	if err == nil {
		t.Fatal("recordMissingSourceErrors() unexpectedly hid the outcome read failure")
	}
	for _, source := range []string{"database_references", "attempt_owners", "storage_scan"} {
		if repository.sources[source] == nil || *repository.sources[source] != "check_failed" {
			t.Fatalf("%s recovery = %+v", source, repository.sources)
		}
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

func TestClassifyStableHardlinkAsLocalFinding(t *testing.T) {
	observedAt := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	observation := storage.Observation{RelativeKey: "unexpected", Type: storage.ObservationRegular, Stable: true, LinkCount: 2}
	findings, err := classify(Snapshot{CutoffAt: observedAt}, []storage.Observation{observation}, observedAt, ScopeAll)
	if err != nil || len(findings) != 1 || findings[0].Kind != "unexpected_hardlink" || findings[0].ObservedSHA != nil {
		t.Fatalf("classify() = %+v, %v", findings, err)
	}
}

func TestClassifyQuarantineOnlyAgainstDurableJournal(t *testing.T) {
	observedAt := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	good := sha256.Sum256([]byte("good"))
	bad := sha256.Sum256([]byte("bad"))
	size := int64(4)
	goodSHA := encodeDigest(good)
	missing := ".quarantine/01234567-89ab-4cde-8f01-23456789abcd"
	mismatch := ".quarantine/12345678-9abc-4def-8012-3456789abcde"
	uncertain := ".quarantine/23456789-abcd-4ef0-8123-456789abcdef"
	snapshot := Snapshot{References: []Reference{
		{Kind: "quarantine", SubjectID: "01234567-89ab-4cde-8f01-23456789abcd", RelativeKey: &missing, ExpectedState: stringPointer("quarantined"), SizeBytes: &size, SHA256: &goodSHA},
		{Kind: "quarantine", SubjectID: "12345678-9abc-4def-8012-3456789abcde", RelativeKey: &mismatch, ExpectedState: stringPointer("quarantined"), SizeBytes: &size, SHA256: &goodSHA},
		{Kind: "quarantine", SubjectID: "23456789-abcd-4ef0-8123-456789abcdef", RelativeKey: &uncertain, SizeBytes: &size, SHA256: &goodSHA},
	}}
	observations := []storage.Observation{
		regularObservation(mismatch, size, bad, observedAt),
		regularObservation(".quarantine/3456789a-bcde-4f01-8234-56789abcdef0", size, good, observedAt),
	}
	findings, err := classify(snapshot, observations, observedAt, ScopeAll)
	if err != nil || len(findings) != 2 || findings[0].Kind != "quarantine_missing" || findings[1].Kind != "quarantine_mismatch" {
		t.Fatalf("classify() = %+v, %v", findings, err)
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
	snapshot                   Snapshot
	snapshotErr                error
	now                        time.Time
	sources                    map[string]*string
	sealed                     bool
	sealedCount                int64
	sealedEnd                  time.Time
	finalizeErr                error
	databaseErr                error
	attemptErr                 error
	blockSource                string
	beginReplyLoss             bool
	beginCalls                 int
	appendReplyLoss            bool
	appendCalls                int
	appended                   map[string]int64
	outcomeFailures            int
	blockOutcomeAttempts       int
	completeCalls              int
	completeFailures           int
	completeReplyLosses        int
	completeExhaustAfterCommit int
}

func (repository *recordingRepository) CaptureSnapshot(context.Context) (SnapshotCapture, error) {
	return SnapshotCapture{Snapshot: repository.snapshot, DatabaseReferencesError: repository.databaseErr, AttemptOwnersError: repository.attemptErr}, repository.snapshotErr
}
func (repository *recordingRepository) DatabaseNow(context.Context) (time.Time, error) {
	return repository.now, nil
}
func (repository *recordingRepository) BeginReport(context.Context, string, Scope, Snapshot, time.Time) error {
	repository.beginCalls++
	if repository.sources == nil {
		repository.sources = make(map[string]*string)
	}
	if repository.beginReplyLoss && repository.beginCalls == 1 {
		return errors.New("begin reply lost")
	}
	return nil
}
func (repository *recordingRepository) CompleteSource(ctx context.Context, _, source string, code *string) error {
	repository.completeCalls++
	if source == repository.blockSource {
		<-ctx.Done()
		return ctx.Err()
	}
	if repository.completeFailures > 0 {
		repository.completeFailures--
		return errors.New("source completion failed before commit")
	}
	if repository.sources == nil {
		repository.sources = make(map[string]*string)
	}
	if code == nil {
		repository.sources[source] = nil
	} else {
		copy := *code
		repository.sources[source] = &copy
	}
	if repository.completeExhaustAfterCommit > 0 {
		repository.completeExhaustAfterCommit--
		<-ctx.Done()
		return ctx.Err()
	}
	if repository.completeReplyLosses > 0 {
		repository.completeReplyLosses--
		return errors.New("source completion reply lost")
	}
	return nil
}
func (repository *recordingRepository) ReportOutcome(ctx context.Context, _ string) (ReportOutcome, error) {
	if repository.blockOutcomeAttempts > 0 {
		repository.blockOutcomeAttempts--
		<-ctx.Done()
		return ReportOutcome{}, ctx.Err()
	}
	if repository.outcomeFailures > 0 {
		repository.outcomeFailures--
		return ReportOutcome{}, errors.New("outcome read failed")
	}
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
func (repository *recordingRepository) AppendFinding(_ context.Context, _ string, finding Finding) (int64, error) {
	repository.appendCalls++
	if repository.appended == nil {
		repository.appended = make(map[string]int64)
	}
	if ordinal, ok := repository.appended[finding.ID]; ok {
		return ordinal, nil
	}
	ordinal := int64(len(repository.appended) + 1)
	repository.appended[finding.ID] = ordinal
	if repository.appendReplyLoss {
		repository.appendReplyLoss = false
		return 0, errors.New("append reply lost")
	}
	return ordinal, nil
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

type staticObservationScanner struct{ observations []storage.Observation }

func (scanner staticObservationScanner) Scan(context.Context) ([]storage.Observation, error) {
	return scanner.observations, nil
}
