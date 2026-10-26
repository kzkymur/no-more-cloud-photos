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

type cleanupItem struct {
	id      string
	mediaID string
	jobID   *string
}

// Match the native preferred-ID admission; the native transaction still owns
// authoritative Media-first locking and revalidation before any unlink.
const cleanupRenditionCandidates = `SELECT r.id::text,r.media_id::text,NULL::text
	FROM renditions r
	JOIN job_targets ct ON ct.id=r.job_target_id AND ct.status='succeeded'
	JOIN jobs cj ON cj.id=ct.job_id AND cj.type='transform' AND cj.media_id_snapshot=r.media_id
	JOIN profiles cp ON cp.id=ct.profile_id AND cp.key=r.profile_key
	JOIN renditions n ON n.media_id=r.media_id AND n.profile_key=r.profile_key AND n.is_current
	JOIN job_targets nt ON nt.id=n.job_target_id AND nt.status='succeeded'
	JOIN jobs nj ON nj.id=nt.job_id AND nj.type='transform' AND nj.media_id_snapshot=r.media_id
	JOIN profiles np ON np.id=nt.profile_id AND np.key=n.profile_key AND np.version>=cp.version
	WHERE NOT r.is_current AND r.purge_after<=clock_timestamp()
	AND NOT EXISTS(SELECT 1 FROM jobs j WHERE j.type='purge' AND j.media_id_snapshot=r.media_id AND j.started_at IS NOT NULL)`

func (s *Service) cleanupRenditionEligible(ctx context.Context, id string) (bool, error) {
	var eligible bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(`+cleanupRenditionCandidates+` AND r.id=$1)`, id).Scan(&eligible)
	return eligible, err
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
	if _, err := s.Pool.Exec(ctx, `UPDATE admin_cleanup_runs SET finished_at=clock_timestamp() WHERE id=$1 AND finished_at IS NULL
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
		// Recover legacy response-loss/crash state before taking a fresh
		// snapshot. An exhausted authorization must never block future work.
		if _, err := tx.Exec(ctx, `UPDATE admin_cleanup_runs r SET finished_at=clock_timestamp()
			WHERE r.kind=$1 AND r.finished_at IS NULL AND NOT EXISTS
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
		selection = cleanupRenditionCandidates + ` ORDER BY r.purge_after,r.media_id,r.id LIMIT 100`
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
	if err != nil || dry {
		return items, runID, err
	}
	if len(items) == 0 {
		return items, runID, tx.Commit(ctx)
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

func (s *Service) cleanupOne(ctx context.Context, lifecycle *medialifecycle.Service, store *storage.Store, kind, runID string, item cleanupItem) (bool, bool, error) {
	if done, err := s.cleanupProof(ctx, kind, item); done || err != nil {
		return done, false, err
	}
	if kind == "renditions" {
		eligible, err := s.cleanupRenditionEligible(ctx, item.id)
		if err != nil || !eligible {
			return false, !eligible && err == nil, err
		}
		_, err = lifecycle.CleanupNextRendition(ctx, item.id, nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
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
		if done || err != nil {
			return done, false, err
		}
		// A native recheck may legitimately decline a stale frozen item.
		// Close that authorization without claiming an effect or deleting it.
		eligible, err = s.cleanupRenditionEligible(ctx, item.id)
		return false, !eligible && err == nil, err
	}
	// A bound authorization may execute only that exact native job. Restore
	// can cancel an unstarted J1; a later J2 needs a new cleanup_begin run.
	if item.jobID != nil {
		var cancelled bool
		if err := s.Pool.QueryRow(ctx, `SELECT status='cancelled' AND started_at IS NULL
			FROM jobs WHERE id=$1 AND type='purge' AND media_id_snapshot=$2`, *item.jobID, item.mediaID).Scan(&cancelled); err != nil {
			return false, false, err
		}
		if cancelled {
			return false, true, nil
		}
	}
	var eligible bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM media WHERE id=$1 AND deleted_at IS NOT NULL AND purge_after<=clock_timestamp())`, item.id).Scan(&eligible); err != nil {
		return false, false, err
	}
	if !eligible {
		return false, true, nil
	}
	if item.jobID == nil {
		enqueued, err := lifecycle.EnqueueDuePurge(ctx, item.mediaID)
		if err != nil {
			return false, false, err
		}
		// Read back the authoritative immutable binding, not the speculative
		// enqueue response or a zero-row UPDATE result.
		var jobID string
		if err := s.Pool.QueryRow(ctx, `UPDATE admin_cleanup_items SET job_id=COALESCE(job_id,$3)
			WHERE run_id=$1 AND object_id=$2 AND outcome='pending' RETURNING job_id::text`, runID, item.id, enqueued.Job.ID).Scan(&jobID); err != nil {
			return false, false, err
		}
		item.jobID = &jobID
	}
	lease, err := lifecycle.StartPurge(ctx, *item.jobID)
	if err != nil {
		return false, false, err
	}
	for {
		if _, err := lifecycle.HeartbeatPurge(ctx, lease.JobID, lease.Token); err != nil {
			return false, false, err
		}
		step, err := lifecycle.RunPurgeFileStep(ctx, lease.JobID, lease.Token, medialifecycle.StoragePurgeFileAction(store))
		if err != nil {
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
	receipt, err := tx.Exec(ctx, `UPDATE admin_cleanup_items SET outcome=$3,completed_at=clock_timestamp() WHERE run_id=$1 AND object_id=$2 AND outcome='pending'`, runID, item.id, outcome)
	if err != nil {
		return err
	}
	if receipt.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if err := s.audit(ctx, tx, "cleanup_result", map[string]any{"run_id": runID, "kind": kind, "outcome": outcome, "native_journal": true}, []string{item.id}, "succeeded", time.Now()); err != nil {
		return err
	}
	// Last receipt, its audit and run completion are one crash boundary.
	if _, err := tx.Exec(ctx, `UPDATE admin_cleanup_runs SET finished_at=clock_timestamp()
		WHERE id=$1 AND finished_at IS NULL AND NOT EXISTS
		(SELECT 1 FROM admin_cleanup_items WHERE run_id=$1 AND outcome='pending')`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
