package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/renditioncleanup"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// sessionLock spans operations that must use the existing services' independent
// transactions. A failed unlock destroys the connection, never returning a
// locked PostgreSQL session to the pool.
func (s *Service) sessionLock(ctx context.Context, name string) (func(), error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,1818))`, name).Scan(&locked); err != nil {
		conn.Release()
		return nil, err
	}
	if !locked {
		conn.Release()
		return nil, ErrConflict
	}
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(cleanupCtx, `SELECT pg_advisory_unlock(hashtextextended($1,1818))`, name).Scan(&unlocked)
		if err != nil || !unlocked {
			_ = conn.Hijack().Close(cleanupCtx)
			return
		}
		conn.Release()
	}, nil
}

func (s *Service) Operational(ctx context.Context, o options) (any, error) {
	if o.command == "maintenance status" {
		return s.MaintenanceStatus(ctx)
	}
	root := os.Getenv("NMCP_STORAGE_ROOT")
	if root == "" {
		return nil, ErrValidation
	}
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	switch o.command {
	case "check":
		return s.Check(ctx, store, reconciliation.Scope(o.scope))
	case "repair":
		return s.Repair(ctx, store, o.report, o.dry)
	case "cleanup":
		return s.Cleanup(ctx, store, o.kind, o.dry)
	case "maintenance resume":
		return s.MaintenanceResume(ctx, store)
	}
	return nil, ErrValidation
}

func (s *Service) Check(ctx context.Context, store *storage.Store, scope reconciliation.Scope) (reconciliation.Result, error) {
	repository, err := reconciliation.NewPostgresRepository(s.Pool)
	if err != nil {
		return reconciliation.Result{}, err
	}
	checker, err := reconciliation.NewChecker(repository, store)
	if err != nil {
		return reconciliation.Result{}, err
	}
	result, err := checker.Check(ctx, scope)
	if err == nil && (!result.Sealed || result.FindingCount > 0) {
		err = ErrConflict
	}
	return result, err
}

type Maintenance struct {
	Mode            string          `json:"mode"`
	Reason          *string         `json:"reason"`
	Owner           *string         `json:"owner"`
	EnteredAt       *time.Time      `json:"entered_at"`
	SuccessfulCheck json.RawMessage `json:"successful_check_report"`
}

func (s *Service) MaintenanceStatus(ctx context.Context) (Maintenance, error) {
	var state Maintenance
	err := s.Pool.QueryRow(ctx, `SELECT mode,reason,owner,entered_at,successful_check_report FROM maintenance_state WHERE id=1`).Scan(
		&state.Mode, &state.Reason, &state.Owner, &state.EnteredAt, &state.SuccessfulCheck)
	return state, err
}

func (s *Service) MaintenanceResume(ctx context.Context, store *storage.Store) (any, error) {
	unlock, err := s.sessionLock(ctx, "maintenance")
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := s.MaintenanceStatus(ctx)
	if err != nil {
		return nil, err
	}
	if state.Mode != "maintenance" {
		return state, ErrConflict
	}
	check, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if err != nil {
		return check, err
	}
	encoded, _ := json.Marshal(check)
	err = s.mutate(ctx, "maintenance_resume", "maintenance-state", map[string]any{"report_id": check.ReportID}, []string{check.ReportID}, func(tx pgx.Tx) error {
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT mode='maintenance' AND entered_at=$1 AND entered_at<=$2
			FROM maintenance_state WHERE id=1 FOR UPDATE NOWAIT`, state.EnteredAt, check.StartedAt).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return ErrConflict
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_check_reports r
			JOIN reconciliation_check_report_seals seal ON seal.report_id=r.id
			WHERE r.id=$1 AND r.scope='all' AND seal.finding_count=0
			AND (SELECT count(*) FROM reconciliation_check_source_results src WHERE src.report_id=r.id AND src.result='complete')=3)
			AND NOT EXISTS(SELECT 1 FROM reconciliation_repair_runs run WHERE NOT EXISTS
			(SELECT 1 FROM reconciliation_repair_results result WHERE result.run_id=run.id))`, check.ReportID).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrConflict
		}
		_, err := tx.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL,
			successful_check_report=$1,updated_at=clock_timestamp() WHERE id=1`, encoded)
		return err
	})
	if err != nil {
		return check, err
	}
	return s.MaintenanceStatus(ctx)
}

func repairError(err error) error {
	switch {
	case errors.Is(err, reconciliation.ErrRepairInvalid):
		return ErrValidation
	case errors.Is(err, reconciliation.ErrRepairConflict), errors.Is(err, reconciliation.ErrRepairAdmission), errors.Is(err, reconciliation.ErrRepairMaintenance):
		return ErrConflict
	default:
		return err
	}
}

func (s *Service) Repair(ctx context.Context, store *storage.Store, report string, dry bool) (any, error) {
	repository, err := reconciliation.NewRepairPostgresRepository(s.Pool)
	if err != nil {
		return nil, err
	}
	repairer, err := reconciliation.NewRepairer(repository, store)
	if err != nil {
		return nil, err
	}
	if dry {
		state, err := s.MaintenanceStatus(ctx)
		if err != nil {
			return nil, err
		}
		if state.Mode != "maintenance" {
			return nil, ErrConflict
		}
		result, err := repairer.DryRun(ctx, report)
		return result, repairError(err)
	}
	unlock, err := s.sessionLock(ctx, "maintenance")
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Admission before maintenance: malformed/unsealed reports cannot disable
	// the service. This is read-only; execution repeats every safety check.
	if _, err := repository.ReadFindings(ctx, report); err != nil {
		return nil, repairError(err)
	}
	err = s.mutate(ctx, "repair_enter", "maintenance-state", map[string]any{"report_id": report}, []string{report}, func(tx pgx.Tx) error {
		var mode string
		if err := tx.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1 FOR UPDATE NOWAIT`).Scan(&mode); err != nil {
			return err
		}
		if mode == "maintenance" {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='administrative repair',owner=$1,
			entered_at=clock_timestamp(),successful_check_report=NULL,updated_at=clock_timestamp() WHERE id=1`, s.Actor+"@"+s.Host)
		return err
	})
	if err != nil {
		return nil, err
	}
	// report ID is a durable idempotency authorization. Re-running the same
	// report adopts the exact plan/quarantine destinations, never a new run.
	plan, err := repairer.Plan(ctx, reconciliation.RepairPlanRequest{ReportID: report, AuthorizationID: report, Actor: s.Actor})
	if err != nil {
		return nil, repairError(err)
	}
	result, effectErr := repairer.Execute(ctx, plan.Run.RunID)
	outcome := "succeeded"
	if effectErr != nil || result.Failed > 0 || result.Unknown > 0 {
		outcome = "partial"
	}
	auditErr := s.recordAudit(ctx, "repair_result", map[string]any{"report_id": report}, []string{plan.Run.RunID}, outcome)
	if effectErr != nil || outcome == "partial" {
		return result, ErrPartial
	}
	return result, auditErr
}

type CleanupResult struct {
	Kind         string   `json:"kind"`
	DryRun       bool     `json:"dry_run"`
	Eligible     int      `json:"eligible"`
	Completed    int      `json:"completed"`
	Failed       int      `json:"failed"`
	MorePossible bool     `json:"more_possible"`
	IDs          []string `json:"affected_ids"`
}

func (s *Service) Cleanup(ctx context.Context, store *storage.Store, kind string, dry bool) (CleanupResult, error) {
	result := CleanupResult{Kind: kind, DryRun: dry, IDs: []string{}}
	if kind == "all" || kind == "backups" {
		return result, ErrUnsupported
	}
	unlock, err := s.sessionLock(ctx, "cleanup:"+kind)
	if err != nil {
		return result, err
	}
	defer unlock()
	service, err := medialifecycle.NewService(s.Pool, os.Getenv("NMCP_FILE_BASE_URL"))
	if err != nil {
		return result, err
	}
	if kind == "renditions" {
		if dry {
			err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE NOT is_current AND purge_after<=clock_timestamp()`).Scan(&result.Eligible)
			return result, err
		}
		if err := s.mutate(ctx, "cleanup_begin", "cleanup-audit:renditions", map[string]any{"kind": kind, "limit": 100}, nil, func(tx pgx.Tx) error { return normal(ctx, tx) }); err != nil {
			return result, err
		}
		runner, err := renditioncleanup.New(service, store, renditioncleanup.Options{SweepLimit: 100}, nil)
		if err != nil {
			return result, err
		}
		sweep, err := runner.Sweep(ctx)
		result.IDs, result.Completed, result.Failed, result.MorePossible = sweep.IDs, sweep.Completed, sweep.Failed, sweep.LimitReached
		result.Eligible = result.Completed + result.Failed
		if auditErr := s.cleanupAudit(ctx, result); auditErr != nil {
			return result, auditErr
		}
		if err != nil || result.Failed > 0 {
			return result, ErrPartial
		}
		return result, nil
	}
	if kind != "media" {
		return result, ErrValidation
	}
	cutoff, err := service.DatabaseNow(ctx)
	if err != nil {
		return result, err
	}
	candidates, err := service.ScanDuePurges(ctx, cutoff, nil, 100)
	if err != nil {
		return result, err
	}
	result.Eligible, result.MorePossible = len(candidates), len(candidates) == 100
	if dry {
		return result, nil
	}
	if err := s.mutate(ctx, "cleanup_begin", "cleanup-audit:media", map[string]any{"kind": kind, "limit": 100}, nil, func(tx pgx.Tx) error { return normal(ctx, tx) }); err != nil {
		return result, err
	}
	// One finite snapshot, no daemon Run loop. Existing purge manifests and
	// per-file receipts converge a crash. We only execute these due media.
	for _, candidate := range candidates {
		enqueued, err := service.EnqueueDuePurge(ctx, candidate.MediaID)
		if err != nil {
			result.Failed++
			continue
		}
		lease, err := service.StartPurge(ctx, enqueued.Job.ID)
		if err != nil {
			result.Failed++
			continue
		}
		for {
			if _, err = service.HeartbeatPurge(ctx, lease.JobID, lease.Token); err != nil {
				break
			}
			var step medialifecycle.PurgeStepResult
			step, err = service.RunPurgeFileStep(ctx, lease.JobID, lease.Token, medialifecycle.StoragePurgeFileAction(store))
			if err != nil {
				break
			}
			if step.Done {
				err = service.FinalizePurge(ctx, lease.JobID, lease.Token)
				break
			}
		}
		if err != nil {
			result.Failed++
			// Leave durable manifests intact. Existing expiry/reclaim will
			// recover an interrupted step; never erase failed job history.
			continue
		}
		result.Completed++
		result.IDs = append(result.IDs, candidate.MediaID)
	}
	if err := s.cleanupAudit(ctx, result); err != nil {
		return result, err
	}
	if result.Failed > 0 {
		return result, ErrPartial
	}
	return result, nil
}

func (s *Service) cleanupAudit(ctx context.Context, result CleanupResult) error {
	outcome := "succeeded"
	if result.Failed > 0 {
		outcome = "partial"
	}
	return s.recordAudit(ctx, "cleanup_result", map[string]any{
		"kind": result.Kind, "completed": result.Completed, "failed": result.Failed,
		"more_possible": result.MorePossible}, result.IDs, outcome)
}

func (s *Service) recordAudit(ctx context.Context, command string, args any, ids []string, outcome string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := s.audit(ctx, tx, command, args, ids, outcome, time.Now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
