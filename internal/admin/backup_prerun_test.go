package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prerunHook writes a hook that records every invocation (stdin, argv count
// and environment) as one JSON line, so "exactly once" and the closed
// contract are observable.
func prerunHook(t *testing.T, exitCode int) (hook, log string) {
	t.Helper()
	dir := t.TempDir()
	hook, log = filepath.Join(dir, "notify"), filepath.Join(dir, "calls.jsonl")
	script := "#!/bin/sh\nstdin=$(cat)\nprintf '{\"argc\":%d,\"env\":\"%s\",\"stdin\":%s}\\n' \"$#\" \"$(env | sort | tr '\\n' ';')\" \"$stdin\" >> " + log + "\nexit " + string(rune('0'+exitCode)) + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return hook, log
}

type prerunCall struct {
	Argc  int               `json:"argc"`
	Env   string            `json:"env"`
	Stdin map[string]string `json:"stdin"`
}

func prerunCalls(t *testing.T, log string) []prerunCall {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []prerunCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var call prerunCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("hook record %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}

// TestBackupPreRunNotificationContract pins issue #57's hook contract.
func TestBackupPreRunNotificationContract(t *testing.T) {
	hook, log := prerunHook(t, 0)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.FixedZone("x", 9*3600))
	var stderr bytes.Buffer
	if got := notifyBackupPreRun(context.Background(), hook, backupCauseDBUnreachable, at, &stderr); got != "succeeded" {
		t.Fatalf("notification = %q", got)
	}
	calls := prerunCalls(t, log)
	if len(calls) != 1 || calls[0].Argc != 0 {
		t.Fatalf("hook calls = %+v", calls)
	}
	want := map[string]string{"status": "failed", "phase": "precondition", "cause": "db_unreachable", "at": "2026-11-12T00:30:00Z"}
	if len(calls[0].Stdin) != len(want) {
		t.Fatalf("stdin = %v, want exactly %v (no backup_id)", calls[0].Stdin, want)
	}
	for key, value := range want {
		if calls[0].Stdin[key] != value {
			t.Fatalf("stdin = %v, want %v", calls[0].Stdin, want)
		}
	}
	for _, entry := range strings.Split(strings.TrimSuffix(calls[0].Env, ";"), ";") {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "LANG", "PWD", "SHLVL", "_":
		default:
			t.Fatalf("hook inherited %q (env %q)", name, calls[0].Env)
		}
	}
	var logged map[string]string
	if err := json.Unmarshal(stderr.Bytes(), &logged); err != nil || logged["cause"] != "db_unreachable" || logged["notification"] != "succeeded" || logged["level"] != "ERROR" {
		t.Fatalf("stderr log = %q", stderr.String())
	}
	failing, _ := prerunHook(t, 9)
	if got := notifyBackupPreRun(context.Background(), failing, backupCauseSchema, at, &bytes.Buffer{}); got != "failed" {
		t.Fatalf("failing hook notification = %q", got)
	}
	if got := notifyBackupPreRun(context.Background(), "", backupCauseSchema, at, &bytes.Buffer{}); got != "not_configured" {
		t.Fatalf("absent hook notification = %q", got)
	}
}

// TestBackupRunCLINotifiesUnreachableDatabaseOnce: with PostgreSQL down, the
// CLI fails before any history row and the hook runs exactly once with the
// closed db_unreachable cause; the exit code stays non-zero and no secret
// reaches the hook or the logs.
func TestBackupRunCLINotifiesUnreachableDatabaseOnce(t *testing.T) {
	hook, log := prerunHook(t, 0)
	const secret = "never-print-this-password"
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("NMCP_ADMIN_DATABASE_URL_FILE", "")
	t.Setenv("NMCP_DATABASE_URL", "postgres://nmcp_backup_control:"+secret+"@127.0.0.1:1/nmcp?sslmode=disable&connect_timeout=2")
	t.Setenv("NMCP_BACKUP_NOTIFY_HOOK", hook)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"backup", "run", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("backup with an unreachable database exited 0")
	}
	calls := prerunCalls(t, log)
	if len(calls) != 1 || calls[0].Stdin["cause"] != "db_unreachable" || calls[0].Stdin["phase"] != "precondition" {
		t.Fatalf("hook calls = %+v", calls)
	}
	if _, present := calls[0].Stdin["backup_id"]; present {
		t.Fatal("pre-history notification carried a backup_id")
	}
	raw, _ := os.ReadFile(log)
	for name, text := range map[string]string{"hook": string(raw), "stdout": stdout.String(), "stderr": stderr.String()} {
		if strings.Contains(text, secret) || strings.Contains(text, "127.0.0.1") {
			t.Fatalf("%s leaked the connection string: %q", name, text)
		}
	}
	if !strings.Contains(stderr.String(), `"cause":"db_unreachable"`) {
		t.Fatalf("stderr does not log the cause: %q", stderr.String())
	}
}

// TestBackupMaintenanceRefusalNotifiesOncePerEntry pins the leader's #57
// decision: every hourly wake during one maintenance is refused, but only the
// first notifies; a new maintenance entry notifies again, and a failed hook
// is retried on the next wake.
func TestBackupMaintenanceRefusalNotifiesOncePerEntry(t *testing.T) {
	hook, log := prerunHook(t, 0)
	b := BackupSettings{Root: t.TempDir(), NotifyHook: hook}
	now := time.Now()
	wake := func(key string) string {
		var stderr bytes.Buffer
		got := notifyBackupPreRunOnce(context.Background(), b, backupCauseMaintenance, key, now, &stderr)
		if !strings.Contains(stderr.String(), `"cause":"maintenance"`) {
			t.Fatalf("wake not logged: %q", stderr.String())
		}
		return got
	}
	first := "2026-11-12T10:00:00.123456Z"
	if got := wake(first); got != "succeeded" {
		t.Fatalf("first wake = %q", got)
	}
	if got := wake(first); got != "already_sent" {
		t.Fatalf("second wake in the same maintenance = %q", got)
	}
	if calls := prerunCalls(t, log); len(calls) != 1 {
		t.Fatalf("hook calls in one maintenance = %d, want 1", len(calls))
	}
	if got := wake("2026-11-12T12:00:00Z"); got != "succeeded" {
		t.Fatalf("new maintenance entry = %q", got)
	}
	if calls := prerunCalls(t, log); len(calls) != 2 {
		t.Fatalf("hook calls after a new maintenance = %d, want 2", len(calls))
	}
	// A failed notification leaves no marker, so the next wake retries.
	failing, failLog := prerunHook(t, 9)
	b.NotifyHook = failing
	third := "2026-11-12T14:00:00Z"
	if got := wake(third); got != "failed" {
		t.Fatalf("failing hook = %q", got)
	}
	if got := wake(third); got != "failed" || len(prerunCalls(t, failLog)) != 2 {
		t.Fatalf("failed notification not retried: %q", got)
	}
	// Other causes are never deduplicated.
	b.NotifyHook = hook
	for range 2 {
		notifyBackupPreRunOnce(context.Background(), b, backupCauseDBUnreachable, "", now, &bytes.Buffer{})
	}
	if calls := prerunCalls(t, log); len(calls) != 4 {
		t.Fatalf("db_unreachable calls = %d, want every wake", len(calls)-2)
	}
}

// TestAdminIntegrationBackupMaintenanceRefusalKeyedOnEntry: Backup during
// maintenance reports the maintenance cause keyed on that entry's entered_at.
func TestAdminIntegrationBackupMaintenanceRefusalKeyedOnEntry(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='administrative repair',owner='t',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var entered time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT entered_at FROM maintenance_state WHERE id=1`).Scan(&entered); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Backup(ctx, BackupSettings{Root: root}, false)
	cause, key, ok := backupPreRunDetail(err)
	if !ok || cause != backupCauseMaintenance || key != entered.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("maintenance refusal = %q %q %v (%v)", cause, key, ok, err)
	}
	if n := backupRunCount(t, ctx, s); n != 0 {
		t.Fatalf("maintenance refusal wrote %d history rows", n)
	}
}

func backupRunCount(t *testing.T, ctx context.Context, s *Service) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM backup_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
