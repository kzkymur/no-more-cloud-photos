package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

func TestRunRejectsInvalidCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("run() code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunOperationalErrorIsStructured(t *testing.T) {
	t.Setenv("NMCP_DATABASE_URL", "")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"migrate", "status"}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("run() code = %d, want %d", code, exitFailure)
	}
	var record map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("stderr is not one JSON log record: %q: %v", stderr.String(), err)
	}
	if record["level"] != "ERROR" || record["msg"] != "load configuration" {
		t.Fatalf("structured log = %#v", record)
	}
}

func TestRunMigrationIntegration(t *testing.T) {
	baseDatabaseURL := os.Getenv("TEST_DATABASE_URL")
	if baseDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	databaseURL := isolatedDatabaseURL(t, baseDatabaseURL)
	t.Setenv("NMCP_DATABASE_URL", databaseURL)

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"migrate", "up", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("migrate up code = %d stderr=%q", code, stderr.String())
	}
	var status database.Status
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode migrate up output %q: %v", stdout.String(), err)
	}
	if !status.Ready() {
		t.Fatalf("migrate up status = %+v, want ready", status)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"migrate", "status", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("migrate status code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"current_version":`) {
		t.Fatalf("migrate status output = %q", stdout.String())
	}
}

func isolatedDatabaseURL(t *testing.T, databaseURL string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create integration admin pool: %v", err)
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		admin.Close()
		t.Fatalf("generate integration schema: %v", err)
	}
	schema := "nmcp_admin_test_" + hex.EncodeToString(random)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create integration schema: %v", err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = identifier
	return config.ConnString()
}
