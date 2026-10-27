package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupIfDueParser(t *testing.T) {
	for _, test := range []struct {
		args  []string
		due   bool
		valid bool
	}{
		{[]string{"backup", "run", "--if-due", "--json"}, true, true},
		{[]string{"backup", "run", "--json"}, false, true},
		{[]string{"backup", "run", "--if-due=false"}, false, true},
		{[]string{"backup", "list", "--if-due"}, false, false},
		{[]string{"backup", "run", "--if-due=credential-secret"}, false, false},
		{[]string{"backup", "run", "unexpected"}, false, false},
	} {
		o, err := parse(test.args)
		if (err == nil) != test.valid {
			t.Fatalf("%v: %v", test.args, err)
		}
		if err == nil && o.ifDue != test.due {
			t.Fatalf("%v due=%v", test.args, o.ifDue)
		}
	}
}
func TestBackupNoopStructuredExitFour(t *testing.T) {
	for _, outcome := range []string{"not_due", "overlap_conflict"} {
		t.Run(outcome, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := finish(&out, &diagnostics, true, BackupRun{Outcome: outcome}, ErrConflict)
			if code != 4 {
				t.Fatalf("exit=%d", code)
			}
			var got struct {
				Result BackupRun `json:"result"`
				Exit   int       `json:"exit_code"`
			}
			d := json.NewDecoder(&out)
			if err := d.Decode(&got); err != nil {
				t.Fatal(err)
			}
			if err := d.Decode(new(any)); err != io.EOF {
				t.Fatal("stdout not exactly one JSON value")
			}
			if got.Exit != 4 || got.Result.Outcome != outcome || got.Result.Status == "succeeded" || got.Result.ID != "" {
				t.Fatalf("invalid noop %+v", got)
			}
			if strings.Contains(diagnostics.String(), "credential-secret") {
				t.Fatal("secret diagnostics")
			}
		})
	}
}
func TestBackupSettingsPaths(t *testing.T) {
	valid := map[string]string{"NMCP_BACKUP_ROOT": "/private/backup", "NMCP_BACKUP_DUMP_SERVICE": "nmcp_backup_dump", "PGSERVICEFILE": "/private/service.conf", "NMCP_BACKUP_DUMP_PASSFILE": "/private/dump.pgpass", "NMCP_BACKUP_PG_DUMP": "", "NMCP_BACKUP_PG_RESTORE": ""}
	for k, v := range valid {
		t.Setenv(k, v)
	}
	settings, err := backupSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.DumpTool != "/usr/bin/pg_dump" || settings.RestoreTool != "/usr/bin/pg_restore" {
		t.Fatal("unexpected tool defaults")
	}
	for _, test := range []struct{ key, value string }{{"NMCP_BACKUP_ROOT", "relative"}, {"PGSERVICEFILE", "relative"}, {"NMCP_BACKUP_DUMP_PASSFILE", "relative"}, {"NMCP_BACKUP_DUMP_SERVICE", "dump password=secret"}, {"NMCP_BACKUP_DUMP_SERVICE", "dump\nuser=operator"}, {"NMCP_BACKUP_PG_DUMP", "pg_dump"}, {"NMCP_BACKUP_PG_RESTORE", "pg_restore"}} {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := backupSettings(); !errors.Is(err, ErrValidation) {
				t.Fatalf("not validation: %v", err)
			}
		})
	}
}
func TestBackupToolCredentialEnvironment(t *testing.T) {
	for _, key := range []string{"PGPASSWORD", "PGHOST", "PGDATABASE", "PGUSER", "PGOPTIONS", "NMCP_DATABASE_URL", "NMCP_RESTORE_DATABASE_URL", "HTTP_PROXY"} {
		t.Setenv(key, "credential-secret")
	}
	t.Setenv("PGPASSFILE", "/control/secret.pgpass")
	b := BackupSettings{DumpService: "readonly_dump", ServiceFile: "/fixed/service.conf", PassFile: "/dump/dump.pgpass"}
	var out bytes.Buffer
	if err := b.tool(context.Background(), "/usr/bin/env", nil, &out); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "PGSERVICE": "readonly_dump", "PGSERVICEFILE": "/fixed/service.conf", "PGPASSFILE": "/dump/dump.pgpass"}
	for _, entry := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || expected[key] != value {
			t.Fatalf("unexpected child env %q", entry)
		}
		delete(expected, key)
	}
	if len(expected) != 0 || strings.Contains(out.String(), "credential-secret") || strings.Contains(out.String(), "/control/") {
		t.Fatalf("credential environment not isolated %q", out.String())
	}
}
func backupFixtureTool(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestBackupToolFailureSanitized(t *testing.T) {
	p := backupFixtureTool(t, "printf 'credential-secret' >&2\nexit 7\n")
	err := (BackupSettings{}).tool(context.Background(), p, []string{"argument-secret"}, io.Discard)
	if !errors.Is(err, errBackup) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsanitized error %v", err)
	}
}
func TestBackupToolCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (BackupSettings{}).tool(ctx, "/usr/bin/true", nil, io.Discard); !errors.Is(err, errBackup) {
		t.Fatalf("canceled tool: %v", err)
	}
}
func TestBackupToolMajor(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"pg_dump (PostgreSQL) 17.6", "17"}, {"pg_restore (PostgreSQL) 17.6 (Ubuntu 17.6-1.pgdg24.04+1)", "17"}, {"pg_dump (PostgreSQL) 18.0\n", "18"}, {"", ""}, {"garbage 17.6", ""}, {"pg_dump (PostgreSQL) not-a-version", ""},
	} {
		if got := toolMajor(test.input); got != test.want {
			t.Fatalf("toolMajor(%q)=%q want %q", test.input, got, test.want)
		}
	}
}
func TestBackupToolVersionCompatibilityAndBounds(t *testing.T) {
	for _, test := range []struct {
		output, server string
		valid          bool
	}{
		{"pg_dump (PostgreSQL) 17.6", "17.7", true}, {"pg_dump (PostgreSQL) 16.9", "17.6", false}, {"unexpected 17.6", "17.6", false}, {strings.Repeat("x", 4097), "17.6", false},
	} {
		p := backupFixtureTool(t, "printf '%s' '"+test.output+"'\n")
		b := BackupSettings{DumpTool: p}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := b.versions(ctx, test.server)
		cancel()
		if (err == nil) != test.valid {
			t.Fatalf("version valid=%v err=%v", test.valid, err)
		}
	}
}
func TestBackupDumpPassfileSecurityBeforeConnect(t *testing.T) {
	for _, kind := range []string{"missing", "public", "symlink", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "dump.pgpass")
			var err error
			switch kind {
			case "missing":
			case "public":
				err = os.WriteFile(p, []byte("*:*:*:*:dump-secret\n"), 0644)
			case "symlink":
				err = os.Symlink("/etc/passwd", p)
			case "hardlink":
				target := filepath.Join(dir, "target")
				err = os.WriteFile(target, nil, 0600)
				if err == nil {
					err = os.Link(target, p)
				}
			case "directory":
				err = os.Mkdir(p, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			b := BackupSettings{PassFile: p}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := b.dumpConnect(ctx)
			if conn != nil {
				conn.Close(context.Background())
				t.Fatal("unsafe passfile connected")
			}
			if !errors.Is(err, errBackup) {
				t.Fatalf("unsafe passfile returned %v", err)
			}
		})
	}
}
