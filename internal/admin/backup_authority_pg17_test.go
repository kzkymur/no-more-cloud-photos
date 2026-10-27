package admin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

// This component proof uses operator credentials solely to prepare a real
// PostgreSQL stream. It does not substitute for the restricted-role Service
// proof, which calls the same blocked producer from ProductionRoles below.
func TestBackupPG17AuthorityLossDuringRealDump(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("real PG17 authority/tool test not executed: bindir absent")
	}
	f := newPG17Fixture(t, bin)
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrap(false)
	f.sql(operator, "ALTER ROLE nmcp_migrator PASSWORD '"+f.passwords["nmcp_migrator"]+"'")
	migrationPool := f.pool("nmcp_migrator", "source")
	m, err := database.NewMigrator(migrationPool)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Up(f.ctx); err != nil {
		t.Fatal("actual source migration setup failed")
	}
	source := f.pool("operator", "source")
	conn, err := source.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(f.ctx, `SELECT pg_advisory_lock(hashtextextended('backup',1818))`); err != nil {
		t.Fatal(err)
	}
	var pid int
	if conn.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid) != nil {
		t.Fatal("lock pid")
	}
	authority := newBackupAuthority(f.ctx, conn)
	authority.Start()
	defer func() { _ = authority.Stop(); authority.cancel() }()
	if authority.Check(f.ctx) != nil {
		t.Fatal("original exact advisory lock not detected")
	}
	root := filepath.Join(f.root, "authority-artifacts")
	if os.Mkdir(root, 0700) != nil {
		t.Fatal("artifact root")
	}
	store, err := backupartifact.Open(root, backupartifact.Options{Lock: true, BeforePublish: authority.Check})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := BackupSettings{Root: root, DumpService: "operator", ServiceFile: f.serviceFile, PassFile: f.passfiles["operator"], DumpTool: filepath.Join(bin, "pg_dump"), RestoreTool: filepath.Join(bin, "pg_restore")}
	marker := filepath.Join(f.root, "real-producer-blocked")
	blocked := pg17BlockedDump(t, f, settings.DumpTool, marker)
	settings.DumpTool = blocked
	id := newID()
	result := make(chan error, 1)
	go func() {
		_, err := store.Publish(authority.ctx, backupartifact.Metadata{ID: id, SnapshotCutoff: time.Now().UTC(), Config: []byte(`{}`), ToolVersions: map[string]string{"pg_dump": "17"}, ServerVersion: "17"}, func(ctx context.Context, file *os.File) error {
			return settings.tool(ctx, settings.DumpTool, []string{"--no-password", "--format=custom"}, file)
		}, func(context.Context, *os.File) error { return nil })
		result <- err
	}()
	producerPID := pg17AwaitProducer(t, f, marker, result)
	var killed bool
	if source.QueryRow(f.ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed) != nil || !killed {
		t.Fatal("actual lock backend kill")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("lost lock published actual dump")
		}
	case <-time.After(8 * time.Second):
		authority.cancel()
		t.Fatal("real tool did not stop promptly after backend loss")
	}
	pg17AwaitProducerStopped(t, f, producerPID)
	if authority.Stop() == nil {
		t.Fatal("lost authority not reported")
	}
	if _, err = os.Lstat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatal("lost authority crossed final rename")
	}
	t.Log("real PG17 advisory backend killed during blocked actual pg_dump; heartbeat canceled tool and final artifact absent; operator component proof")
}

func pg17BlockedDump(t *testing.T, f *pg17Fixture, real, marker string) string {
	t.Helper()
	path := filepath.Join(f.root, "blocked-real-pg_dump-"+newID())
	f.write(path, fmt.Sprintf(`#!/usr/bin/python3
import os, subprocess, sys, time
real = %q
if sys.argv[1:] == ['--version']:
    os.execve(real, [real, '--version'], os.environ)
p = subprocess.Popen([real, *sys.argv[1:]], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=os.environ)
try:
    prefix = p.stdout.read(4096)
    if len(prefix) != 4096 or prefix[:5] != b'PGDMP' or p.poll() is not None:
        raise RuntimeError('real producer not blocked on custom stream')
    os.write(1, prefix)
    with open(%q, 'x') as receipt:
        receipt.write('real-PGDMP-producer-blocked:'+str(p.pid))
    time.sleep(60)
finally:
    p.terminate()
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        p.kill(); p.wait(timeout=5)
sys.exit(24)
`, real, marker), 0700)
	return path
}
func pg17AwaitProducer(t *testing.T, f *pg17Fixture, marker string, early <-chan error) int {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if raw, err := os.ReadFile(marker); err == nil && strings.HasPrefix(string(raw), "real-PGDMP-producer-blocked:") {
			pid, parseErr := strconv.Atoi(strings.TrimPrefix(string(raw), "real-PGDMP-producer-blocked:"))
			if parseErr != nil || pid <= 0 {
				t.Fatal("invalid real producer PID")
			}
			return pid
		}
		select {
		case <-early:
			t.Fatal("dump ended before blocked producer observation")
		case <-deadline.C:
			t.Fatal("real custom dump producer not observed blocked")
		case <-f.ctx.Done():
			t.Fatal("fixture canceled")
		case <-tick.C:
		}
	}
}

func pg17ServiceLockLossDuringDump(t *testing.T, f *pg17Fixture, source *pgxpool.Pool, service *Service, b BackupSettings) {
	t.Helper()
	marker := filepath.Join(f.root, "service-real-producer-blocked")
	blocked := b
	blocked.DumpTool = pg17BlockedDump(t, f, b.DumpTool, marker)
	type outcome struct {
		run BackupRun
		err error
	}
	done := make(chan outcome, 1)
	early := make(chan error, 1)
	go func() { r, e := service.Backup(f.ctx, blocked, false); done <- outcome{r, e}; early <- e }()
	producerPID := pg17AwaitProducer(t, f, marker, early)
	var pid int
	if source.QueryRow(f.ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&pid) != nil {
		t.Fatal("service lock backend")
	}
	var killed bool
	if source.QueryRow(f.ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed) != nil || !killed {
		t.Fatal("kill service advisory backend")
	}
	select {
	case result := <-done:
		if result.err == nil || result.run.Status != "failed" || result.run.Notification != "succeeded" {
			t.Fatal("lock loss during real dump not failed/notified")
		}
		if _, err := os.Lstat(filepath.Join(b.Root, result.run.ID)); !os.IsNotExist(err) {
			t.Fatal("service published after actual lock loss")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("service real dump was not canceled on lock loss")
	}
	pg17AwaitProducerStopped(t, f, producerPID)
	t.Log("restricted-role Service real blocked pg_dump canceled after actual advisory backend kill; failed notification durable and no rename")
}

func pg17AwaitProducerStopped(t *testing.T, f *pg17Fixture, pid int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatal("cannot inspect actual producer termination")
		}
		// A reparented zombie is exited and cannot execute/write; do not confuse it
		// with a live producer, or falsely claim that its PID was already reaped.
		at := strings.LastIndexByte(string(raw), ')')
		if at >= 0 && len(raw) > at+2 && raw[at+2] == 'Z' {
			t.Log("actual producer exited (zombie awaiting host reaper)")
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("actual pg_dump producer still executing after tool cancellation")
		case <-f.ctx.Done():
			t.Fatal("producer termination observation canceled")
		}
	}
}
