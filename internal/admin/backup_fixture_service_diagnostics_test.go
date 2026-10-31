package admin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func fixtureBackupPhase(value string) string {
	switch value {
	case "":
		return "none"
	case "running_commit", "prepare", "dump", "publication_uncertain", "authority_lost_after_publish", "history":
		return value
	default:
		return "unrecognized"
	}
}

func fixtureBackupCLI(message string) (string, int) {
	var record struct {
		Message  string `json:"message"`
		ExitCode *int   `json:"exit_code"`
	}
	if json.Unmarshal([]byte(message), &record) != nil || record.ExitCode == nil || *record.ExitCode < 0 || *record.ExitCode > 5 {
		return "unrecognized", -1
	}
	labels := map[string]string{"success": "success", "administrative operation failed": "operation_failed", ErrValidation.Error(): "validation", ErrConfirmation.Error(): "confirmation", ErrConflict.Error(): "conflict", ErrPartial.Error(): "partial"}
	if label, ok := labels[record.Message]; ok {
		return label, *record.ExitCode
	}
	return "unrecognized", -1
}

func fixtureUnitProperty(key, value string) string {
	switch key {
	case "MainPID", "ExecMainCode", "ExecMainStatus", "NRestarts":
		if _, err := strconv.ParseUint(value, 10, 32); err == nil && len(value) <= 10 {
			return value
		}
	case "ActiveState", "SubState", "Result":
		switch value {
		case "active", "inactive", "failed", "activating", "deactivating", "running", "dead", "exited", "success", "exit-code", "signal", "core-dump", "timeout", "resources", "protocol", "condition", "start-limit-hit", "oom-kill", "watchdog", "dependency":
			return value
		}
	}
	return "unrecognized"
}

func (f *liveCutoverFixture) startBackup(pool *pgxpool.Pool) {
	f.t.Helper()
	cmd := exec.CommandContext(f.ctx, "/usr/bin/systemctl", "start", "nmcp-backup.service")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	err := cmd.Run()
	_, statErr := os.Lstat("/run/nmcp-backup")
	runtimeAbsent := os.IsNotExist(statErr)
	if err != nil || !runtimeAbsent {
		logLiveBackupFailure(f.t, f.ctx, pool)
		f.t.Fatalf("actual production backup unit failed or runtime credentials survived: runtime_absent=%t; raw output suppressed, no manual runtime removal", runtimeAbsent)
	}
}

func logLiveBackupFailure(t *testing.T, parent context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var properties fixtureBoundedOutput
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--all", "--property=MainPID,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus,NRestarts", "nmcp-backup.service")
	cmd.Stdout = &properties
	t.Logf("backup owned-unit inspection_error=%t", cmd.Run() != nil)
	for _, line := range strings.Split(string(properties.data), "\n") {
		field := strings.SplitN(line, "=", 2)
		if len(field) != 2 {
			continue
		}
		// Never echo unknown property names or values.
		switch field[0] {
		case "MainPID", "ActiveState", "SubState", "Result", "ExecMainCode", "ExecMainStatus", "NRestarts":
			t.Logf("backup owned-unit %s=%s", field[0], fixtureUnitProperty(field[0], field[1]))
		}
	}
	rows, err := pool.Query(ctx, `SELECT status,coalesce(error_code,''),notification_status,failure_audit_pending,count(*) FROM backup_runs GROUP BY 1,2,3,4 ORDER BY 1,2,3,4 LIMIT 32`)
	if err != nil {
		t.Logf("backup metadata sqlstate=%s", fixtureSQLState(err))
	} else {
		groups := 0
		for rows.Next() {
			var status, phase, notification string
			var auditPending bool
			var count int64
			if rows.Scan(&status, &phase, &notification, &auditPending, &count) != nil {
				t.Log("backup metadata scan failed")
				break
			}
			groups++
			t.Logf("backup metadata status=%s phase=%s notification=%s audit_pending=%t count=%d", fixtureJobMetadata(status), fixtureBackupPhase(phase), fixtureJobMetadata(notification), auditPending, count)
		}
		t.Logf("backup metadata groups=%d sqlstate=%s", groups, fixtureSQLState(rows.Err()))
		rows.Close()
	}
	var journal fixtureBoundedOutput
	cmd = exec.CommandContext(ctx, "/usr/bin/journalctl", "--unit=nmcp-backup.service", "--lines=32", "--output=cat", "--no-pager")
	cmd.Stdout = &journal
	failed := cmd.Run() != nil
	counts := map[string]int{}
	for _, line := range strings.Split(string(journal.data), "\n") {
		label, code := fixtureBackupCLI(line)
		key := label + "/" + strconv.Itoa(code)
		counts[key]++
	}
	// Keys above contain only fixed labels and bounded numeric codes.
	for key, count := range counts {
		t.Logf("backup CLI classification=%s count=%d inspection_error=%t", key, count, failed)
	}
}

func TestBackupFixtureServiceDiagnosticsClosed(t *testing.T) {
	secret := "postgres://user:CANARY@host/db"
	for _, value := range []string{secret, "prepare\nCANARY", "invalid"} {
		if fixtureBackupPhase(value) != "unrecognized" {
			t.Fatal("unsafe phase")
		}
	}
	for _, raw := range []string{secret, `{"message":"CANARY","exit_code":1}`, `{"message":"success","exit_code":999}`, `{"message":"success"}`} {
		if label, code := fixtureBackupCLI(raw); label != "unrecognized" || code != -1 {
			t.Fatal("unsafe CLI classification")
		}
	}
	if label, code := fixtureBackupCLI(`{"message":"administrative operation failed","exit_code":1,"password":"CANARY"}`); label != "operation_failed" || code != 1 {
		t.Fatal("closed CLI failure hidden")
	}
	for _, key := range []string{"MainPID", "Result", "CANARY"} {
		if fixtureUnitProperty(key, secret) != "unrecognized" {
			t.Fatal("unsafe unit property")
		}
	}
	if fixtureUnitProperty("MainPID", strings.Repeat("0", 2048)) != "unrecognized" {
		t.Fatal("unbounded numeric diagnostic")
	}
	if fixtureUnitProperty("ExecMainStatus", "243") != "243" || fixtureBackupPhase("prepare") != "prepare" {
		t.Fatal("failure discriminators lost")
	}
}
