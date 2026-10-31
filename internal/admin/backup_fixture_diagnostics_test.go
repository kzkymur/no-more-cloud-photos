package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fixtureSQLState(err error) string {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) != 5 {
		return "unavailable"
	}
	for _, c := range pg.Code {
		if !(c >= '0' && c <= '9') && !(c >= 'A' && c <= 'Z') {
			return "unavailable"
		}
	}
	return pg.Code
}

func fixtureJobMetadata(value string) string {
	switch value {
	case "", "pending", "queued", "running", "succeeded", "failed", "cancelled", "process_timeout", "process_failed", "process_output_limit", "lease_expired", "worker_shutdown", "maintenance_paused":
		return value
	default:
		return "unrecognized"
	}
}

// Deliberately exclude error_message, SQL, DSNs, environment and media data.
func logLiveNativeReadiness(t *testing.T, parent context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	for _, table := range []string{"jobs", "job_targets"} {
		rows, err := pool.Query(ctx, "SELECT status,coalesce(error_code,''),count(*),min(attempts),max(attempts) FROM "+table+" GROUP BY 1,2 ORDER BY 1,2 LIMIT 32")
		if err != nil {
			t.Logf("native readiness %s query sqlstate=%s", table, fixtureSQLState(err))
			continue
		}
		for rows.Next() {
			var status, code string
			var count, first, last int64
			if err := rows.Scan(&status, &code, &count, &first, &last); err != nil {
				t.Log("native readiness metadata scan failed")
				break
			}
			t.Logf("native readiness %s: status=%s code=%s count=%d attempts=%d..%d", table, fixtureJobMetadata(status), fixtureJobMetadata(code), count, first, last)
		}
		if rows.Err() != nil {
			t.Logf("native readiness metadata sqlstate=%s", fixtureSQLState(rows.Err()))
		}
		rows.Close()
	}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		properties, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--all", "--property=Id,MainPID,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus,NRestarts", unit).Output()
		t.Logf("native readiness owned unit=%s inspection_error=%t properties=%.2048q", unit, err != nil, properties)
	}
	var journal fixtureBoundedOutput
	cmd := exec.CommandContext(ctx, "/usr/bin/journalctl", "--unit=nmcp-worker.service", "--lines=32", "--output=cat", "--no-pager")
	cmd.Stdout = &journal
	err := cmd.Run()
	stages := map[string]int{}
	for _, line := range strings.Split(string(journal.data), "\n") {
		stages[fixtureWorkerStage(line)]++
	}
	for _, stage := range []string{"ready", "still_capabilities", "animation_capabilities", "video_capabilities", "other_startup_error", "unrecognized"} {
		if stages[stage] > 0 {
			t.Logf("native readiness worker journal: stage=%s count=%d inspection_error=%t", stage, stages[stage], err != nil)
		}
	}
}

type fixtureBoundedOutput struct{ data []byte }

func (b *fixtureBoundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 65536 - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func fixtureWorkerStage(message string) string {
	var record struct {
		Message string `json:"msg"`
		Error   string `json:"error"`
	}
	if json.Unmarshal([]byte(message), &record) != nil {
		return "unrecognized"
	}
	if record.Message == "core Worker ready" {
		return "ready"
	}
	if record.Message != "core Worker stopped with an error" {
		return "unrecognized"
	}
	for _, stage := range []string{"still", "animation", "video"} {
		if strings.HasPrefix(record.Error, "check "+stage+" processor capabilities:") {
			return stage + "_capabilities"
		}
	}
	return "other_startup_error"
}

func TestBackupFixtureDiagnosticsExcludeSecrets(t *testing.T) {
	secret := "postgres://user:CANARY@host/db?password=CANARY"
	for _, state := range []string{"42601", "42501", "08006"} {
		err := fmt.Errorf("wrapper %s: %w", secret, &pgconn.PgError{Code: state, Message: secret, Detail: secret})
		if got := fixtureSQLState(err); got != state {
			t.Fatalf("state mismatch %q", got)
		}
	}
	for _, err := range []error{nil, errors.New(secret), &pgconn.PgError{Code: secret}, &pgconn.PgError{Code: "abcde"}, &pgconn.PgError{Code: "42\n01"}} {
		if fixtureSQLState(err) != "unavailable" {
			t.Fatal("unsafe diagnostic")
		}
	}
	for _, code := range []string{secret, "arbitrary-message", "process_failed\n"} {
		if fixtureJobMetadata(code) != "unrecognized" {
			t.Fatal("unsafe job diagnostic")
		}
	}
	if fixtureJobMetadata("process_failed") != "process_failed" {
		t.Fatal("known failure hidden")
	}
}

func TestBackupFixtureJournalBoundedAndClassified(t *testing.T) {
	var output fixtureBoundedOutput
	input := strings.Repeat("CANARY", 20000)
	if n, err := output.Write([]byte(input)); err != nil || n != len(input) || len(output.data) != 65536 {
		t.Fatal("unbounded journal capture")
	}
	if _, err := output.Write([]byte(input)); err != nil || len(output.data) != 65536 {
		t.Fatal("capture grew")
	}
	for _, stage := range []string{"still", "animation", "video"} {
		raw := `{"msg":"core Worker stopped with an error","error":"check ` + stage + ` processor capabilities: postgres://CANARY"}`
		if fixtureWorkerStage(raw) != stage+"_capabilities" {
			t.Fatal("stage not classified")
		}
	}
	for _, raw := range []string{input, `{"msg":"CANARY","error":"CANARY"}`, `{"password":"CANARY"}`} {
		if fixtureWorkerStage(raw) != "unrecognized" {
			t.Fatal("raw journal leaked")
		}
	}
}
