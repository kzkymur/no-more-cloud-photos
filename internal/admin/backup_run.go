package admin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
)

func (s *Service) Backup(ctx context.Context, b BackupSettings, ifDue bool) (result BackupRun, err error) {
	result = BackupRun{Outcome: "failed"}
	// One pinned lock session plus an independent failure journal connection
	// are mandatory. Reject a one-slot pool before any history or lock mutation.
	if s.Pool == nil || s.Pool.Config().MaxConns < 2 {
		return result, ErrValidation
	}
	// This precise acquired session must still own the lock at the final commit.
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
		result.Outcome = "overlap_conflict"
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
	// Both locks are required before recovery or history mutation. A surviving
	// dumper with a lost DB backend must never be declared abandoned by a rival.
	authority := newBackupAuthority(ctx, conn)
	defer authority.cancel()
	store, err := backupartifact.Open(b.Root, backupartifact.Options{Lock: true, BeforePublish: authority.Check})
	if err != nil {
		if errors.Is(err, backupartifact.ErrLocked) {
			result.Outcome = "overlap_conflict"
			return result, ErrConflict
		}
		return result, errBackup
	}
	defer store.Close()
	// A previous holder cannot still be running after we obtain the same lock.
	abandoned, err := s.Pool.Query(ctx, `SELECT id::text FROM backup_runs WHERE status='running' OR (status='failed' AND (notification_status='pending' OR failure_audit_pending)) ORDER BY started_at`)
	if err != nil {
		return result, errBackup
	}
	ids := []string{}
	for abandoned.Next() {
		var id string
		if abandoned.Scan(&id) != nil {
			abandoned.Close()
			return result, errBackup
		}
		ids = append(ids, id)
	}
	err = abandoned.Err()
	abandoned.Close()
	if err != nil {
		return result, errBackup
	}
	for _, id := range ids {
		var status string
		if s.Pool.QueryRow(ctx, `SELECT status FROM backup_runs WHERE id=$1`, id).Scan(&status) != nil {
			return result, errBackup
		}
		if status == "running" {
			_ = s.backupFailure(id, "abandoned", b)
		} else {
			_ = s.backupNotify(ctx, id, "abandoned", b)
		}
		_ = s.retryBackupFailureAudit(ctx, id)
		if s.Pool.QueryRow(ctx, `SELECT status FROM backup_runs WHERE id=$1`, id).Scan(&status) != nil || status == "running" {
			return result, errBackup
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, errBackup
	}
	defer rollback(tx)
	if err = backupNormal(ctx, tx); err != nil {
		return result, err
	}
	cfg, cutoff, due, err := backupDue(ctx, tx, ifDue)
	if err != nil {
		return result, errBackup
	}
	if !due {
		result.Outcome = "not_due"
		return result, ErrConflict
	}
	result = BackupRun{ID: newID(), Status: "running", StartedAt: cutoff, Notification: "pending", Outcome: "started"}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return result, errBackup
	}
	_, err = tx.Exec(ctx, `INSERT INTO backup_runs(id,status,started_at,config_snapshot) VALUES($1,'running',$2,$3)`, result.ID, cutoff, configJSON)
	if err != nil {
		return result, errBackup
	}
	phase := "running_commit"
	defer func() {
		if err != nil {
			if s.backupFailure(result.ID, phase, b) == nil {
				// A commit acknowledgement was lost, but the committed success is real.
				result.Status = "succeeded"
				result.Outcome = "succeeded"
				err = nil
			} else {
				result.Status = "failed"
				result.Outcome = "failed"
				err = errBackup
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if persisted, readErr := s.backupResult(cleanup, result.ID); readErr == nil {
				result = persisted
			}
			cancel()
		}
	}()

	err = tx.Commit(ctx)
	if err == nil && s.afterBackupRunningCommit != nil {
		err = s.afterBackupRunningCommit(ctx)
	}
	if err != nil {
		rollback(tx)
		return result, errBackup
	}
	phase = "prepare"
	dump, err := b.dumpConnect(ctx)
	if err != nil {
		return result, errBackup
	}
	defer dump.Close(context.Background())
	sourceIdentity, identityErr := backupIdentity(ctx, conn)
	dumpIdentity, dumpIdentityErr := backupIdentity(ctx, dump)
	if identityErr != nil || dumpIdentityErr != nil || sourceIdentity != dumpIdentity {
		return result, errBackup
	}
	// Main stops using the pinned session until Stop joins this monitor.
	authority.Start()
	defer func() { _ = authority.Stop(); authority.cancel() }()
	ctx = authority.ctx
	if err = authority.Check(ctx); err != nil {
		return result, errBackup
	}
	server, err := backupInventory(ctx, dump)
	if err != nil {
		return result, errBackup
	}
	tool, err := b.versions(ctx, server)
	if err != nil {
		return result, errBackup
	}
	var restoreVersion limitedOutput
	if err = b.tool(ctx, b.RestoreTool, []string{"--version"}, &restoreVersion); err != nil || toolMajor(string(restoreVersion)) != toolMajor(tool) {
		return result, errBackup
	}
	snapshotTx, err := dump.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, errBackup
	}
	defer rollback(snapshotTx)
	var snapshot string
	if err = snapshotTx.QueryRow(ctx, `SELECT pg_export_snapshot(),transaction_timestamp()`).Scan(&snapshot, &cutoff); err != nil {
		return result, errBackup
	}
	result.StartedAt = cutoff
	var snapshotConfig []byte
	if err = snapshotTx.QueryRow(ctx, `SELECT to_jsonb(system_config)-'id' FROM system_config WHERE id=1`).Scan(&snapshotConfig); err != nil {
		return result, errBackup
	}
	configJSON = snapshotConfig
	phase = "dump"
	manifest, err := store.Publish(ctx, backupartifact.Metadata{ID: result.ID, SnapshotCutoff: cutoff, Config: configJSON, ServerVersion: server, ToolVersions: map[string]string{"pg_dump": tool, "pg_restore": string(restoreVersion)}},
		func(ctx context.Context, f *os.File) error {
			return b.tool(ctx, b.DumpTool, []string{"--no-password", "--format=custom", "--snapshot=" + snapshot}, f)
		},
		func(ctx context.Context, f *os.File) error {
			cmd := exec.CommandContext(ctx, b.RestoreTool, "--list", "/proc/self/fd/3")
			cmd.ExtraFiles = []*os.File{f}
			cmd.Env = b.toolEnv()
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			cmd.WaitDelay = 5 * time.Second
			if cmd.Run() != nil {
				return errBackup
			}
			return nil
		})
	if err != nil {
		var uncertain *backupartifact.PublicationError
		if errors.As(err, &uncertain) {
			phase = "publication_uncertain"
		}
		return result, errBackup
	}
	if err = authority.Stop(); err != nil {
		phase = "authority_lost_after_publish"
		return result, errBackup
	}
	phase = "history"
	// Beginning on the pinned lock connection detects lock/session loss. The
	// maintenance SHARE lock is held through success/audit commit.
	final, err := conn.Begin(ctx)
	if err != nil {
		return result, errBackup
	}
	defer rollback(final)
	if err = backupNormal(ctx, final); err != nil {
		return result, errBackup
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return result, errBackup
	}
	tag, err := final.Exec(ctx, `UPDATE backup_runs SET status='succeeded',finished_at=clock_timestamp(),final_relative_path=$2,size_bytes=$3,sha256=$4,manifest=$5,postgres_version=$6,tool_version=$7,started_at=$8,config_snapshot=$9 WHERE id=$1 AND status='running'`, result.ID, result.ID, manifest.DumpSize, manifest.DumpSHA256, encoded, server, tool, cutoff, configJSON)
	if err != nil || tag.RowsAffected() != 1 {
		return result, errBackup
	}
	if err = s.audit(ctx, final, "backup_run", map[string]any{"if_due": ifDue}, []string{result.ID}, "succeeded", cutoff); err != nil {
		return result, errBackup
	}
	err = final.Commit(ctx)
	if err == nil && s.afterBackupCommit != nil {
		err = s.afterBackupCommit(ctx)
	}
	if err != nil {
		return result, errBackup
	}
	result.Status = "succeeded"
	result.Outcome = "succeeded"
	result.Notification = "pending"
	return result, nil
}
