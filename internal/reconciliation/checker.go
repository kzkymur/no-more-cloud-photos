package reconciliation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const evidenceDrainBudget = 5 * time.Second

type repository interface {
	CaptureSnapshot(context.Context) (SnapshotCapture, error)
	DatabaseNow(context.Context) (time.Time, error)
	BeginReport(context.Context, string, Scope, Snapshot, time.Time) error
	CompleteSource(context.Context, string, string, *string) error
	ReportOutcome(context.Context, string) (ReportOutcome, error)
	AppendFinding(context.Context, string, Finding) (int64, error)
	FinalizeReport(context.Context, string, int64, time.Time) error
}

type scanner interface {
	Scan(context.Context) ([]storage.Observation, error)
}

type Checker struct {
	repository repository
	scanner    scanner
	newID      func() (string, error)
}

func NewChecker(repository repository, scanner scanner) (*Checker, error) {
	if repository == nil || scanner == nil {
		return nil, errors.New("reconciliation repository and scanner are required")
	}
	return &Checker{repository: repository, scanner: scanner, newID: newUUIDv4}, nil
}

// Check executes a read-only database/filesystem comparison and publishes an
// immutable report. It never enters maintenance and never mutates storage.
func (checker *Checker) Check(ctx context.Context, scope Scope) (returned Result, returnedErr error) {
	if ctx == nil || scope != ScopeFiles && scope != ScopeDB && scope != ScopeAll {
		return Result{}, errors.New("invalid reconciliation check request")
	}
	reportID, err := checker.newID()
	if err != nil {
		return Result{}, fmt.Errorf("generate reconciliation report ID: %w", err)
	}
	capture, snapshotErr := checker.repository.CaptureSnapshot(ctx)
	snapshot := capture.Snapshot
	if snapshot.StartedAt.IsZero() || snapshot.CutoffAt.IsZero() {
		if snapshotErr == nil {
			snapshotErr = errors.New("reconciliation snapshot omitted its chronology")
		}
		return Result{}, snapshotErr
	}
	if snapshot.EndedAt.IsZero() {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evidenceDrainBudget)
		snapshot.EndedAt, err = checker.repository.DatabaseNow(drainCtx)
		cancel()
		if err != nil {
			return Result{}, errors.Join(snapshotErr, err)
		}
	}
	setupCtx := ctx
	var setupCancel context.CancelFunc
	if snapshotErr != nil {
		setupCtx, setupCancel = context.WithTimeout(context.WithoutCancel(ctx), evidenceDrainBudget)
		defer setupCancel()
	}
	scanStarted, err := checker.repository.DatabaseNow(setupCtx)
	if err != nil {
		return Result{}, err
	}
	if scanStarted.Before(snapshot.EndedAt) {
		return Result{}, errors.New("reconciliation database clock regressed before storage scan")
	}
	if err := checker.beginReport(setupCtx, reportID, scope, snapshot, scanStarted); err != nil {
		return Result{}, err
	}
	result := Result{ReportID: reportID, StartedAt: scanStarted}
	failureCode := "check_failed"
	finished := false
	defer func() {
		if finished {
			return
		}
		drainErr := checker.recordMissingSourceErrors(ctx, reportID, failureCode)
		returnedErr = errors.Join(returnedErr, drainErr)
	}()
	if snapshotErr != nil {
		failureCode = "database_snapshot"
		return result, snapshotErr
	}
	var databaseCode, attemptCode *string
	if capture.DatabaseReferencesError != nil {
		databaseCode = stringPointer("database_references")
	}
	if capture.AttemptOwnersError != nil {
		attemptCode = stringPointer("attempt_owners")
	}
	if err := checker.completeSource(ctx, reportID, "database_references", databaseCode); err != nil {
		return result, err
	}
	if err := checker.completeSource(ctx, reportID, "attempt_owners", attemptCode); err != nil {
		return result, err
	}
	if capture.DatabaseReferencesError != nil || capture.AttemptOwnersError != nil {
		failureCode = "database_source_failed"
		return result, errors.Join(capture.DatabaseReferencesError, capture.AttemptOwnersError)
	}

	observations, scanErr := checker.scanner.Scan(ctx)
	if scanErr != nil {
		failureCode = "storage_scan"
		return result, scanErr
	}
	scanEnded, err := checker.repository.DatabaseNow(ctx)
	if err != nil {
		failureCode = "database_clock"
		return result, err
	}
	if scanEnded.Before(scanStarted) {
		failureCode = "clock_regression"
		return result, errors.New("reconciliation database clock regressed during storage scan")
	}
	result.EndedAt = scanEnded
	findings, err := classify(snapshot, observations, scanStarted, scope)
	if err != nil {
		failureCode = "classification"
		return result, err
	}
	for index := range findings {
		findings[index].ID, err = checker.newID()
		if err != nil {
			failureCode = "finding_id"
			return result, err
		}
		ordinal, appendErr := checker.appendFinding(ctx, reportID, findings[index])
		if appendErr != nil {
			failureCode = "finding_persist"
			return result, appendErr
		}
		if ordinal != int64(index+1) {
			failureCode = "ordinal_drift"
			return result, errors.New("reconciliation finding ordinal drift")
		}
	}
	if err := checker.repository.FinalizeReport(ctx, reportID, int64(len(findings)), scanEnded); err != nil {
		failureCode = "finalize"
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evidenceDrainBudget)
		outcome, outcomeErr := checker.repository.ReportOutcome(drainCtx, reportID)
		cancel()
		if outcomeErr != nil || !exactSeal(outcome, int64(len(findings)), scanEnded) {
			return result, errors.Join(err, outcomeErr)
		}
	}
	result.Sealed = true
	result.FindingCount = int64(len(findings))
	finished = true
	return result, nil
}

func (checker *Checker) appendFinding(parent context.Context, reportID string, finding Finding) (int64, error) {
	ordinal, err := checker.repository.AppendFinding(parent, reportID, finding)
	if err == nil {
		return ordinal, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), evidenceDrainBudget)
	defer cancel()
	retryOrdinal, retryErr := checker.repository.AppendFinding(ctx, reportID, finding)
	if retryErr != nil {
		return 0, errors.Join(err, retryErr)
	}
	return retryOrdinal, nil
}

func (checker *Checker) beginReport(parent context.Context, reportID string, scope Scope, snapshot Snapshot, scanStarted time.Time) error {
	err := checker.repository.BeginReport(parent, reportID, scope, snapshot, scanStarted)
	if err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), evidenceDrainBudget)
	defer cancel()
	if retryErr := checker.repository.BeginReport(ctx, reportID, scope, snapshot, scanStarted); retryErr != nil {
		return errors.Join(err, retryErr)
	}
	return nil
}

func (checker *Checker) completeSource(parent context.Context, reportID, source string, code *string) error {
	err := checker.repository.CompleteSource(parent, reportID, source, code)
	if err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), evidenceDrainBudget)
	defer cancel()
	outcome, outcomeErr := checker.repository.ReportOutcome(ctx, reportID)
	if outcomeErr == nil && sourceMatches(outcome, source, code) {
		return nil
	}
	return errors.Join(err, outcomeErr)
}

func (checker *Checker) recordMissingSourceErrors(parent context.Context, reportID, code string) error {
	outcome, err := checker.readOutcomeDetached(parent, reportID)
	if err != nil {
		return err
	}
	var result error
	if outcome.DatabaseResult == nil {
		result = errors.Join(result, checker.completeSourceDetached(parent, reportID, "database_references", &code))
	}
	if outcome.AttemptResult == nil {
		result = errors.Join(result, checker.completeSourceDetached(parent, reportID, "attempt_owners", &code))
	}
	if outcome.StorageResult == nil {
		result = errors.Join(result, checker.completeSourceDetached(parent, reportID, "storage_scan", &code))
	}
	return result
}

func (checker *Checker) readOutcomeDetached(parent context.Context, reportID string) (ReportOutcome, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), evidenceDrainBudget)
	defer cancel()
	return checker.repository.ReportOutcome(ctx, reportID)
}

func (checker *Checker) completeSourceDetached(parent context.Context, reportID, source string, code *string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), evidenceDrainBudget)
	defer cancel()
	return checker.completeSource(ctx, reportID, source, code)
}

func sourceMatches(outcome ReportOutcome, source string, code *string) bool {
	var result, errorCode *string
	switch source {
	case "database_references":
		result, errorCode = outcome.DatabaseResult, outcome.DatabaseError
	case "attempt_owners":
		result, errorCode = outcome.AttemptResult, outcome.AttemptError
	case "storage_scan":
		result, errorCode = outcome.StorageResult, outcome.StorageError
	default:
		return false
	}
	if result == nil {
		return false
	}
	if code == nil {
		return *result == "complete" && errorCode == nil
	}
	return *result == "error" && errorCode != nil && *errorCode == *code
}

func exactSeal(outcome ReportOutcome, count int64, endedAt time.Time) bool {
	return outcome.Sealed && outcome.SealedFindingCount != nil && *outcome.SealedFindingCount == count &&
		outcome.ScanEndedAt != nil && outcome.ScanEndedAt.Equal(endedAt)
}

func classify(snapshot Snapshot, observations []storage.Observation, observedAt time.Time, scope Scope) ([]Finding, error) {
	includeDatabaseFindings := scope != ScopeFiles
	includeFileFindings := scope != ScopeDB
	expected := make(map[string]Reference)
	targets := make(map[string]Reference)
	terminal := make(map[string]Reference)
	owners := make(map[string]AttemptOwner, len(snapshot.Attempts))
	for _, owner := range snapshot.Attempts {
		owners[owner.AttemptID] = owner
	}
	findings := make([]Finding, 0)
	for _, reference := range snapshot.References {
		switch reference.Kind {
		case "transform_target":
			targets[reference.SubjectID] = reference
		case "original", "rendition":
			if reference.RelativeKey == nil || !validReferenceKey(reference) {
				if includeDatabaseFindings {
					findings = append(findings, databaseFinding(reference, "invalid_provenance", "noncanonical_storage_key", observedAt))
				}
				continue
			}
			expected[*reference.RelativeKey] = reference
			if includeDatabaseFindings && reference.Kind == "rendition" && reference.ProvenanceValid != nil && !*reference.ProvenanceValid {
				findings = append(findings, databaseFinding(reference, "invalid_provenance", "target_job_original_profile_mismatch", observedAt))
			}
			if includeDatabaseFindings && reference.Kind == "rendition" && reference.CurrentValid != nil && !*reference.CurrentValid {
				findings = append(findings, databaseFinding(reference, "invalid_current", "current_group_invalid", observedAt))
			}
		case "expired_media":
			if includeDatabaseFindings {
				findings = append(findings, databaseFinding(reference, "expired_media_candidate", "retention_due", observedAt))
			}
		case "expired_rendition":
			if includeDatabaseFindings {
				findings = append(findings, databaseFinding(reference, "expired_rendition_candidate", "retention_due", observedAt))
			}
		case "purged_original", "purged_rendition", "cleanup_deleted":
			if reference.RelativeKey != nil {
				terminal[*reference.RelativeKey] = reference
			}
		}
	}

	seen := make(map[string]bool, len(observations))
	if !includeFileFindings {
		sort.SliceStable(findings, func(i, j int) bool { return findingOrder(findings[i]) < findingOrder(findings[j]) })
		return findings, nil
	}
	for _, observation := range observations {
		seen[observation.RelativeKey] = true
		if observation.Type == storage.ObservationDirectory && expectedStorageDirectory(observation.RelativeKey) && observation.Stable {
			continue
		}
		if !observation.Stable {
			findings = append(findings, observationFinding(observation, "unstable_observation", "namespace_or_metadata_changed", "path", observedAt))
			continue
		}
		if observation.Type == storage.ObservationSymlink {
			findings = append(findings, observationFinding(observation, "symlink", "symlink_refused", "path", observedAt))
			continue
		}
		if observation.Type == storage.ObservationRegular && observation.LinkCount > 1 {
			findings = append(findings, observationFinding(observation, "unexpected_hardlink", "regular_inode_has_multiple_links", "path", observedAt))
			continue
		}
		if observation.Type != storage.ObservationRegular {
			findings = append(findings, observationFinding(observation, "unexpected_type", "unexpected_object_type", "path", observedAt))
			continue
		}
		if reference, ok := terminal[observation.RelativeKey]; ok {
			finding := findingFromReference(reference, "terminal_delete_residue", "completed_delete_left_file", observedAt)
			applyObservation(&finding, observation)
			findings = append(findings, finding)
			continue
		}
		if reference, ok := expected[observation.RelativeKey]; ok {
			if reference.SizeBytes != nil && observation.Size != nil && *reference.SizeBytes != *observation.Size {
				finding := findingFromReference(reference, "size_mismatch", "referenced_size_mismatch", observedAt)
				applyObservation(&finding, observation)
				findings = append(findings, finding)
			} else if reference.SHA256 != nil && observation.SHA256 != nil && *reference.SHA256 != hex.EncodeToString(observation.SHA256[:]) {
				finding := findingFromReference(reference, "sha256_mismatch", "referenced_sha256_mismatch", observedAt)
				applyObservation(&finding, observation)
				findings = append(findings, finding)
			}
			continue
		}
		if key, err := storage.ParseOriginalKey(observation.RelativeKey); err == nil {
			id := key.OriginalID().String()
			finding := observationFinding(observation, "final_orphan", "unreferenced_canonical_final", "original", observedAt)
			finding.SubjectID, finding.ExpectedState = &id, stringPointer("unreferenced")
			findings = append(findings, finding)
			continue
		}
		if key, err := storage.ParseRenditionKey(observation.RelativeKey); err == nil {
			rid, targetID := key.RenditionID().String(), key.JobTargetID().String()
			finding := observationFinding(observation, "final_orphan", "unreferenced_canonical_final", "rendition", observedAt)
			finding.SubjectID, finding.TargetID, finding.ExpectedState = &rid, &targetID, stringPointer("unreferenced")
			if target, ok := targets[targetID]; ok {
				finding.MediaID, finding.JobID = stringPointer(target.MediaID), target.JobID
			}
			findings = append(findings, finding)
			continue
		}
		if key, err := storage.ParseAttemptTempKey(observation.RelativeKey); err == nil {
			findings = append(findings, classifyTemp(snapshot, key, observation, owners[key.AttemptID().String()], targets, observedAt))
			continue
		}
		if _, err := storage.ParseQuarantineKey(observation.RelativeKey); err == nil {
			findings = append(findings, observationFinding(observation, "unexpected_path", "untracked_quarantine_object", "quarantine", observedAt))
			continue
		}
		findings = append(findings, observationFinding(observation, "unexpected_path", "noncanonical_storage_path", "path", observedAt))
	}
	for path, reference := range expected {
		if seen[path] {
			continue
		}
		finding := findingFromReference(reference, "referenced_missing", "referenced_file_absent", observedAt)
		finding.ObservedType = "missing"
		findings = append(findings, finding)
	}
	sort.SliceStable(findings, func(i, j int) bool { return findingOrder(findings[i]) < findingOrder(findings[j]) })
	return findings, nil
}

func classifyTemp(snapshot Snapshot, key storage.AttemptTempKey, observation storage.Observation, owner AttemptOwner, targets map[string]Reference, observedAt time.Time) Finding {
	aged := observation.ModifiedAt != nil && observation.ChangedAt != nil &&
		!observation.ModifiedAt.After(snapshot.CutoffAt.Add(-storage.AttemptTempGrace)) &&
		!observation.ChangedAt.After(snapshot.CutoffAt.Add(-storage.AttemptTempGrace))
	kind, reason := "recent_or_live_temp", "recent_timestamp"
	if aged {
		kind, reason = "aged_attempt_temp", attemptReason(snapshot, key, owner, targets)
	}
	subjectType := "original_attempt_temp"
	subjectID := key.OriginalID().String()
	finding := observationFinding(observation, kind, reason, subjectType, observedAt)
	finding.SubjectID = &subjectID
	attemptID := key.AttemptID().String()
	finding.AttemptID = &attemptID
	if key.Kind() == storage.RenditionFinalAttemptTemp {
		finding.SubjectType = "rendition_attempt_temp"
		if renditionID, ok := key.RenditionID(); ok {
			id := renditionID.String()
			finding.SubjectID = &id
		}
		if targetID, ok := key.JobTargetID(); ok {
			id := targetID.String()
			finding.TargetID = &id
			if target, exists := targets[id]; exists {
				finding.MediaID, finding.JobID = stringPointer(target.MediaID), target.JobID
			}
		}
	}
	return finding
}

func attemptReason(snapshot Snapshot, key storage.AttemptTempKey, owner AttemptOwner, targets map[string]Reference) string {
	if owner.AttemptID == "" {
		return "missing_owner"
	}
	if owner.Coverage != "native" {
		return "legacy_owner"
	}
	if owner.OriginalID != key.OriginalID().String() {
		return "owner_mismatch"
	}
	if key.Kind() == storage.RenditionFinalAttemptTemp {
		targetID, _ := key.JobTargetID()
		target, ok := targets[targetID.String()]
		if owner.Kind != "transform" || !ok || owner.JobID == nil || target.JobID == nil || *owner.JobID != *target.JobID {
			return "owner_mismatch"
		}
	} else if owner.Kind != "upload" || owner.TempRelativeKey == nil || *owner.TempRelativeKey != key.String() {
		return "owner_mismatch"
	}
	switch owner.EventType {
	case "aborted", "released", "expired":
		if owner.EventOccurredAt != nil && !owner.EventOccurredAt.After(snapshot.CutoffAt.Add(-storage.AttemptTempGrace)) {
			return "terminal_owner"
		}
		return "recent_owner"
	case "reclaimed":
		return "reclaimed_owner"
	case "registered", "claimed", "heartbeat":
		if owner.LeaseExpiresAt != nil && !owner.LeaseExpiresAt.After(snapshot.CutoffAt) {
			return "expired_without_terminal"
		}
		return "live_owner"
	default:
		return "unknown_owner"
	}
}

func validReferenceKey(reference Reference) bool {
	if reference.RelativeKey == nil {
		return false
	}
	if reference.Kind == "original" {
		key, err := storage.ParseOriginalKey(*reference.RelativeKey)
		return err == nil && key.OriginalID().String() == reference.SubjectID
	}
	key, err := storage.ParseRenditionKey(*reference.RelativeKey)
	return err == nil && key.RenditionID().String() == reference.SubjectID && reference.OriginalID != nil &&
		key.OriginalID().String() == *reference.OriginalID && reference.TargetID != nil && key.JobTargetID().String() == *reference.TargetID
}

func databaseFinding(reference Reference, kind, reason string, observedAt time.Time) Finding {
	return findingFromReference(reference, kind, reason, observedAt)
}

func findingFromReference(reference Reference, kind, reason string, observedAt time.Time) Finding {
	subjectType := reference.Kind
	if strings.HasPrefix(subjectType, "expired_") {
		subjectType = strings.TrimPrefix(subjectType, "expired_")
	}
	if strings.HasPrefix(subjectType, "purged_") {
		subjectType = strings.TrimPrefix(subjectType, "purged_")
	}
	if subjectType == "cleanup_deleted" {
		subjectType = "rendition"
	}
	return Finding{
		Kind: kind, Reason: reason, SubjectType: subjectType,
		SubjectID: stringPointer(reference.SubjectID), MediaID: stringPointer(reference.MediaID),
		JobID: reference.JobID, TargetID: reference.TargetID,
		RelativeKey: reference.RelativeKey, ExpectedState: reference.ExpectedState,
		ExpectedSize: reference.SizeBytes, ExpectedSHA: reference.SHA256,
		ObservedType: "unknown", ObservedAt: observedAt.UTC(),
	}
}

func observationFinding(observation storage.Observation, kind, reason, subjectType string, observedAt time.Time) Finding {
	finding := Finding{
		Kind: kind, Reason: reason, SubjectType: subjectType,
		RelativeKey: stringPointer(observation.RelativeKey), ObservedType: string(observation.Type),
		ObservedAt: observedAt.UTC(),
	}
	applyObservation(&finding, observation)
	return finding
}

func applyObservation(finding *Finding, observation storage.Observation) {
	finding.ObservedType = string(observation.Type)
	finding.ObservedSize = observation.Size
	finding.ObservedMTime = observation.ModifiedAt
	finding.ObservedCTime = observation.ChangedAt
	if observation.SHA256 != nil {
		value := hex.EncodeToString(observation.SHA256[:])
		finding.ObservedSHA = &value
	}
}

func expectedStorageDirectory(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) == 1 {
		return path == "originals" || path == "renditions" || path == ".quarantine"
	}
	if parts[0] != "originals" && parts[0] != "renditions" {
		return false
	}
	if len(parts) == 2 {
		return len(parts[1]) == 2 && strings.IndexFunc(parts[1], func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
		}) == -1
	}
	originalID, err := storage.ParseOriginalID(parts[2])
	if err != nil || parts[1] != originalID.String()[:2] {
		return false
	}
	if len(parts) == 3 {
		return true
	}
	if parts[0] == "renditions" && len(parts) == 4 {
		_, err := storage.ParseJobTargetID(parts[3])
		return err == nil
	}
	return false
}

func findingOrder(finding Finding) string {
	return pointerValue(finding.RelativeKey) + "\x00" + finding.Kind + "\x00" + finding.SubjectType + "\x00" +
		pointerValue(finding.SubjectID) + "\x00" + pointerValue(finding.MediaID) + "\x00" +
		pointerValue(finding.JobID) + "\x00" + pointerValue(finding.TargetID) + "\x00" +
		pointerValue(finding.AttemptID) + "\x00" + finding.Reason
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func newUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
