package admin

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

type cleanupLifecycle interface {
	EnqueueDuePurge(context.Context, string) (medialifecycle.EnqueueResult, error)
	ReclaimExpiredPurge(context.Context, string) error
	StartPurge(context.Context, string) (medialifecycle.PurgeLease, error)
	HeartbeatPurge(context.Context, string, string) (time.Time, error)
	RunPurgeFileStep(context.Context, string, string, medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error)
	FinalizePurge(context.Context, string, string) error
	FinishPurgeAttempt(context.Context, string, string, string) error
	CleanupNextRendition(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error)
}

type cleanupItem struct {
	id      string
	mediaID string
	jobID   *string
}

// Cleanup automatically resumes an unfinished finite run of the same kind.
// Its durable authorization fixes exact object IDs BEFORE any destructive call.
// Native lifecycle journals prove effects after response loss or audit failure;
// each completion receipt and its affected-ID audit commit atomically.
func (s *Service) Cleanup(ctx context.Context, store *storage.Store, kind string, dry bool) (CleanupResult, error) {
	result := CleanupResult{Kind: kind, DryRun: dry, IDs: []string{}}
	if kind == "all" || kind == "backups" {
		return result, ErrUnsupported
	}
	if kind != "media" && kind != "renditions" {
		return result, ErrValidation
	}
	unlock, err := s.sessionLock(ctx, "cleanup:"+kind)
	if err != nil {
		return result, err
	}
	defer unlock()
	items, runID, err := s.cleanupSelection(ctx, kind, dry)
	result.RunID, result.Eligible = runID, len(items)
	if err != nil || dry || len(items) == 0 {
		return result, err
	}
	lifecycle, err := medialifecycle.NewService(s.Pool, os.Getenv("NMCP_FILE_BASE_URL"))
	if err != nil {
		return result, err
	}
	for _, item := range items {
		completed, skipped, effectErr := s.cleanupOne(ctx, lifecycle, store, kind, runID, item)
		if effectErr != nil {
			result.Failed++
			continue
		}
		outcome := "completed"
		if skipped {
			outcome = "skipped"
		}
		if !completed && !skipped {
			result.Failed++
			continue
		}
		if err := s.cleanupReceipt(ctx, kind, runID, item, outcome); err != nil {
			result.Failed++
			continue
		}
		if skipped {
			result.Skipped++
		} else {
			result.Completed++
			result.IDs = append(result.IDs, item.id)
		}
	}
	// Finite run completion is independent of newly-due work. Do not claim
	// all cleanup done; another full100 snapshot may exist after this run.
	result.MorePossible = len(items) == 100 || result.Failed > 0
	if _, err := s.Pool.Exec(ctx, `UPDATE admin_cleanup_runs SET finished_at=clock_timestamp() WHERE id=$1
		AND NOT EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$1 AND outcome='pending')`, runID); err != nil {
		return result, err
	}
	if result.Failed > 0 {
		return result, ErrPartial
	}
	return result, nil
}

func (s *Service) cleanupSelection(ctx context.Context, kind string, dry bool) ([]cleanupItem, string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer rollback(tx)
	if err := normal(ctx, tx); err != nil {
		return nil, "", err
	}
	var runID string
	if !dry {
		// Converge older runs stranded after their last durable receipt. New
		// due work must not be hidden forever behind an exhausted snapshot.
		if _, err := tx.Exec(ctx, `UPDATE admin_cleanup_runs r SET finished_at=clock_timestamp()
			WHERE kind=$1 AND finished_at IS NULL AND NOT EXISTS
			(SELECT 1 FROM admin_cleanup_items i WHERE i.run_id=r.id AND i.outcome='pending')`, kind); err != nil {
			return nil, "", err
		}
		err = tx.QueryRow(ctx, `SELECT id::text FROM admin_cleanup_runs WHERE kind=$1 AND finished_at IS NULL FOR UPDATE`, kind).Scan(&runID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
	}
	selection := `SELECT id::text,id::text,NULL::text FROM media WHERE deleted_at IS NOT NULL AND purge_after<=clock_timestamp()
		ORDER BY purge_after,id LIMIT 100`
	if kind == "renditions" {
		selection = `SELECT r.id::text,r.media_id::text,NULL::text FROM renditions r
			WHERE NOT r.is_current AND r.purge_after<=clock_timestamp()
			AND EXISTS(SELECT 1 FROM job_targets candidate_target
			JOIN jobs candidate_job ON candidate_job.id=candidate_target.job_id AND candidate_job.type='transform' AND candidate_job.media_id_snapshot=r.media_id
			JOIN profiles candidate_profile ON candidate_profile.id=candidate_target.profile_id AND candidate_profile.key=r.profile_key
			JOIN renditions current_rendition ON current_rendition.media_id=r.media_id AND current_rendition.profile_key=r.profile_key AND current_rendition.is_current
			JOIN job_targets current_target ON current_target.id=current_rendition.job_target_id AND current_target.status='succeeded'
			JOIN jobs current_job ON current_job.id=current_target.job_id AND current_job.type='transform' AND current_job.media_id_snapshot=r.media_id
			JOIN profiles current_profile ON current_profile.id=current_target.profile_id AND current_profile.key=current_rendition.profile_key AND current_profile.version>=candidate_profile.version
			WHERE candidate_target.id=r.job_target_id AND candidate_target.status='succeeded')
			AND NOT EXISTS(SELECT 1 FROM jobs purge_job WHERE purge_job.type='purge' AND purge_job.media_id_snapshot=r.media_id AND purge_job.started_at IS NOT NULL) ORDER BY r.purge_after,r.media_id,r.id LIMIT 100`
	}
	var args []any
	if runID != "" {
		selection = `SELECT object_id::text,media_id::text,job_id::text FROM admin_cleanup_items WHERE run_id=$1 AND outcome='pending' ORDER BY media_id,object_id`
		args = []any{runID}
	}
	rows, err := tx.Query(ctx, selection, args...)
	if err != nil {
		return nil, "", err
	}
	var items []cleanupItem
	for rows.Next() {
		var item cleanupItem
		if err := rows.Scan(&item.id, &item.mediaID, &item.jobID); err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || dry || len(items) == 0 {
		return items, runID, err
	}
	if runID == "" {
		runID = newID()
		if _, err := tx.Exec(ctx, `INSERT INTO admin_cleanup_runs(id,kind) VALUES($1,$2)`, runID, kind); err != nil {
			return nil, "", err
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			if _, err := tx.Exec(ctx, `INSERT INTO admin_cleanup_items(run_id,object_id,media_id) VALUES($1,$2,$3)`, runID, item.id, item.mediaID); err != nil {
				return nil, "", err
			}
			ids = append(ids, item.id)
		}
		if err := s.audit(ctx, tx, "cleanup_begin", map[string]any{"run_id": runID, "kind": kind, "phase": "authorized"}, ids, "partial", time.Now()); err != nil {
			return nil, "", err
		}
	}
	return items, runID, tx.Commit(ctx)
}

func (s *Service) cleanupProof(ctx context.Context, kind string, item cleanupItem) (bool, error) {
	var done bool
	if kind == "media" {
		err := s.Pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM media WHERE id=$1) AND EXISTS(
			SELECT 1 FROM jobs j WHERE j.media_id_snapshot=$1 AND j.type='purge' AND j.status='succeeded'
			AND ($2::uuid IS NULL OR j.id=$2) AND j.started_at IS NOT NULL
			AND EXISTS(SELECT 1 FROM change_events e WHERE e.media_id=$1 AND e.event_type='media_purged' AND e.reason='physical_purge'))`, item.mediaID, item.jobID).Scan(&done)
		return done, err
	}
	err := s.Pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM renditions WHERE id=$1) AND
		(EXISTS(SELECT 1 FROM rendition_cleanup_progress WHERE rendition_id=$1 AND disposition IN ('deleted','missing'))
		OR EXISTS(SELECT 1 FROM purge_file_progress p JOIN jobs j ON j.id=p.job_id
		WHERE p.object_kind='rendition' AND p.object_id=$1 AND p.media_id_snapshot=$2
		AND p.disposition IN ('deleted','missing') AND j.type='purge' AND j.status='succeeded'))`, item.id, item.mediaID).Scan(&done)
	return done, err
}

func (s *Service) cleanupOne(ctx context.Context, lifecycle cleanupLifecycle, store *storage.Store, kind, runID string, item cleanupItem) (bool, bool, error) {
	if done, err := s.cleanupProof(ctx, kind, item); done || err != nil {
		return done, false, err
	}
	if kind == "renditions" {
		var eligible bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM renditions r WHERE r.id=$1 AND NOT r.is_current AND r.purge_after<=clock_timestamp()
			AND EXISTS(SELECT 1 FROM job_targets candidate_target
			JOIN jobs candidate_job ON candidate_job.id=candidate_target.job_id AND candidate_job.type='transform' AND candidate_job.media_id_snapshot=r.media_id
			JOIN profiles candidate_profile ON candidate_profile.id=candidate_target.profile_id AND candidate_profile.key=r.profile_key
			JOIN renditions current_rendition ON current_rendition.media_id=r.media_id AND current_rendition.profile_key=r.profile_key AND current_rendition.is_current
			JOIN job_targets current_target ON current_target.id=current_rendition.job_target_id AND current_target.status='succeeded'
			JOIN jobs current_job ON current_job.id=current_target.job_id AND current_job.type='transform' AND current_job.media_id_snapshot=r.media_id
			JOIN profiles current_profile ON current_profile.id=current_target.profile_id AND current_profile.key=current_rendition.profile_key AND current_profile.version>=candidate_profile.version
			WHERE candidate_target.id=r.job_target_id AND candidate_target.status='succeeded')
			AND NOT EXISTS(SELECT 1 FROM jobs purge_job WHERE purge_job.type='purge' AND purge_job.media_id_snapshot=r.media_id AND purge_job.started_at IS NOT NULL))`, item.id).Scan(&eligible); err != nil {
			return false, false, err
		}
		if !eligible {
			// Frozen membership is authorization, not permission to bypass the
			// native current/provenance/version recheck. Stale work terminates.
			return false, true, nil
		}
		_, err := lifecycle.CleanupNextRendition(ctx, item.id, nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
			key, err := storage.ParseRenditionKey(path)
			if err != nil || key.RenditionID().String() != id {
				return false, storage.ErrValidation
			}
			result, err := store.DeleteRendition(ctx, key, storage.DeleteExpectation{ExpectedSize: size})
			return result.Missing, err
		})
		if err != nil {
			return false, false, err
		}
		done, err := s.cleanupProof(ctx, kind, item)
		return done, false, err
	}
	var eligible bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM media WHERE id=$1 AND deleted_at IS NOT NULL AND purge_after<=clock_timestamp())`, item.id).Scan(&eligible); err != nil {
		return false, false, err
	}
	if !eligible {
		return false, true, nil
	}
	if item.jobID != nil {
		var cancelled bool
		if err := s.Pool.QueryRow(ctx, `SELECT status='cancelled' FROM jobs WHERE id=$1`, *item.jobID).Scan(&cancelled); err != nil {
			return false, false, err
		}
		if cancelled {
			// Restore revoked this exact authorization. Close it as skipped;
			// a later finite run must authorize any re-deleted Media afresh.
			return false, true, nil
		}
	}
	enqueued, err := lifecycle.EnqueueDuePurge(ctx, item.mediaID)
	if err != nil {
		return false, false, err
	}
	// Bind the exact job before its destructive first start. If the enqueue
	// response was lost, the same lifecycle enqueue rediscovers that job.
	if item.jobID != nil && *item.jobID != enqueued.Job.ID {
		return false, true, nil
	}
	bound, err := s.Pool.Exec(ctx, `UPDATE admin_cleanup_items SET job_id=$3 WHERE run_id=$1 AND object_id=$2
		AND outcome='pending' AND (job_id IS NULL OR job_id=$3)`, runID, item.id, enqueued.Job.ID)
	if err != nil {
		return false, false, err
	}
	if bound.RowsAffected() != 1 {
		return false, false, ErrConflict
	}
	if err := lifecycle.ReclaimExpiredPurge(ctx, enqueued.Job.ID); err != nil {
		return false, false, err
	}
	lease, err := lifecycle.StartPurge(ctx, enqueued.Job.ID)
	if err != nil {
		return false, false, err
	}
	for {
		if _, err := lifecycle.HeartbeatPurge(ctx, lease.JobID, lease.Token); err != nil {
			return false, false, err
		}
		step, err := lifecycle.RunPurgeFileStep(ctx, lease.JobID, lease.Token, medialifecycle.StoragePurgeFileAction(store))
		if err != nil {
			var unknown *medialifecycle.CommitOutcomeUnknown
			if !errors.As(err, &unknown) && !errors.Is(err, medialifecycle.ErrPurgeLeaseLost) && !medialifecycle.IsKind(err, medialifecycle.KindDatabaseUnavailable) {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = lifecycle.FinishPurgeAttempt(releaseCtx, lease.JobID, lease.Token, "process_failed")
				cancel()
			}
			return false, false, err
		}
		if step.Done {
			err := lifecycle.FinalizePurge(ctx, lease.JobID, lease.Token)
			return err == nil, false, err
		}
	}
}

func (s *Service) cleanupReceipt(ctx context.Context, kind, runID string, item cleanupItem, outcome string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `UPDATE admin_cleanup_items SET outcome=$3,completed_at=clock_timestamp() WHERE run_id=$1 AND object_id=$2 AND outcome='pending'`, runID, item.id, outcome); err != nil {
		return err
	}
	if err := s.audit(ctx, tx, "cleanup_result", map[string]any{"run_id": runID, "kind": kind, "outcome": outcome, "native_journal": true}, []string{item.id}, "succeeded", time.Now()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_cleanup_runs SET finished_at=clock_timestamp() WHERE id=$1
		AND NOT EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$1 AND outcome='pending')`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
