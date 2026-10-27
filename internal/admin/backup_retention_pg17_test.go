package admin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

// Real SQL/FS causal proof. Fixture history deliberately backdates real custom
// dumps; it does not pretend the dumps themselves were taken forty days ago.
func TestBackupPG17RetentionPolicySerialization(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: real retention serialization NOT executed")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute PG17 bindir required")
	}
	for _, tool := range []string{"postgres", "pg_dump", "pg_restore"} {
		out, err := exec.Command(filepath.Join(bin, tool), "--version").Output()
		if err != nil || !strings.Contains(string(out), "(PostgreSQL) 17.") {
			t.Fatal("genuine PG17 required")
		}
	}
	f := newPG17Fixture(t, bin)
	ctx := f.ctx
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrap(false)
	f.sql(operator, "ALTER ROLE nmcp_migrator PASSWORD '"+f.passwords["nmcp_migrator"]+"'")
	migrator := f.pool("nmcp_migrator", "source")
	m, err := database.NewMigrator(migrator)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Up(ctx); err != nil {
		t.Fatal("actual migration chain", err)
	}
	status, err := m.Status(ctx)
	if err != nil || !status.Ready() || status.CurrentVersion != status.ExpectedVersion {
		t.Fatal("actual current migration count not ready")
	}
	source := f.pool("operator", "source")
	// Migration service is intentional until the independently owned role SQL's
	// strict function-body pin is republished. No substitute grants are made.
	service := New(migrator)
	config := New(migrator)
	root := filepath.Join(f.root, "retention")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	b := BackupSettings{Root: root}
	publish := func(days int) string {
		t.Helper()
		id := newID()
		store, e := backupartifact.Open(root, backupartifact.Options{})
		if e != nil {
			t.Fatal("artifact store", e)
		}
		defer store.Close()
		manifest, e := store.Publish(ctx, backupartifact.Metadata{ID: id, SnapshotCutoff: time.Now().UTC(), Config: json.RawMessage(`{}`), ServerVersion: "17", ToolVersions: map[string]string{"pg_dump": "17"}},
			func(ctx context.Context, file *os.File) error {
				cmd := f.process(ctx, "pg_dump", "--format=custom", "--dbname="+f.dsn("operator", "source"))
				cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
				cmd.Stdout = file
				cmd.Stderr = io.Discard
				return cmd.Run()
			}, func(ctx context.Context, file *os.File) error {
				cmd := f.process(ctx, "pg_restore", "--list")
				cmd.Stdin = file
				cmd.Stdout = io.Discard
				cmd.Stderr = io.Discard
				return cmd.Run()
			})
		if e != nil {
			t.Fatal("real dump/list/durable publish", e)
		}
		raw, e := json.Marshal(manifest)
		if e != nil {
			t.Fatal(e)
		}
		f.sql(source, `INSERT INTO backup_runs(id,status,started_at,finished_at,config_snapshot,final_relative_path,size_bytes,sha256,manifest,postgres_version,tool_version) VALUES($1::uuid,'succeeded',clock_timestamp()-make_interval(days=>$2),clock_timestamp(),'{}',$1::text,$3,$4,$5,'17','17')`, id, days, manifest.DumpSize, manifest.DumpSHA256, raw)
		return id
	}
	old := publish(40)
	anchor := publish(31)
	verify := func(id string) {
		t.Helper()
		store, e := backupartifact.Open(root, backupartifact.Options{ReadOnly: true})
		if e != nil {
			t.Fatal(e)
		}
		defer store.Close()
		if _, e = store.Verify(id); e != nil {
			t.Fatal("artifact no longer valid", e)
		}
	}
	if _, err = config.ConfigSet(ctx, "db_backup_retention_days", "30"); err != nil {
		t.Fatal("ConfigSet30", err)
	}
	selected := 0
	service.afterBackupCleanupSelection = func(ctx context.Context) error {
		selected++
		// This is after a real selection commit, not an injected selection list.
		_, e := config.ConfigSet(ctx, "db_backup_retention_days", "90")
		return e
	}
	first, err := service.CleanupBackups(ctx, b, false)
	service.afterBackupCleanupSelection = nil
	if err != nil || selected != 1 || len(first.Deleted) != 0 || len(first.Eligible) != 1 || first.Eligible[0] != old || len(first.SkippedIDs) != 1 || first.SkippedIDs[0] != old {
		t.Fatalf("policy extension did not skip selection: %+v err=%v", first, err)
	}
	verify(old)
	verify(anchor)
	if pg17Scalar(t, ctx, source, `SELECT (retention_deleting_at IS NULL AND retention_deleted_at IS NULL)::text FROM backup_runs WHERE id=$1`, old) != "true" {
		t.Fatal("skipped selection changed durable lifecycle")
	}
	// A committed intent is not authority under the now-extended policy either.
	f.sql(source, `UPDATE backup_runs SET retention_deleting_at=clock_timestamp() WHERE id=$1`, old)
	again, err := service.CleanupBackups(ctx, b, false)
	if err != nil || len(again.Deleted) != 0 {
		t.Fatal("intent overrode current retention", err)
	}
	verify(old)
	verify(anchor)
	if _, err = config.ConfigSet(ctx, "db_backup_retention_days", "30"); err != nil {
		t.Fatal(err)
	}
	// Deferred constraint trigger runs inside the actual audit COMMIT, after
	// filesystem deletion. Releasing config SHARE before commit must fail this
	// oracle, even if the earlier hook observed a transient lock wait.
	f.sql(migrator, `CREATE FUNCTION public.retention_commit_lock_oracle() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
 BEGIN
  IF NEW.command='cleanup_backups' THEN
   IF NOT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity a
     WHERE a.datname=current_database() AND a.wait_event_type='Lock'
       AND a.query LIKE 'UPDATE system_config SET db_backup_retention_days=%'
       AND pg_backend_pid()=ANY(pg_catalog.pg_blocking_pids(a.pid))
       AND EXISTS(SELECT 1 FROM pg_catalog.pg_locks w WHERE w.pid=a.pid AND NOT w.granted))
     OR (SELECT db_backup_retention_days FROM public.system_config WHERE id=1)<>30 THEN
    RAISE EXCEPTION 'config SHARE not held at audit commit' USING ERRCODE='23514';
   END IF;
  END IF;
  RETURN NEW;
 END; $$;
 CREATE CONSTRAINT TRIGGER retention_commit_lock_oracle AFTER INSERT ON admin_audit
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.retention_commit_lock_oracle()`)
	// Start an actual ConfigSet while the deletion transaction holds config SHARE.
	// Prove its UPDATE is waiting on that backend using PostgreSQL's lock graph.
	setDone := make(chan error, 1)
	started := false
	service.beforeBackupArtifactDelete = func(ctx context.Context) error {
		started = true
		go func() { _, e := config.ConfigSet(ctx, "db_backup_retention_days", "90"); setDone <- e }()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			e := source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE a.datname=current_database() AND a.wait_event_type='Lock' AND a.query LIKE 'UPDATE system_config SET db_backup_retention_days=%' AND EXISTS(SELECT 1 FROM unnest(pg_blocking_pids(a.pid)) p(pid) JOIN pg_locks l ON l.pid=p.pid WHERE l.locktype='advisory' AND l.granted) AND EXISTS(SELECT 1 FROM pg_locks w WHERE w.pid=a.pid AND NOT w.granted))`).Scan(&blocked)
			if e != nil {
				return e
			}
			if blocked {
				break
			}
			select {
			case e := <-setDone:
				t.Fatalf("ConfigSet escaped held SHARE fence: %v", e)
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				t.Fatal("no actual pg_stat_activity/pg_locks blocking proof")
			case <-tick.C:
			}
		}
		verify(old)
		verify(anchor)
		if pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='cleanup_backups' AND $1::uuid=ANY(affected_ids)`, old) != "0" {
			t.Fatal("delete audit preceded filesystem delete")
		}
		return nil
	}
	final, err := service.CleanupBackups(ctx, b, false)
	service.beforeBackupArtifactDelete = nil
	if err != nil || !started || len(final.Deleted) != 1 || final.Deleted[0] != old {
		t.Fatalf("reset30 did not delete exact old artifact: %+v %v", final, err)
	}
	select {
	case e := <-setDone:
		if e != nil {
			t.Fatal("blocked ConfigSet failed after commit", e)
		}
	case <-ctx.Done():
		t.Fatal("ConfigSet never released")
	}
	f.sql(migrator, `DROP TRIGGER retention_commit_lock_oracle ON admin_audit; DROP FUNCTION public.retention_commit_lock_oracle()`)
	verify(anchor)
	if _, err = os.Stat(filepath.Join(root, old)); !os.IsNotExist(err) {
		t.Fatal("old artifact bytes remain")
	}
	if pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='cleanup_backups' AND outcome='succeeded' AND $1::uuid=ANY(affected_ids)`, old) != "1" {
		t.Fatal("cleanup audit not committed exactly once")
	}
	if _, err = config.ConfigSet(ctx, "db_backup_retention_days", "30"); err != nil {
		t.Fatal(err)
	}
	final, err = service.CleanupBackups(ctx, b, false)
	if err != nil || len(final.Deleted) != 0 {
		t.Fatal("cleanup failed to converge", err)
	}
	verify(anchor)
	t.Log("real PG17 + actual dump/list/FS: selection -> ConfigSet90 -> skip; committed intent obeys current retention; reset30 deletes old only; actual ConfigSet lock wait released by delete/audit commit; aged last-one preserved. Migration-service role, NOT restricted control-role acceptance.")
}
