package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
)

type BackupCleanupResult struct {
	Deleted           []string `json:"deleted"`
	Eligible          []string `json:"eligible"`
	SkippedIDs        []string `json:"skipped_ids"`
	DryRun            bool     `json:"dry_run"`
	StagingReconciled []string `json:"staging_reconciled"`
	StagingEligible   []string `json:"staging_eligible"`
}

// CleanupBackups journals deletion intent before touching bytes. Retrying a
// committed intent uses the artifact's reserved .deleting namespace, never a
// wildcard or an unvalidated temp directory. Successful run history is retained.
func (s *Service) CleanupBackups(ctx context.Context, b BackupSettings, dry bool) (BackupCleanupResult, error) {
	result := BackupCleanupResult{Deleted: []string{}, Eligible: []string{}, DryRun: dry, SkippedIDs: []string{}}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, errBackup
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('backup',1818))`).Scan(&locked); err != nil {
		return result, errBackup
	}
	if !locked {
		return result, ErrConflict
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if conn.QueryRow(cleanup, `SELECT pg_advisory_unlock(hashtextextended('backup',1818))`).Scan(&unlocked) != nil || !unlocked {
			_ = conn.Hijack().Close(cleanup)
		}
	}()

	opts, err := backupArtifactOptions(b.Root, false)
	if err != nil {
		return result, err
	}
	store, err := backupartifact.Open(b.Root, opts)
	if err != nil {
		if errors.Is(err, backupartifact.ErrLocked) {
			return result, ErrConflict
		}
		return result, errBackup
	}
	defer store.Close()
	// A short transaction captures victims with the current retention. The
	// operation lock prevents publishing a new success while they are selected.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, errBackup
	}
	defer rollback(tx)
	if err = backupNormal(ctx, tx); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT b.id::text,(b.retention_deleting_at IS NOT NULL) FROM backup_runs b,system_config c
 WHERE c.id=1 AND b.status='succeeded' AND b.retention_deleted_at IS NULL
 AND b.id<>(SELECT id FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1)
 AND b.started_at<clock_timestamp()-make_interval(days=>c.db_backup_retention_days)
 ORDER BY b.started_at,b.id`)
	if err != nil {
		return result, errBackup
	}
	type victim struct {
		id       string
		resuming bool
	}
	victims := []victim{}
	for rows.Next() {
		var v victim
		if rows.Scan(&v.id, &v.resuming) != nil {
			rows.Close()
			return result, errBackup
		}
		victims = append(victims, v)
		result.Eligible = append(result.Eligible, v.id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, errBackup
	}
	if err = tx.Commit(ctx); err != nil {
		return result, errBackup
	}
	if s.afterBackupCleanupSelection != nil {
		if err = s.afterBackupCleanupSelection(ctx); err != nil {
			return result, err
		}
	}
	// A history row is not proof of a usable keeper. Never delete an older
	// verified artifact when the newest recorded success is missing/corrupt.
	var keeper string
	if len(victims) > 0 {
		var hash string
		var size int64
		var raw []byte
		if err = conn.QueryRow(ctx, `SELECT id::text,sha256,size_bytes,manifest FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1`).Scan(&keeper, &hash, &size, &raw); err != nil {
			return result, errBackup
		}
		m, verifyErr := store.Verify(keeper)
		encoded, _ := json.Marshal(m)
		if verifyErr != nil || m.ID != keeper || m.DumpSHA256 != hash || m.DumpSize != size || !backupJSONEqual(encoded, raw) {
			return result, errBackup
		}
	}
	for _, v := range victims {
		if !v.resuming {
			if _, err = store.Verify(v.id); err != nil {
				return result, errBackup
			}
		}
		if dry {
			continue
		}
		// Intent is durable crash recovery metadata, not permission to ignore a
		// later retention extension. Serialize both decisions with ConfigSet.
		intent, err := conn.Begin(ctx)
		if err != nil {
			return result, errBackup
		}
		if err = backupNormal(ctx, intent); err != nil {
			rollback(intent)
			return result, err
		}
		change, err := intent.Exec(ctx, `UPDATE backup_runs b SET retention_deleting_at=COALESCE(b.retention_deleting_at,clock_timestamp()) FROM system_config c WHERE c.id=1 AND b.id=$1 AND b.status='succeeded' AND b.retention_deleted_at IS NULL AND b.started_at<clock_timestamp()-make_interval(days=>c.db_backup_retention_days) AND b.id<>(SELECT id FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1)`, v.id)
		if err != nil {
			rollback(intent)
			return result, errBackup
		}
		if change.RowsAffected() != 1 {
			rollback(intent)
			result.SkippedIDs = append(result.SkippedIDs, v.id)
			continue
		}
		if err = intent.Commit(ctx); err != nil {
			return result, errBackup
		}
		fence, err := conn.Begin(ctx)
		if err != nil {
			return result, errBackup
		}
		if err = backupNormal(ctx, fence); err != nil {
			rollback(fence)
			return result, err
		}
		var eligible bool
		if err = fence.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backup_runs b,system_config c WHERE c.id=1 AND b.id=$1 AND b.status='succeeded' AND b.retention_deleted_at IS NULL AND b.started_at<clock_timestamp()-make_interval(days=>c.db_backup_retention_days) AND b.id<>(SELECT id FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1))`, v.id).Scan(&eligible); err != nil {
			rollback(fence)
			return result, errBackup
		}
		if !eligible {
			rollback(fence)
			result.SkippedIDs = append(result.SkippedIDs, v.id)
			continue
		}
		// The closed fence holds config SHARE through byte removal and audit
		// commit. An actual ConfigSet cannot change the policy mid-delete.
		if s.beforeBackupArtifactDelete != nil {
			if err = s.beforeBackupArtifactDelete(ctx); err != nil {
				rollback(fence)
				return result, err
			}
		}
		if err = store.ResumeDelete(v.id); err != nil {
			rollback(fence)
			return result, errBackup
		}
		_, err = fence.Exec(ctx, `UPDATE backup_runs SET retention_deleting_at=NULL,retention_deleted_at=clock_timestamp() WHERE id=$1`, v.id)
		if err == nil {
			err = s.audit(ctx, fence, "cleanup_backups", map[string]any{"backup_id": v.id}, []string{v.id}, "succeeded", time.Now())
		}
		if err != nil {
			rollback(fence)
			return result, errBackup
		}
		if err = fence.Commit(ctx); err != nil {
			return result, errBackup
		}
		result.Deleted = append(result.Deleted, v.id)
	}
	// Failed staging has its own fixed 48-hour safety window, unrelated to
	// successful-backup retention. No root listing, unknown-name deletion, or
	// published uncertain artifact reclamation is performed here.
	staged, err := conn.Query(ctx, `SELECT id::text FROM backup_runs WHERE status='failed' AND finished_at<clock_timestamp()-interval '48 hours' AND staging_cleanup_completed_at IS NULL ORDER BY finished_at,id LIMIT 100`)
	if err != nil {
		return result, errBackup
	}
	for staged.Next() {
		var id string
		if staged.Scan(&id) != nil {
			staged.Close()
			return result, errBackup
		}
		result.StagingEligible = append(result.StagingEligible, id)
	}
	err = staged.Err()
	staged.Close()
	if err != nil {
		return result, errBackup
	}
	if !dry {
		for _, id := range result.StagingEligible {
			fence, err := conn.Begin(ctx)
			if err != nil {
				return result, errBackup
			}
			if err = backupNormal(ctx, fence); err != nil {
				rollback(fence)
				return result, err
			}
			if err = store.RemoveStaging(id); err != nil {
				rollback(fence)
				return result, errBackup
			}
			_, err = fence.Exec(ctx, `UPDATE backup_runs SET staging_cleanup_completed_at=clock_timestamp() WHERE id=$1 AND status='failed' AND finished_at<clock_timestamp()-interval '48 hours'`, id)
			if err == nil {
				err = s.audit(ctx, fence, "cleanup_backup_staging", map[string]string{"backup_id": id}, []string{id}, "succeeded", time.Now())
			}
			if err != nil {
				rollback(fence)
				return result, errBackup
			}
			if err = fence.Commit(ctx); err != nil {
				return result, errBackup
			}
			result.StagingReconciled = append(result.StagingReconciled, id)
		}
	}
	return result, nil
}

func backupJSONEqual(a, b []byte) bool {
	var first, second any
	if json.Unmarshal(a, &first) != nil || json.Unmarshal(b, &second) != nil {
		return false
	}
	ac, ae := json.Marshal(first)
	bc, be := json.Marshal(second)
	return ae == nil && be == nil && bytes.Equal(ac, bc)
}
