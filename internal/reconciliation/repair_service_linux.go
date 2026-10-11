//go:build linux

package reconciliation

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const (
	repairPageLimit         = 1000
	repairConvergenceBudget = 10 * time.Second
)

type repairExecutionRepository interface {
	ReadFindings(context.Context, string) ([]RepairFinding, error)
	ReadRunByAuthorization(context.Context, string) (RepairRun, bool, error)
	ReadRun(context.Context, string) (RepairRun, error)
	BeginRun(context.Context, string, RepairPlanRequest) error
	PrepareItem(context.Context, string, string, string, string) error
	PlanDigest(context.Context, string) (string, error)
	SealPlan(context.Context, string, string, int64, string) error
	ReadManifest(context.Context, string) ([]RepairManifestItem, error)
	Preflight(context.Context, string, string, string) (RepairAuthority, error)
	BeginAttempt(context.Context, string, string, string) (int, error)
	BeginApply(context.Context, string, string, string) (*RepairApplySession, error)
	ReadHistory(context.Context, string) ([]RepairHistoryEvent, error)
	ConfirmHistory(context.Context, string, string) ([]RepairHistoryEvent, error)
	Finish(context.Context, string) (RepairExecutionResult, error)
}

type Repairer struct {
	repository        repairExecutionRepository
	store             *storage.Store
	newID             func() (string, error)
	convergenceBudget time.Duration
}

func NewRepairer(repository repairExecutionRepository, store *storage.Store) (*Repairer, error) {
	if repository == nil || store == nil {
		return nil, errors.New("repair repository and storage are required")
	}
	return &Repairer{repository: repository, store: store, newID: newUUIDv4, convergenceBudget: repairConvergenceBudget}, nil
}

func (repairer *Repairer) Plan(ctx context.Context, request RepairPlanRequest) (RepairPlan, error) {
	if ctx == nil || request.ReportID == "" || request.AuthorizationID == "" || request.Actor == "" {
		return RepairPlan{}, ErrRepairInvalid
	}
	findings, err := repairer.repository.ReadFindings(ctx, request.ReportID)
	if err != nil {
		return RepairPlan{}, err
	}
	run, exists, err := repairer.repository.ReadRunByAuthorization(ctx, request.AuthorizationID)
	if err != nil {
		return RepairPlan{}, err
	}
	replayed := exists
	if exists {
		if run.ReportID != request.ReportID || run.Actor != request.Actor {
			return RepairPlan{}, ErrRepairConflict
		}
	} else {
		runID, idErr := repairer.newID()
		if idErr != nil {
			return RepairPlan{}, idErr
		}
		if err = repairer.repository.BeginRun(ctx, runID, request); err != nil {
			confirmed, found, confirmErr := repairer.repository.ReadRunByAuthorization(context.WithoutCancel(ctx), request.AuthorizationID)
			if confirmErr != nil || !found {
				return RepairPlan{}, errors.Join(err, confirmErr)
			}
			// The authorization ID is the caller's idempotency key. Concurrent
			// planners may have generated different candidate run UUIDs; adopt the
			// one that won only when its complete logical input is identical.
			if confirmed.ReportID != request.ReportID || confirmed.AuthorizationID != request.AuthorizationID || confirmed.Actor != request.Actor {
				return RepairPlan{}, ErrRepairConflict
			}
			run = confirmed
		} else {
			run, err = repairer.repository.ReadRun(ctx, runID)
			if err != nil {
				return RepairPlan{}, err
			}
		}
	}
	existing, err := repairer.repository.ReadManifest(ctx, run.RunID)
	if err != nil {
		return RepairPlan{}, err
	}
	byFinding := make(map[string]RepairManifestItem, len(existing))
	for _, item := range existing {
		byFinding[item.FindingID] = item
	}
	items := make([]RepairManifestItem, 0, len(findings))
	for _, finding := range findings {
		if item, ok := byFinding[finding.FindingID]; ok {
			item.RepairFinding = finding
			items = append(items, item)
			continue
		}
		itemID, idErr := repairer.newID()
		if idErr != nil {
			return RepairPlan{}, idErr
		}
		quarantineID, idErr := repairer.newID()
		if idErr != nil {
			return RepairPlan{}, idErr
		}
		if err = repairer.repository.PrepareItem(ctx, run.RunID, itemID, finding.FindingID, quarantineID); err != nil {
			// A response-loss or concurrent planner can leave the exact finding
			// durably prepared under the winner's UUIDs. Read and reuse those
			// persisted IDs rather than generating another destination.
			confirmed, confirmErr := repairer.repository.ReadManifest(context.WithoutCancel(ctx), run.RunID)
			if confirmErr != nil {
				return RepairPlan{}, errors.Join(err, confirmErr)
			}
			var found *RepairManifestItem
			for index := range confirmed {
				if confirmed[index].FindingID == finding.FindingID {
					found = &confirmed[index]
					break
				}
			}
			if found == nil {
				return RepairPlan{}, err
			}
			found.RepairFinding = finding
			items = append(items, *found)
			continue
		}
		items = append(items, RepairManifestItem{RepairFinding: finding, RunID: run.RunID, ItemID: itemID, QuarantineID: quarantineID, DestinationRelativeKey: ".quarantine/" + quarantineID})
	}
	items, err = repairer.repository.ReadManifest(ctx, run.RunID)
	if err != nil {
		return RepairPlan{}, err
	}
	if len(items) != len(findings) {
		return RepairPlan{}, ErrRepairIncomplete
	}
	digest, err := repairer.repository.PlanDigest(ctx, run.RunID)
	if err != nil {
		return RepairPlan{}, err
	}
	if run.PlanID == nil {
		planID, idErr := repairer.newID()
		if idErr != nil {
			return RepairPlan{}, idErr
		}
		if err = repairer.repository.SealPlan(ctx, planID, run.RunID, int64(len(items)), digest); err != nil {
			confirmed, confirmErr := repairer.repository.ReadRun(context.WithoutCancel(ctx), run.RunID)
			if confirmErr != nil || confirmed.PlanID == nil || confirmed.PlanDigest == nil || *confirmed.PlanDigest != digest || confirmed.PlanItemCount == nil || *confirmed.PlanItemCount != int64(len(items)) {
				return RepairPlan{}, errors.Join(err, confirmErr)
			}
			run = confirmed
		} else {
			run, err = repairer.repository.ReadRun(ctx, run.RunID)
			if err != nil {
				return RepairPlan{}, err
			}
		}
	} else if run.PlanDigest == nil || *run.PlanDigest != digest || run.PlanItemCount == nil || *run.PlanItemCount != int64(len(items)) {
		return RepairPlan{}, ErrRepairConflict
	}
	return RepairPlan{Run: run, Items: items, Replayed: replayed}, nil
}

func (repairer *Repairer) DryRun(ctx context.Context, reportID string) (RepairPreview, error) {
	if ctx == nil || reportID == "" {
		return RepairPreview{}, ErrRepairInvalid
	}
	findings, err := repairer.repository.ReadFindings(ctx, reportID)
	if err != nil {
		return RepairPreview{}, err
	}
	preview := RepairPreview{ReportID: reportID, Items: make([]RepairPreviewItem, 0, len(findings))}
	for _, finding := range findings {
		previewID, idErr := repairer.newID()
		if idErr != nil {
			return RepairPreview{}, idErr
		}
		authority, preflightErr := repairer.repository.Preflight(ctx, reportID, finding.FindingID, previewID)
		item := RepairPreviewItem{FindingID: finding.FindingID, SourceKey: finding.SourceRelativeKey, PreviewDestination: ".quarantine/" + previewID}
		if preflightErr != nil {
			return RepairPreview{}, preflightErr
		}
		if authority.Decision != "apply" {
			item.Decision = authority.Decision
			item.Reason = authority.Reason
			preview.Items = append(preview.Items, item)
			continue
		}
		progress, inspectErr := repairer.inspectStorage(ctx, finding, previewID)
		if inspectErr != nil {
			item.Decision, item.Reason = repairStorageDecision(inspectErr, false)
		} else if progress.Disposition == storage.RepairBothAbsent {
			item.Decision = "resolved"
			item.Reason = "source_missing"
		} else {
			item.Decision = "eligible"
			item.Reason = "matched"
		}
		preview.Items = append(preview.Items, item)
	}
	return preview, nil
}

func (repairer *Repairer) Execute(ctx context.Context, runID string) (RepairExecutionResult, error) {
	if ctx == nil || runID == "" {
		return RepairExecutionResult{}, ErrRepairInvalid
	}
	run, err := repairer.repository.ReadRun(ctx, runID)
	if err != nil {
		return RepairExecutionResult{}, err
	}
	if run.PlanID == nil {
		return RepairExecutionResult{}, ErrRepairIncomplete
	}
	items, err := repairer.repository.ReadManifest(ctx, runID)
	if err != nil {
		return RepairExecutionResult{}, err
	}
	for index := range items {
		if items[index].EffectiveOutcome != nil && *items[index].EffectiveOutcome != "failed" {
			continue
		}
		if err := repairer.executeItem(ctx, &items[index]); err != nil {
			return RepairExecutionResult{}, err
		}
	}
	return repairer.repository.Finish(ctx, runID)
}

func (repairer *Repairer) executeItem(parent context.Context, item *RepairManifestItem) error {
	history, err := repairer.repository.ReadHistory(parent, item.ItemID)
	if err != nil {
		return err
	}
	attemptID := ""
	physical := false
	if len(history) > 0 {
		attemptID = history[len(history)-1].AttemptID
		for _, event := range history {
			physical = physical || event.PhysicalEffectPossible
		}
	}
	if attemptID == "" || history[len(history)-1].EventType == "failed" {
		attemptID, err = repairer.newID()
		if err != nil {
			return err
		}
		preparedID, idErr := repairer.newID()
		if idErr != nil {
			return idErr
		}
		if _, err = repairer.repository.BeginAttempt(parent, attemptID, item.ItemID, preparedID); err != nil {
			confirmed, confirmErr := repairer.repository.ReadHistory(context.WithoutCancel(parent), item.ItemID)
			if confirmErr != nil || len(confirmed) == 0 {
				return errors.Join(err, confirmErr)
			}
			latest := confirmed[len(confirmed)-1]
			if repairAttemptTerminal(latest.EventType) {
				return nil
			}
			if latest.EventType != "prepared" && latest.EventType != "uncertain" {
				return err
			}
			attemptID = latest.AttemptID
		}
	}
	item.ActiveAttemptID = &attemptID
	for recoveryPass := 0; recoveryPass < 2; recoveryPass++ {
		applyCtx := parent
		cancel := func() {}
		if physical || recoveryPass > 0 {
			applyCtx, cancel = context.WithTimeout(context.Background(), repairer.convergenceBudget)
		}
		outcome, applyErr := repairer.applyAttempt(applyCtx, item, physical || recoveryPass > 0)
		cancel()
		if applyErr != nil {
			return applyErr
		}
		if outcome != "uncertain" {
			if outcome == "failed" {
				return ErrRepairRetryable
			}
			return nil
		}
		physical = true
	}
	// The bounded protocol could not establish truth. First persist an
	// uncertainty convergence boundary, then use the same attempt and exact
	// destination to persist final unknown. The separate boundary is necessary
	// when both earlier physical-effect transactions rolled back: the database
	// must never accept unknown without durable uncertainty evidence.
	finalCtx, cancel := context.WithTimeout(context.Background(), repairer.convergenceBudget)
	defer cancel()
	session, err := repairer.repository.BeginApply(finalCtx, item.RunID, item.ItemID, attemptID)
	if err != nil {
		return errors.Join(ErrRepairOutcomeUnknown, err)
	}
	if session.Authority().Decision == "terminal" {
		_ = session.Rollback(finalCtx)
		return nil
	}
	if err = repairer.appendEvent(finalCtx, session, attemptID, "applying", "applying", nil, nil, nil); err == nil {
		code := "outcome_uncertain"
		err = repairer.appendEvent(finalCtx, session, attemptID, "uncertain", "unknown", &code, nil, nil)
	}
	if err != nil {
		_ = session.Rollback(context.Background())
		return errors.Join(ErrRepairOutcomeUnknown, err)
	}
	if err = session.Commit(finalCtx); err != nil {
		if confirmErr := repairer.confirmCommit(finalCtx, item, "uncertain", err); confirmErr != nil {
			return errors.Join(ErrRepairOutcomeUnknown, confirmErr)
		}
	}
	session, err = repairer.repository.BeginApply(finalCtx, item.RunID, item.ItemID, attemptID)
	if err != nil {
		return errors.Join(ErrRepairOutcomeUnknown, err)
	}
	if session.Authority().Decision == "terminal" {
		_ = session.Rollback(finalCtx)
		return nil
	}
	if err = repairer.appendEvent(finalCtx, session, attemptID, "applying", "applying", nil, nil, nil); err == nil {
		code := "outcome_uncertain"
		err = repairer.appendEvent(finalCtx, session, attemptID, "unknown", "unknown", &code, nil, nil)
	}
	if err != nil {
		_ = session.Rollback(context.Background())
		return errors.Join(ErrRepairOutcomeUnknown, err)
	}
	if err = session.Commit(finalCtx); err != nil {
		return repairer.confirmCommit(finalCtx, item, "unknown", err)
	}
	return nil
}

func (repairer *Repairer) applyAttempt(parent context.Context, item *RepairManifestItem, physicalBefore bool) (string, error) {
	if item.ActiveAttemptID == nil || *item.ActiveAttemptID == "" {
		return "", ErrRepairIncomplete
	}
	session, err := repairer.repository.BeginApply(parent, item.RunID, item.ItemID, *item.ActiveAttemptID)
	if err != nil {
		return "", err
	}
	attemptID := *item.ActiveAttemptID
	rollback := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Rollback(ctx)
	}
	authority := session.Authority()
	if authority.Decision == "terminal" {
		rollback()
		return authority.Reason, nil
	}
	if err = repairer.appendEvent(parent, session, attemptID, "applying", "applying", nil, nil, nil); err != nil {
		rollback()
		return "", err
	}
	if authority.Decision != "apply" {
		code := authority.Reason
		if code == "" {
			code = "source_changed"
		}
		event, outcome := "stale", "stale"
		if physicalBefore {
			event, outcome, code = "unknown", "unknown", "outcome_uncertain"
		}
		if err = repairer.appendEvent(parent, session, attemptID, event, outcome, &code, nil, nil); err != nil {
			rollback()
			return "", err
		}
		if err = session.Commit(parent); err != nil {
			return repairer.recoverCommit(parent, item, event, physicalBefore, err)
		}
		return event, nil
	}
	operation, progress, storageErr := repairer.beginStorage(parent, *item)
	if storageErr != nil {
		event, outcome, code := repairStorageEvent(storageErr, physicalBefore)
		if err = repairer.appendEvent(parent, session, attemptID, event, outcome, &code, nil, nil); err != nil {
			rollback()
			return "", errors.Join(storageErr, err)
		}
		if err = session.Commit(parent); err != nil {
			return repairer.recoverCommit(parent, item, event, event == "uncertain" || physicalBefore, err)
		}
		return event, nil
	}
	defer operation.Close()
	if progress.Disposition == storage.RepairBothAbsent {
		code := "source_missing"
		event, outcome := "resolved", "resolved"
		if physicalBefore {
			event, outcome, code = "unknown", "unknown", "outcome_uncertain"
		}
		if err = repairer.appendEvent(parent, session, attemptID, event, outcome, &code, nil, nil); err != nil {
			rollback()
			return "", err
		}
		if err = session.Commit(parent); err != nil {
			return "", repairer.confirmCommit(parent, item, event, err)
		}
		return event, nil
	}
	progress, err = operation.Revalidate(parent)
	if err != nil {
		event, outcome, code := repairStorageEvent(err, physicalBefore)
		if appendErr := repairer.appendEvent(parent, session, attemptID, event, outcome, &code, nil, nil); appendErr != nil {
			rollback()
			return "", errors.Join(err, appendErr)
		}
		if commitErr := session.Commit(parent); commitErr != nil {
			return repairer.recoverCommit(parent, item, event, event == "uncertain" || physicalBefore, commitErr)
		}
		return event, nil
	}
	size := progress.ObservedSize
	sha := hex.EncodeToString(progress.ObservedSHA256[:])
	if err = repairer.appendEvent(parent, session, attemptID, "revalidated", "matched", nil, &size, &sha); err != nil {
		rollback()
		return "", err
	}
	progress, renameErr := operation.Rename(parent)
	if !progress.RenameObserved {
		event, outcome, code := repairStorageEvent(renameErr, false)
		if renameErr == nil {
			event, outcome, code = "failed", "failed", "internal"
		}
		if err = repairer.appendEvent(parent, session, attemptID, event, outcome, &code, nil, nil); err != nil {
			rollback()
			return "", errors.Join(renameErr, err)
		}
		if err = session.Commit(parent); err != nil {
			return repairer.recoverCommit(parent, item, event, event == "uncertain", err)
		}
		return event, nil
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), repairer.convergenceBudget)
	defer cancel()
	var convergenceErr error
	if err = repairer.appendEvent(drainCtx, session, attemptID, "rename", "renamed", nil, nil, nil); err != nil {
		convergenceErr = errors.Join(convergenceErr, err)
	}
	progress, sourceErr := operation.SyncSourceDirectory(drainCtx)
	convergenceErr = errors.Join(convergenceErr, renameErr, sourceErr)
	if progress.SourceDirectoryDurable {
		if err = repairer.appendEvent(drainCtx, session, attemptID, "source_directory_fsync", "durable", nil, nil, nil); err != nil {
			convergenceErr = errors.Join(convergenceErr, err)
		}
	}
	progress, destinationErr := operation.SyncDestinationDirectory(drainCtx)
	convergenceErr = errors.Join(convergenceErr, destinationErr)
	if progress.DestinationDirectoryDurable {
		if err = repairer.appendEvent(drainCtx, session, attemptID, "destination_directory_fsync", "durable", nil, nil, nil); err != nil {
			convergenceErr = errors.Join(convergenceErr, err)
		}
	}
	progress, verifyErr := operation.VerifyCanonicalDestination(drainCtx)
	convergenceErr = errors.Join(convergenceErr, verifyErr)
	terminal := "completed"
	outcome := "quarantined"
	var errorCode *string
	if convergenceErr != nil || !progress.SourceDirectoryDurable || !progress.DestinationDirectoryDurable || !progress.CanonicalDestinationVerified {
		terminal = "uncertain"
		outcome = "unknown"
		code := "outcome_uncertain"
		errorCode = &code
	}
	if err = repairer.appendEvent(drainCtx, session, attemptID, terminal, outcome, errorCode, &size, &sha); err != nil {
		rollback()
		// Rename was observed before this journal failure. Re-enter the bounded
		// same-attempt recovery loop so a rolled-back applying transaction never
		// strands an exact quarantine destination behind prepared-only evidence.
		return "uncertain", nil
	}
	if err = session.Commit(drainCtx); err != nil {
		return repairer.recoverCommit(drainCtx, item, terminal, true, err)
	}
	return terminal, nil
}

func (repairer *Repairer) appendEvent(ctx context.Context, session *RepairApplySession, attemptID, eventType, outcome string, errorCode *string, size *int64, sha *string) error {
	id, err := repairer.newID()
	if err != nil {
		return err
	}
	_, err = session.Append(ctx, RepairEvent{ID: id, AttemptID: attemptID, Type: eventType, Outcome: outcome, ErrorCode: errorCode, ObservedSize: size, ObservedSHA: sha})
	return err
}

func (repairer *Repairer) confirmCommit(ctx context.Context, item *RepairManifestItem, want string, commitErr error) error {
	confirmCtx, cancel := context.WithTimeout(context.Background(), repairer.convergenceBudget)
	defer cancel()
	history, err := repairer.repository.ConfirmHistory(confirmCtx, item.RunID, item.ItemID)
	if err != nil {
		return errors.Join(commitErr, err)
	}
	if len(history) > 0 && history[len(history)-1].EventType == want {
		return nil
	}
	return errors.Join(commitErr, ErrRepairOutcomeUnknown)
}

func (repairer *Repairer) recoverCommit(ctx context.Context, item *RepairManifestItem, want string, physicalEffectPossible bool, commitErr error) (string, error) {
	if err := repairer.confirmCommit(ctx, item, want, commitErr); err == nil {
		return want, nil
	} else if physicalEffectPossible {
		// The caller's bounded loop reopens the same attempt and exact
		// destination. This is required when a rename may have occurred but the
		// transaction or its response was lost.
		return "uncertain", nil
	} else {
		return "", err
	}
}

func (repairer *Repairer) inspectStorage(ctx context.Context, finding RepairFinding, quarantineID string) (storage.RepairProgress, error) {
	destination, err := repairQuarantineKey(quarantineID)
	if err != nil {
		return storage.RepairProgress{}, err
	}
	expectation := storage.RepairExpectation{Size: finding.ExpectedSize, SHA256: finding.ExpectedSHA256, MTime: finding.ObservedMTime, CTime: finding.ObservedCTime}
	switch finding.SourceType {
	case RepairOriginalFinal:
		key, err := storage.ParseOriginalKey(finding.SourceRelativeKey)
		if err != nil {
			return storage.RepairProgress{}, err
		}
		return repairer.store.InspectRepairOriginal(ctx, key, destination, expectation)
	case RepairRenditionFinal:
		key, err := storage.ParseRenditionKey(finding.SourceRelativeKey)
		if err != nil {
			return storage.RepairProgress{}, err
		}
		return repairer.store.InspectRepairRendition(ctx, key, destination, expectation)
	case RepairOriginalAttemptTemp, RepairRenditionAttemptTemp:
		key, err := storage.ParseAttemptTempKey(finding.SourceRelativeKey)
		if err != nil {
			return storage.RepairProgress{}, err
		}
		return repairer.store.InspectRepairAttemptTemp(ctx, key, destination, expectation)
	default:
		return storage.RepairProgress{}, ErrRepairAdmission
	}
}

func (repairer *Repairer) beginStorage(ctx context.Context, item RepairManifestItem) (*storage.RepairQuarantine, storage.RepairProgress, error) {
	destination, err := repairQuarantineKey(item.QuarantineID)
	if err != nil {
		return nil, storage.RepairProgress{}, err
	}
	expectation := storage.RepairExpectation{Size: item.ExpectedSize, SHA256: item.ExpectedSHA256, MTime: item.ObservedMTime, CTime: item.ObservedCTime}
	switch item.SourceType {
	case RepairOriginalFinal:
		key, err := storage.ParseOriginalKey(item.SourceRelativeKey)
		if err != nil {
			return nil, storage.RepairProgress{}, err
		}
		return repairer.store.BeginRepairOriginal(ctx, key, destination, expectation)
	case RepairRenditionFinal:
		key, err := storage.ParseRenditionKey(item.SourceRelativeKey)
		if err != nil {
			return nil, storage.RepairProgress{}, err
		}
		return repairer.store.BeginRepairRendition(ctx, key, destination, expectation)
	case RepairOriginalAttemptTemp, RepairRenditionAttemptTemp:
		key, err := storage.ParseAttemptTempKey(item.SourceRelativeKey)
		if err != nil {
			return nil, storage.RepairProgress{}, err
		}
		return repairer.store.BeginRepairAttemptTemp(ctx, key, destination, expectation)
	default:
		return nil, storage.RepairProgress{}, ErrRepairAdmission
	}
}

func repairQuarantineKey(id string) (storage.QuarantineKey, error) {
	parsed, err := storage.ParseQuarantineID(id)
	if err != nil {
		return storage.QuarantineKey{}, err
	}
	return storage.NewQuarantineKey(parsed)
}

func repairStorageDecision(err error, physical bool) (string, string) {
	event, _, code := repairStorageEvent(err, physical)
	return event, code
}
func repairStorageEvent(err error, physical bool) (string, string, string) {
	if physical {
		return "unknown", "unknown", "outcome_uncertain"
	}
	var repairErr *storage.RepairError
	if errors.As(err, &repairErr) {
		// Source-absent/destination-present can be the only observable evidence
		// left by a rename whose database transaction/response was lost. First
		// persist a recoverable uncertainty boundary; the same attempt and
		// destination will then be inspected again before final unknown.
		if repairErr.Progress.RenameObserved || repairErr.Progress.DestinationPresent {
			return "uncertain", "unknown", "outcome_uncertain"
		}
		switch repairErr.Cause {
		case storage.RepairCauseSourceMissing:
			return "resolved", "resolved", "source_missing"
		case storage.RepairCauseSourceChanged, storage.RepairCauseManifestMismatch, storage.RepairCauseSourceRestored:
			return "stale", "stale", "source_changed"
		case storage.RepairCauseCancelled:
			return "failed", "failed", "cancelled"
		default:
			return "failed", "failed", string(repairErr.Cause)
		}
	}
	return "failed", "failed", "internal"
}

func repairAttemptTerminal(eventType string) bool {
	switch eventType {
	case "completed", "failed", "stale", "resolved", "unknown":
		return true
	default:
		return false
	}
}

func (result RepairExecutionResult) String() string {
	return fmt.Sprintf("%s:%d/%d", result.Result, result.Quarantined, result.Manifest)
}
