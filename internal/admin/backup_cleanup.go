package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
)

type BackupCleanupResult struct {
	Deleted  []string `json:"deleted"`
	Eligible []string `json:"eligible"`
	DryRun   bool     `json:"dry_run"`
}

// CleanupBackups journals deletion intent before touching bytes. Retrying a
// committed intent uses the artifact's reserved .deleting namespace, never a
// wildcard or an unvalidated temp directory. Successful run history is retained.
func (s *Service) CleanupBackups(ctx context.Context, b BackupSettings, dry bool) (BackupCleanupResult, error) {
	result := BackupCleanupResult{Deleted: []string{}, Eligible: []string{}, DryRun: dry}
	release, err := s.sessionLock(ctx, "backup")
	if err != nil {
		return result, err
	}
	defer release()
	store, err := backupartifact.Open(b.Root, backupartifact.Options{})
	if err != nil {
		return result, errBackup
	}
	defer store.Close()
	// A short transaction captures victims with the current retention. The
	// operation lock prevents publishing a new success while they are selected.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return result, errBackup
	}
	defer rollback(tx)
	if err = normal(ctx, tx); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT b.id::text,(b.manifest ? 'retention_deleting') FROM backup_runs b,system_config c
 WHERE c.id=1 AND b.status='succeeded' AND NOT(b.manifest ? 'retention_deleted_at')
 AND b.id<>(SELECT id FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1)
 AND (b.started_at<clock_timestamp()-make_interval(days=>c.db_backup_retention_days) OR b.manifest ? 'retention_deleting')
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
	for _, v := range victims {
		if !v.resuming {
			if _, err = store.Verify(v.id); err != nil {
				return result, errBackup
			}
		}
		if dry {
			continue
		}
		// Hold the maintenance fence and newest-success recheck through filesystem
		// removal and final audit. Intent commits independently to recover crashes.
		intent, err := s.Pool.Exec(ctx, `UPDATE backup_runs SET manifest=jsonb_set(manifest,'{retention_deleting}',to_jsonb(clock_timestamp())) WHERE id=$1 AND status='succeeded' AND NOT(manifest ? 'retention_deleted_at') AND id<>(SELECT id FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1)`, v.id)
		if err != nil || intent.RowsAffected() != 1 {
			return result, errBackup
		}
		fence, err := s.Pool.Begin(ctx)
		if err != nil {
			return result, errBackup
		}
		if err = normal(ctx, fence); err != nil {
			rollback(fence)
			return result, err
		}
		var newest string
		if err = fence.QueryRow(ctx, `SELECT id::text FROM backup_runs WHERE status='succeeded' ORDER BY started_at DESC,id DESC LIMIT 1 FOR SHARE`).Scan(&newest); err != nil || newest == v.id {
			rollback(fence)
			return result, ErrConflict
		}
		if err = store.ResumeDelete(v.id); err != nil {
			rollback(fence)
			return result, errBackup
		}
		_, err = fence.Exec(ctx, `UPDATE backup_runs SET manifest=jsonb_set(manifest-'retention_deleting','{retention_deleted_at}',to_jsonb(clock_timestamp())) WHERE id=$1`, v.id)
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
	return result, nil
}

// isBackupDeleted is kept separate from artifact manifests: retention metadata
// belongs to control history, never changes an immutable on-disk manifest.
func isBackupDeleted(raw json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m["retention_deleted_at"]
	return ok
}
