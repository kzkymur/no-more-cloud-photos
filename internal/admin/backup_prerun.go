package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Issue #57: a backup that fails before its backup_runs row exists (the
// database is unreachable, the schema is not ready, the artifact store cannot
// be opened, ...) has no history row to journal and no backup ID, but it is a
// failed backup all the same. It must reach the operator's notification hook
// exactly once. The cause is one of these closed classes; nothing else (no
// error text, path, DSN or credential) ever leaves the process.
const (
	backupCauseDBUnreachable = "db_unreachable"
	backupCauseSchema        = "schema_not_ready"
	backupCauseConfiguration = "configuration"
	backupCauseArtifactStore = "artifact_store"
	backupCauseRecovery      = "recovery"
	backupCauseMaintenance   = "maintenance"
	backupCauseHistory       = "history"
)

// backupPreRunError marks a failure before any history row exists. It keeps
// the underlying sentinel (errBackup, ErrValidation, ErrConflict) for the exit
// code.
type backupPreRunError struct {
	cause string
	err   error
	// key identifies the condition instance for once-only causes: for
	// maintenance, that maintenance's entered_at.
	key string
}

func (e *backupPreRunError) Error() string {
	return "backup failed before a history row existed (" + e.cause + ")"
}

func (e *backupPreRunError) Unwrap() error { return e.err }

func backupPreRun(cause string, err error) error {
	if err == nil {
		err = errBackup
	}
	return &backupPreRunError{cause: cause, err: err}
}

// backupPreRunCause returns the closed cause of a pre-history failure.
func backupPreRunCause(err error) (string, bool) {
	cause, _, ok := backupPreRunDetail(err)
	return cause, ok
}

func backupPreRunDetail(err error) (cause, key string, ok bool) {
	var pre *backupPreRunError
	if errors.As(err, &pre) {
		return pre.cause, pre.key, true
	}
	return "", "", false
}

// maintenanceNotifiedMarker records, in the backup root (the only path the
// backup unit may write), the entered_at of the maintenance whose refusal was
// already notified. Nothing else reads the backup root by name.
const maintenanceNotifiedMarker = ".maintenance-notified"

// notifyBackupPreRunOnce is notifyBackupPreRun, except that a `maintenance`
// refusal is notified once per maintenance entry (keyed on its entered_at),
// not on every timer wake: later wakes in the same maintenance only log. The
// marker is written only after a successful notification, so a failed hook
// is retried on the next wake.
func notifyBackupPreRunOnce(ctx context.Context, b BackupSettings, cause, key string, now time.Time, stderr io.Writer) string {
	if cause != backupCauseMaintenance || key == "" || !filepath.IsAbs(b.Root) {
		return notifyBackupPreRun(ctx, b.NotifyHook, cause, now, stderr)
	}
	marker := filepath.Join(b.Root, maintenanceNotifiedMarker)
	if previous, err := os.ReadFile(marker); err == nil && string(previous) == key {
		_ = json.NewEncoder(stderr).Encode(map[string]string{"level": "ERROR", "message": "backup failed before a history row existed",
			"cause": cause, "notification": "already_sent"})
		return "already_sent"
	}
	notification := notifyBackupPreRun(ctx, b.NotifyHook, cause, now, stderr)
	if notification == "succeeded" {
		tmp := marker + ".tmp"
		if os.WriteFile(tmp, []byte(key), 0o600) == nil {
			_ = os.Rename(tmp, marker)
		} else {
			_ = os.Remove(tmp)
		}
	}
	return notification
}

// backupPreRunTimeout bounds the hook exactly like the post-history hook.
const backupPreRunTimeout = 10 * time.Second

// notifyBackupPreRun invokes the configured hook once for a pre-history
// failure and logs one closed JSON line to stderr. The hook contract is the
// post-history one (fixed absolute executable, no argv, PATH/LANG only,
// 10-second deadline, output discarded), with this stdin document:
//
//	{"status":"failed","phase":"precondition","cause":"<class>","at":"<RFC3339 UTC>"}
//
// It has no backup_id: none exists. The result is reported, never retried
// (there is no history row to retry from); a missing or failing hook is
// logged as notification "failed" / "not_configured".
func notifyBackupPreRun(ctx context.Context, hook, cause string, now time.Time, stderr io.Writer) string {
	notification := "not_configured"
	if filepath.IsAbs(hook) {
		notification = "failed"
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupPreRunTimeout)
		defer cancel()
		cmd := exec.CommandContext(hookCtx, hook)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
		metadata, _ := json.Marshal(map[string]string{"status": "failed", "phase": "precondition", "cause": cause, "at": now.UTC().Format(time.RFC3339)})
		cmd.Stdin = strings.NewReader(string(metadata))
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		if cmd.Run() == nil {
			notification = "succeeded"
		}
	}
	_ = json.NewEncoder(stderr).Encode(map[string]string{"level": "ERROR", "message": "backup failed before a history row existed",
		"cause": cause, "notification": notification})
	return notification
}
