package admin

// This opt-in test owns every PostgreSQL process it starts. It never connects
// to TEST_DATABASE_URL, a system cluster, or a production socket. The same-UID
// sandbox cannot establish production's separate-UID HBA attestation; see the
// explicit Restore rejection proof below, not a fabricated successful receipt.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

type pg17Fixture struct {
	t                                    *testing.T
	ctx                                  context.Context
	bin, root, data, socket, serviceFile string
	passwords                            map[string]string
	passfiles                            map[string]string
}

func pg17Repository(t *testing.T) string {
	t.Helper()
	dir, e := os.Getwd()
	if e != nil {
		t.Fatal("resolve test working directory")
	}
	for {
		if info, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil && info.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot locate repository for PG17 fixture")
		}
		dir = parent
	}
}

func newPG17Fixture(t *testing.T, bin string) *pg17Fixture {
	t.Helper()
	repo := pg17Repository(t)
	// The canonical worktree is workspace/project/worktree. An explicit override
	// supports other layouts without falling back to a global /tmp socket.
	workspace := os.Getenv("TEST_BACKUP_PG17_WORKSPACE")
	if workspace == "" {
		workspace = filepath.Clean(filepath.Join(repo, "../.."))
	}
	if !filepath.IsAbs(workspace) {
		t.Fatal("PG17 workspace must be absolute")
	}
	if os.Geteuid() == 0 && os.Getenv("TEST_BACKUP_PG17_WORKSPACE") == "" {
		// Root CI runs the server as postgres; /tmp is deliberately explicit here,
		// not a fallback for a sandbox-denied workspace operation.
		workspace = "/tmp"
	}
	root, err := os.MkdirTemp(workspace, ".p17-")
	if err != nil {
		t.Fatal("create private workspace fixture")
	}
	if len(filepath.Join(root, ".s.PGSQL.6543")) >= 108 {
		t.Fatal("workspace socket path exceeds Unix limit")
	}
	if os.Geteuid() == 0 {
		postgres, e := user.Lookup("postgres")
		if e != nil {
			t.Fatal("root PG17 test requires postgres OS account")
		}
		uid, e := strconv.Atoi(postgres.Uid)
		if e != nil {
			t.Fatal("postgres UID")
		}
		gid, e := strconv.Atoi(postgres.Gid)
		if e != nil {
			t.Fatal("postgres GID")
		}
		if os.Chown(root, uid, gid) != nil || os.Chmod(root, 0755) != nil {
			t.Fatal("prepare owned postgres socket directory")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	f := &pg17Fixture{t: t, ctx: ctx, bin: bin, root: root, data: filepath.Join(root, "data"), socket: root, serviceFile: filepath.Join(root, "services.conf"), passwords: map[string]string{}, passfiles: map[string]string{}}
	t.Cleanup(cancel)
	// Remove only our exact MkdirTemp result, and only after process absence.
	safeToRemove := true
	t.Cleanup(func() {
		if safeToRemove {
			if err := os.RemoveAll(root); err != nil {
				t.Error("remove owned PG17 fixture")
			}
		}
	})
	for _, role := range []string{"operator", "nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			t.Fatal("generate SCRAM secret")
		}
		f.passwords[role] = hex.EncodeToString(secret[:])
	}
	pw := filepath.Join(root, "init.password")
	f.write(pw, f.passwords["operator"]+"\n", 0600)
	if os.Geteuid() == 0 {
		u, _ := user.Lookup("postgres")
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if os.Chown(pw, uid, gid) != nil {
			t.Fatal("postgres init password ownership")
		}
	}
	f.command("initdb", nil, "-D", f.data, "--username=operator", "--pwfile="+pw, "--auth-local=scram-sha-256", "--auth-host=reject", "--encoding=UTF8", "--locale=C")
	f.write(filepath.Join(f.data, "postgresql.auto.conf"), "listen_addresses = ''\nunix_socket_directories = '"+root+"'\nport = 6543\n", 0600)
	if os.Geteuid() == 0 {
		u, _ := user.Lookup("postgres")
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if os.Chown(filepath.Join(f.data, "postgresql.auto.conf"), uid, gid) != nil {
			t.Fatal("postgres config ownership")
		}
	}
	// Register stop before start, including a start that partially succeeds.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		cmd := f.process(stopCtx, "pg_ctl", "-D", f.data, "-m", "immediate", "-w", "stop")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		stopErr := cmd.Run()
		if raw, e := os.ReadFile(filepath.Join(f.data, "postmaster.pid")); e == nil {
			first := strings.SplitN(string(raw), "\n", 2)[0]
			pid, e := strconv.Atoi(first)
			if e != nil || pid <= 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
				safeToRemove = false
				t.Error("PG17 cleanup could not prove PID absence")
				return
			}
		} else if !os.IsNotExist(e) {
			safeToRemove = false
			t.Error("PG17 cleanup cannot inspect PID file")
			return
		}
		// Independently probe the PID captured at startup, not just a removed file.
		if raw, e := os.ReadFile(filepath.Join(root, "owned.pid")); e == nil {
			pid, e := strconv.Atoi(string(raw))
			if e != nil || syscall.Kill(pid, 0) != syscall.ESRCH {
				safeToRemove = false
				t.Error("PG17 owned postmaster still exists")
				return
			}
		}
		if stopErr != nil {
			t.Error("PG17 stop command failed")
		}
		t.Log("PG17 cleanup: owned postmaster PID absent")
	})
	f.command("pg_ctl", nil, "-D", f.data, "-l", filepath.Join(root, "postgres.log"), "-w", "start")
	raw, err := os.ReadFile(filepath.Join(f.data, "postmaster.pid"))
	if err != nil {
		t.Fatal("read owned postmaster PID")
	}
	f.write(filepath.Join(root, "owned.pid"), strings.SplitN(string(raw), "\n", 2)[0], 0600)
	var services strings.Builder
	for role, password := range f.passwords {
		// No wildcard credentials; dump/control never share a passfile.
		pass := filepath.Join(root, role+".pgpass")
		var entries strings.Builder
		for _, db := range []string{"postgres", "source", "unrelated", "candidate", "gated"} {
			fmt.Fprintf(&entries, "localhost:6543:%s:%s:%s\n%s:6543:%s:%s:%s\n", db, role, password, root, db, role, password)
		}
		f.write(pass, entries.String(), 0600)
		f.passfiles[role] = pass
		fmt.Fprintf(&services, "[%s]\nhost=%s\nport=6543\ndbname=source\nuser=%s\nsslmode=disable\n", role, root, role)
	}
	f.write(f.serviceFile, services.String(), 0600)
	return f
}
func (f *pg17Fixture) write(path, body string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		f.t.Fatal("write private PG17 fixture")
	}
}
func (f *pg17Fixture) process(ctx context.Context, tool string, args ...string) *exec.Cmd {
	if os.Geteuid() == 0 && (tool == "pg_ctl" || tool == "initdb") {
		prefix := []string{"--reuid=postgres", "--regid=postgres", "--init-groups", filepath.Join(f.bin, tool)}
		return exec.CommandContext(ctx, "/usr/bin/setpriv", append(prefix, args...)...)
	}
	return exec.CommandContext(ctx, filepath.Join(f.bin, tool), args...)
}
func (f *pg17Fixture) command(tool string, env []string, args ...string) {
	f.t.Helper()
	cmd := f.process(f.ctx, tool, args...)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}, env...)
	cmd.Stdout = io.Discard
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if runErr := cmd.Run(); runErr != nil {
		// Preserve useful initdb/start diagnostics without exposing SQL/credentials.
		if tool == "initdb" || tool == "pg_ctl" {
			safe := diagnostic.String()
			for _, secret := range f.passwords {
				if secret != "" {
					safe = strings.ReplaceAll(safe, secret, "[REDACTED]")
				}
			}
			if len(safe) > 4096 {
				safe = safe[:4096] + " [truncated]"
			}
			exit := -1
			var failed *exec.ExitError
			if errors.As(runErr, &failed) {
				exit = failed.ExitCode()
			}
			f.t.Fatalf("PG17 %s failed exit=%d: %s", tool, exit, safe)
		}
		state := regexp.MustCompile(`ERROR:\s+([0-9A-Z]{5}):`).FindStringSubmatch(diagnostic.String())
		if len(state) == 2 {
			f.t.Fatalf("PG17 %s failed SQLSTATE=%s (raw output suppressed)", tool, state[1])
		}
		// Only classify fixed libpq connection failures, never echo addresses/secrets.
		if strings.Contains(diagnostic.String(), "no password supplied") {
			f.t.Fatalf("PG17 %s failed: no password supplied", tool)
		}
		if strings.Contains(diagnostic.String(), "invalid connection option") {
			f.t.Fatalf("PG17 %s failed: invalid connection option", tool)
		}
		f.t.Fatalf("PG17 %s failed (raw output suppressed)", tool)
	}
}
func (f *pg17Fixture) dsn(role, db string) string {
	return "host=" + quoteConnValue(f.socket) + " port=6543 dbname=" + quoteConnValue(db) + " user=" + role + " sslmode=disable passfile=" + quoteConnValue(f.passfiles[role])
}
func (f *pg17Fixture) pool(role, db string) *pgxpool.Pool {
	f.t.Helper()
	p, e := pgxpool.New(f.ctx, f.dsn(role, db))
	if e != nil {
		f.t.Fatal("parse PG17 pool")
	}
	f.t.Cleanup(p.Close)
	if p.Ping(f.ctx) != nil {
		f.t.Fatal("connect PG17 role " + role)
	}
	return p
}
func (f *pg17Fixture) sql(p *pgxpool.Pool, sql string, args ...any) {
	f.t.Helper()
	if _, e := p.Exec(f.ctx, sql, args...); e != nil {
		f.t.Fatal("PG17 SQL phase failed (raw error suppressed)")
	}
}
func (f *pg17Fixture) bootstrap(after bool) {
	f.t.Helper()
	file := filepath.Join(pg17Repository(f.t), "infra/ubuntu/postgresql-roles.sql")
	args := []string{"-X", "--no-password", "--set=ON_ERROR_STOP=1", "--set=VERBOSITY=verbose", "--dbname=" + f.dsn("operator", "source")}
	if after {
		args = append(args, "--set=nmcp_backup_grants_after_migration=1", "--set=nmcp_migration_compatibility=0", "--file="+file)
		f.command("psql", nil, args...)
	} else {
		// Fresh bootstrap first establishes the exact hardened graph. Compatibility
		// is intentionally allowed only FROM that verified graph, never empty roles.
		fresh := append(append([]string{}, args...), "--set=nmcp_migration_compatibility=0", "--file="+file)
		f.command("psql", nil, fresh...)
		compatible := append(append([]string{}, args...), "--set=nmcp_migration_compatibility=1", "--file="+file)
		f.command("psql", nil, compatible...)
	}
}
func pg17Scalar(t *testing.T, ctx context.Context, p *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if p.QueryRow(ctx, sql, args...).Scan(&s) != nil {
		t.Fatal("PG17 scalar assertion query")
	}
	return s
}

func TestBackupPG17ProductionRoles(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: real PostgreSQL 17 production-role backup/restore NOT executed")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("TEST_BACKUP_PG17_BINDIR must be absolute")
	}
	for _, tool := range []string{"postgres", "pg_dump", "pg_restore", "initdb", "pg_ctl", "psql"} {
		cmd := exec.Command(filepath.Join(bin, tool), "--version")
		output, e := cmd.Output()
		if e != nil || !strings.Contains(string(output), "(PostgreSQL) 17.") {
			t.Fatalf("%s is not a genuine PostgreSQL 17 binary", tool)
		}
	}
	// dumpConnect intentionally rejects ambient libpq overrides. Do not leak them
	// into fixtures or turn a configured gate into an ambient-credential test.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	f := newPG17Fixture(t, bin)
	ctx := f.ctx
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.sql(operator, `CREATE DATABASE unrelated TEMPLATE template0 OWNER operator`)
	f.sql(operator, `REVOKE ALL ON DATABASE unrelated FROM PUBLIC`)
	f.bootstrap(false)
	// A configured test must fail until the contractual postmigration phase and
	// dedicated roles are integrated. No emergency manual GRANT is permitted.
	if pg17Scalar(t, ctx, operator, `SELECT count(*)::text FROM pg_roles WHERE rolname IN ('nmcp_backup_control','nmcp_backup_dump')`) != "2" {
		t.Fatal("production backup roles absent: integrate SQL role contract; no manual grants")
	}
	for _, role := range []string{"nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		f.sql(operator, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+f.passwords[role]+"'")
	}
	migrator := f.pool("nmcp_migrator", "source")
	migration, e := database.NewMigrator(migrator)
	if e != nil {
		t.Fatal("create production migrator")
	}
	if e = migration.Up(ctx); e != nil {
		t.Fatal("production nmcp_migrator migration failed (raw error suppressed)")
	}
	f.sql(migrator, `CREATE SCHEMA proof AUTHORIZATION nmcp_migrator; CREATE SEQUENCE proof.serial; CREATE TABLE proof.payload(id bigint PRIMARY KEY DEFAULT nextval('proof.serial'),body text NOT NULL); INSERT INTO proof.payload(body) VALUES ('real PostgreSQL 17 data'),('second row')`)
	f.bootstrap(true)
	source := f.pool("operator", "source")
	control := f.pool("nmcp_backup_control", "source")
	dump := f.pool("nmcp_backup_dump", "source")
	if _, e = backupInventory(ctx, mustPG17Conn(t, ctx, dump)); e != nil {
		t.Fatal("postmigration dump inventory contract missing or unsafe")
	}
	for _, sql := range []string{`INSERT INTO proof.payload(body) VALUES ('forbidden')`, `UPDATE proof.payload SET body='forbidden'`, `DELETE FROM proof.payload`, `CREATE TABLE public.forbidden(id int)`, `SET ROLE nmcp_migrator`, `SET ROLE nmcp_backup_control`, `SET ROLE nmcp_purge_function_owner`} {
		if _, e := dump.Exec(ctx, sql); e == nil {
			t.Fatal("restricted dump negative privilege unexpectedly allowed")
		}
	}
	if _, e := control.Exec(ctx, `UPDATE proof.payload SET body='forbidden'`); e == nil {
		t.Fatal("control may mutate unrelated application data")
	}
	if _, e := migrator.Exec(ctx, `CREATE DATABASE forbidden_createdb`); e == nil {
		t.Fatal("migrator unexpectedly has CREATEDB")
	}
	denied, e := pgx.Connect(ctx, f.dsn("nmcp_backup_dump", "unrelated"))
	if e == nil {
		denied.Close(ctx)
		t.Fatal("dump can connect to unrelated database")
	}
	b := BackupSettings{Root: filepath.Join(f.root, "backups"), DumpService: "nmcp_backup_dump", ServiceFile: f.serviceFile, PassFile: f.passfiles["nmcp_backup_dump"], DumpTool: filepath.Join(bin, "pg_dump"), RestoreTool: filepath.Join(bin, "pg_restore"), NotifyHook: filepath.Join(f.root, "notify-ok")}
	if os.Mkdir(b.Root, 0700) != nil {
		t.Fatal("create backup root")
	}
	f.write(b.NotifyHook, "#!/bin/sh\n# reviewed fixture: consume only sanitized metadata; no inherited credentials\ncat >/dev/null\nexit 0\n", 0700)
	service := New(control)
	start := time.Now()
	run, e := service.Backup(ctx, b, false)
	if e != nil || run.Status != "succeeded" {
		t.Fatal("real production-role Backup failed")
	}
	t.Logf("actual PG17 dump success: elapsed_ms=%d", time.Since(start).Milliseconds())
	history, e := service.BackupList(ctx)
	if e != nil || len(history) != 1 || history[0].ID != run.ID || history[0].Status != "succeeded" {
		t.Fatal("success history not durable")
	}
	store, e := backupartifact.Open(b.Root, backupartifact.Options{ReadOnly: true})
	if e != nil {
		t.Fatal("open published artifact")
	}
	defer store.Close()
	manifest, e := store.Verify(run.ID)
	if e != nil || manifest.DumpSize <= 0 || manifest.ID != run.ID {
		t.Fatal("real dump manifest integrity")
	}
	if e := store.WithDump(run.ID, func(file *os.File) error {
		cmd := exec.CommandContext(ctx, b.RestoreTool, "--list")
		cmd.Stdin = file
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return cmd.Run()
	}); e != nil {
		t.Fatal("real pg_restore --list failed")
	}

	// Fresh template0 restore preserves multiowner objects, ACLs, definer code,
	// schema, sequence state and application rows; no --no-owner/--no-acl shortcut.
	f.sql(operator, `CREATE DATABASE candidate TEMPLATE template0 OWNER nmcp_migrator`)
	f.sql(operator, `REVOKE ALL ON DATABASE candidate FROM PUBLIC; GRANT CONNECT ON DATABASE candidate TO nmcp_api`)
	candidate := f.pool("operator", "candidate")
	restoreStart := time.Now()
	if e = store.WithDump(run.ID, func(file *os.File) error {
		cmd := exec.CommandContext(ctx, b.RestoreTool, "--single-transaction", "--exit-on-error", "--dbname="+f.dsn("operator", "candidate"))
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PGPASSFILE=" + f.passfiles["operator"]}
		cmd.Stdin = file
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return cmd.Run()
	}); e != nil {
		t.Fatal("actual fresh operator pg_restore failed")
	}
	for _, query := range pg17CatalogProofQueries {
		if pg17Scalar(t, ctx, source, query) != pg17Scalar(t, ctx, candidate, query) {
			t.Fatal("restored schema/owner/ACL/function/sequence/data equality failed")
		}
	}
	t.Logf("actual fresh pg_restore equality: rpo_ms=%d rto_ms=%d", time.Since(manifest.SnapshotCutoff).Milliseconds(), time.Since(restoreStart).Milliseconds())

	// The due decision must use current database configuration, not a cached
	// timer interval. Moving the last cutoff to 2h ago allows 1h but not 24h.
	before := pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`)
	noop, e := service.Backup(ctx, b, true)
	if !errors.Is(e, ErrConflict) || noop.Outcome != "not_due" || pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`) != before {
		t.Fatal("immediate if-due created history or did not return not_due")
	}
	f.sql(source, `UPDATE backup_runs SET started_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, run.ID)
	for _, test := range []struct {
		hours int
		want  bool
	}{{24, false}, {1, true}} {
		f.sql(source, `UPDATE system_config SET db_backup_interval_hours=$1 WHERE id=1`, test.hours)
		tx, e := control.Begin(ctx)
		if e != nil {
			t.Fatal("begin due proof")
		}
		cfg, _, due, e := backupDue(ctx, tx, true)
		rollback(tx)
		if e != nil || due != test.want || cfg.DBBackupIntervalHours != test.hours {
			t.Fatal("if-due does not use current configuration")
		}
	}
	lastSuccess := pg17Scalar(t, ctx, source, `SELECT max(started_at)::text FROM backup_runs WHERE status='succeeded'`)
	broken := b
	broken.DumpTool = filepath.Join(f.root, "missing-pg_dump")
	failed, e := service.Backup(ctx, broken, false)
	if e == nil || failed.Status != "failed" {
		t.Fatal("tool failure not persisted")
	}
	if pg17Scalar(t, ctx, source, `SELECT status||'/'||notification_status FROM backup_runs WHERE id=$1`, failed.ID) != "failed/succeeded" {
		t.Fatal("tool failure or successful notification not persistent")
	}
	f.write(filepath.Join(f.root, "notify-fail"), "#!/bin/sh\ncat >/dev/null\nexit 9\n", 0700)
	broken.NotifyHook = filepath.Join(f.root, "notify-fail")
	failed, e = service.Backup(ctx, broken, false)
	if e == nil || failed.Status != "failed" || pg17Scalar(t, ctx, source, `SELECT status||'/'||notification_status FROM backup_runs WHERE id=$1`, failed.ID) != "failed/failed" {
		t.Fatal("hook failure not persistent")
	}
	if pg17Scalar(t, ctx, source, `SELECT max(started_at)::text FROM backup_runs WHERE status='succeeded'`) != lastSuccess {
		t.Fatal("failures changed last success")
	}
	lock, e := control.Acquire(ctx)
	if e != nil {
		t.Fatal("acquire overlap holder")
	}
	if _, e = lock.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('backup',1818))`); e != nil {
		lock.Release()
		t.Fatal("hold backup lock")
	}
	before = pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`)
	overlap, overlapErr := service.Backup(ctx, b, false)
	_, unlockErr := lock.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('backup',1818))`)
	lock.Release()
	if unlockErr != nil || !errors.Is(overlapErr, ErrConflict) || overlap.Outcome != "overlap_conflict" || pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`) != before {
		t.Fatal("overlap lock did not prevent a second history row")
	}

	pg17RestoreAttestationBoundary(t, f, source, operator, service, b, run.ID)
	pg17ServiceLockLossDuringDump(t, f, source, service, b)
	pg17InterruptedRealDump(t, f, source, service, b)
	pg17FailureJournalOracles(t, f, source, service, b)
	pg17LockLossNoRivalHistory(t, f, source, service, b)
	pg17CommitUnknownAndRetention(t, f, source, service, b, run.ID)
	t.Log("proof scope: real Backup, real dump/list/manifest, fresh real pg_restore, role negatives; full successful Service.Restore and production distinct-UID HBA isolation NOT proven")
}

func mustPG17Conn(t *testing.T, ctx context.Context, p *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	c, e := p.Acquire(ctx)
	if e != nil {
		t.Fatal("acquire inventory connection")
	}
	t.Cleanup(c.Release)
	return c.Conn()
}

// Stable logical catalog projections avoid database-specific OIDs and never
// include SCRAM secrets. Application rows exclude the backup's own mutable
// history/audit records, which intentionally differ after snapshot publication.
var pg17CatalogProofQueries = []string{
	`SELECT coalesce(jsonb_agg(jsonb_build_array(n.nspname,pg_get_userbyid(n.nspowner),n.nspacl::text) ORDER BY n.nspname),'[]')::text FROM pg_namespace n WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'`,
	`SELECT coalesce(jsonb_agg(jsonb_build_array(n.nspname,c.relname,c.relkind,pg_get_userbyid(c.relowner),c.relacl::text) ORDER BY n.nspname,c.relname),'[]')::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('public','proof')`,
	`SELECT coalesce(jsonb_agg(jsonb_build_array(n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_userbyid(p.proowner),p.prosecdef,p.proacl::text,pg_get_functiondef(p.oid)) ORDER BY n.nspname,p.proname,pg_get_function_identity_arguments(p.oid)),'[]')::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('public','proof')`,
	`SELECT coalesce(jsonb_agg(jsonb_build_array(pg_get_userbyid(d.defaclrole),n.nspname,d.defaclobjtype,d.defaclacl::text) ORDER BY pg_get_userbyid(d.defaclrole),n.nspname,d.defaclobjtype),'[]')::text FROM pg_default_acl d LEFT JOIN pg_namespace n ON n.oid=d.defaclnamespace`,
	`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM proof.payload p`,
	`SELECT jsonb_build_array(last_value,is_called)::text FROM proof.serial`,
	`SELECT to_jsonb(c)::text FROM system_config c WHERE id=1`,
	`SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY version),'[]')::text FROM schema_migrations m`,
}

func pg17RestoreAttestationBoundary(t *testing.T, f *pg17Fixture, source, operator *pgxpool.Pool, service *Service, b BackupSettings, backupID string) {
	t.Helper()
	ctx := f.ctx
	f.sql(operator, `CREATE DATABASE gated TEMPLATE template0 OWNER nmcp_migrator`)
	f.sql(operator, `REVOKE ALL ON DATABASE gated FROM PUBLIC; GRANT CONNECT ON DATABASE gated TO nmcp_api`)
	// Actual ordered local SCRAM gate denies all routine SQL logins except the
	// explicit API proof identity. No TCP listener exists at all.
	hba := "local gated operator scram-sha-256\nlocal gated nmcp_api scram-sha-256\nlocal gated all reject\nlocal all all scram-sha-256\nhost all all 0.0.0.0/0 reject\nhost all all ::0/0 reject\n"
	f.write(filepath.Join(f.data, "pg_hba.conf"), hba, 0600)
	f.command("pg_ctl", nil, "-D", f.data, "reload")
	// pg_reload_conf is asynchronous; fresh denied connections demonstrate the
	// loaded rule instead of trusting a receipt generated from an unapplied file.
	for _, role := range []string{"nmcp_backup_control", "nmcp_backup_dump", "nmcp_migrator", "nmcp_worker"} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, e := pgx.Connect(ctx, f.dsn(role, "gated"))
			if e != nil {
				break
			}
			c.Close(ctx)
			if time.Now().After(deadline) {
				t.Fatal("local routine-role HBA denial not loaded")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	tcpConfig, e := pgx.ParseConfig(f.dsn("operator", "gated"))
	if e != nil {
		t.Fatal("parse negative TCP probe")
	}
	tcpConfig.Host = "127.0.0.1"
	tcpConfig.ConnectTimeout = time.Second
	c, e := pgx.ConnectConfig(ctx, tcpConfig)
	if e == nil {
		c.Close(ctx)
		t.Fatal("TCP operator route unexpectedly available")
	}
	gated := f.pool("operator", "gated")
	si, e := restoreIdentity(ctx, source)
	if e != nil {
		t.Fatal("source restore identity")
	}
	ci, e := restoreIdentity(ctx, gated)
	if e != nil {
		t.Fatal("candidate restore identity")
	}
	hash := sha256.Sum256([]byte(hba))
	// This bit is deliberately FALSE: all credentials are readable by the same
	// current UID. SQL-role/TCP denial is not equivalent to distinct-UID isolation.
	receipt := RestoreReceipt{Source: si, Candidate: ci, Template0: true, Empty: true, RoutineLoginDenied: false, OperatorSocketOnly: true, HBASHA256: hex.EncodeToString(hash[:]), IssuedAt: time.Now()}
	for _, dir := range []string{"ledger", "storage", "receipt"} {
		if os.Mkdir(filepath.Join(f.root, dir), 0700) != nil {
			t.Fatal("create restore private directory")
		}
	}
	receiptFile := filepath.Join(f.root, "receipt", "operator.json")
	raw, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal("encode receipt")
	}
	f.write(receiptFile, string(raw), 0600)
	existing, e := os.ReadFile(f.serviceFile)
	if e != nil {
		t.Fatal("read service fixture")
	}
	f.write(f.serviceFile, string(existing)+fmt.Sprintf("[restore_operator]\nhost=%s\nport=6543\ndbname=gated\nuser=operator\nsslmode=disable\n[restore_api]\nhost=%s\nport=6543\ndbname=gated\nuser=nmcp_api\nsslmode=disable\n", f.socket, f.socket), 0600)
	restorePass := filepath.Join(f.root, "restore.pgpass")
	f.write(restorePass, fmt.Sprintf("localhost:6543:gated:operator:%s\nlocalhost:6543:gated:nmcp_api:%s\n%s:6543:gated:operator:%s\n%s:6543:gated:nmcp_api:%s\n", f.passwords["operator"], f.passwords["nmcp_api"], f.socket, f.passwords["operator"], f.socket, f.passwords["nmcp_api"]), 0600)
	settings := RestoreSettings{RestoreTool: b.RestoreTool, Root: b.Root, LedgerRoot: filepath.Join(f.root, "ledger"), ReceiptFile: receiptFile, Service: "restore_operator", APIService: "restore_api", ServiceFile: f.serviceFile, PassFile: restorePass, StorageRoot: filepath.Join(f.root, "storage"), FileBaseURL: "https://files.example.test/files/"}
	if _, e := service.Restore(ctx, backupID, settings); !errors.Is(e, errRestore) {
		t.Fatal("Service.Restore accepted an unattested same-UID receipt")
	}
	if pg17Scalar(t, ctx, gated, `SELECT count(*)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`) != "0" {
		t.Fatal("rejected Restore mutated fresh candidate")
	}
	t.Log("actual Service.Restore rejects unproven distinct-UID receipt; local routine-role and TCP negatives verified, not full production HBA equivalence")
}

// TestBackupPG17OperatorRestore is deliberately a separate root-only gate.
// Ubuntu CI provisions postgres/nmcp/nmcp-backup; an unprivileged developer run
// must report a skip, never impersonate a second UID or sign a false receipt.
func TestBackupPG17OperatorRestore(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: full real operator Restore NOT executed")
	}
	if os.Geteuid() != 0 {
		t.Skip("full operator Restore requires root and actual postgres/nmcp/nmcp-backup OS identities; same-UID simulation prohibited")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("PG17 binary directory must be absolute")
	}
	for _, name := range []string{"postgres", "nmcp", "nmcp-backup"} {
		u, e := user.Lookup(name)
		if e != nil || u.Uid == "0" {
			t.Fatal("root PG17 gate requires distinct provisioned OS users")
		}
	}
	for _, tool := range []string{"postgres", "pg_dump", "pg_restore"} {
		output, e := exec.Command(filepath.Join(bin, tool), "--version").Output()
		if e != nil || !strings.Contains(string(output), "(PostgreSQL) 17.") {
			t.Fatal("root restore requires actual PostgreSQL 17 binaries")
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	f := newPG17Fixture(t, bin)
	ctx := f.ctx
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrap(false)
	if pg17Scalar(t, ctx, operator, `SELECT count(*)::text FROM pg_roles WHERE rolname IN ('nmcp_backup_control','nmcp_backup_dump')`) != "2" {
		t.Fatal("production backup role bootstrap missing")
	}
	for _, role := range []string{"nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		f.sql(operator, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+f.passwords[role]+"'")
	}
	migrator := f.pool("nmcp_migrator", "source")
	migration, e := database.NewMigrator(migrator)
	if e != nil {
		t.Fatal("create root gate migrator")
	}
	if migration.Up(ctx) != nil {
		t.Fatal("actual production migrator failed")
	}
	f.sql(migrator, `CREATE SCHEMA proof AUTHORIZATION nmcp_migrator; CREATE SEQUENCE proof.serial; CREATE TABLE proof.payload(id bigint PRIMARY KEY DEFAULT nextval('proof.serial'),body text NOT NULL); INSERT INTO proof.payload(body) VALUES ('operator restore data'),('second row')`)
	f.bootstrap(true)
	source := f.pool("operator", "source")
	control := f.pool("nmcp_backup_control", "source")
	root := filepath.Join(f.root, "backups")
	if os.Mkdir(root, 0700) != nil {
		t.Fatal("private backup root")
	}
	b := BackupSettings{Root: root, DumpService: "nmcp_backup_dump", ServiceFile: f.serviceFile, PassFile: f.passfiles["nmcp_backup_dump"], DumpTool: filepath.Join(bin, "pg_dump"), RestoreTool: filepath.Join(bin, "pg_restore")}
	run, e := New(control).Backup(ctx, b, false)
	if e != nil || run.Status != "succeeded" {
		t.Fatal("root gate real restricted Backup failed")
	}
	f.sql(operator, `CREATE DATABASE gated TEMPLATE template0 OWNER nmcp_migrator`)
	f.sql(operator, `REVOKE ALL ON DATABASE gated FROM PUBLIC; GRANT CONNECT ON DATABASE gated TO nmcp_api`)
	// Only the operator UID root maps to these two SQL logins. No routine UID
	// map entry exists; explicit rejects precede the source SCRAM rule.
	f.write(filepath.Join(f.data, "pg_ident.conf"), "operator_test root operator\noperator_test root nmcp_api\n", 0600)
	postgres, _ := user.Lookup("postgres")
	uid, _ := strconv.Atoi(postgres.Uid)
	gid, _ := strconv.Atoi(postgres.Gid)
	if os.Chown(filepath.Join(f.data, "pg_ident.conf"), uid, gid) != nil {
		t.Fatal("peer map ownership")
	}
	hba := "local gated operator peer map=operator_test\nlocal gated nmcp_api peer map=operator_test\nlocal gated all reject\nlocal all all scram-sha-256\nhost all all 0.0.0.0/0 reject\nhost all all ::0/0 reject\n"
	f.write(filepath.Join(f.data, "pg_hba.conf"), hba, 0600)
	f.command("pg_ctl", nil, "-D", f.data, "reload")
	// A successful fresh root peer connection establishes that reload completed.
	peerDSN := "host=" + quoteConnValue(f.socket) + " port=6543 dbname=gated user=operator sslmode=disable"
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, e := pgx.Connect(ctx, peerDSN)
		if e == nil {
			c.Close(ctx)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root-only peer route did not become live")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, osRole := range []string{"nmcp", "nmcp-backup"} {
		for _, sqlRole := range []string{"operator", "nmcp_api", "nmcp_backup_control", "nmcp_backup_dump", "nmcp_migrator", "nmcp_worker"} {
			args := []string{"--reuid=" + osRole, "--regid=" + osRole, "--init-groups", filepath.Join(bin, "psql"), "-X", "--no-password", "--host=" + f.socket, "--port=6543", "--dbname=gated", "--username=" + sqlRole, "--command=SELECT 1"}
			cmd := exec.CommandContext(ctx, "/usr/bin/setpriv", args...)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
			var diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			cmd.Stdout = io.Discard
			if cmd.Run() == nil {
				t.Fatal("routine UID reached operator-gated candidate")
			}
			// Do not accept command-not-found, permission-to-exec failures or a bad
			// binary as proof: require an actual PostgreSQL HBA/peer rejection.
			if !strings.Contains(diagnostic.String(), "pg_hba.conf rejects connection") && !strings.Contains(diagnostic.String(), "Peer authentication failed") {
				t.Fatal("routine UID denial was not an actual PostgreSQL HBA rejection")
			}
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/setpriv", "--reuid="+osRole, "--regid="+osRole, "--init-groups", "/usr/bin/test", "-r", f.passfiles["operator"])
		if e := cmd.Run(); e == nil {
			t.Fatal("routine UID can read operator passfile")
		} else if x, ok := e.(*exec.ExitError); !ok || x.ExitCode() != 1 {
			t.Fatal("operator credential isolation probe failed unexpectedly")
		}
	}
	// Explicit actual negative TCP operator probe, not just listen_addresses text.
	tcp, e := pgx.ParseConfig(peerDSN)
	if e != nil {
		t.Fatal("TCP probe config")
	}
	tcp.Host = "127.0.0.1"
	tcp.ConnectTimeout = time.Second
	if c, e := pgx.ConnectConfig(ctx, tcp); e == nil {
		c.Close(ctx)
		t.Fatal("operator TCP connection allowed")
	}
	// Do not open a pool connection to candidate before Restore's empty-database
	// check: it correctly rejects other live candidate sessions.
	sourceID, e := restoreIdentity(ctx, source)
	if e != nil {
		t.Fatal("source identity")
	}
	temporary, e := pgxpool.New(ctx, peerDSN)
	if e != nil {
		t.Fatal("candidate identity pool")
	}
	candidateID, e := restoreIdentity(ctx, temporary)
	temporary.Close()
	if e != nil {
		t.Fatal("candidate identity")
	}
	hash := sha256.Sum256([]byte(hba))
	receipt := RestoreReceipt{Source: sourceID, Candidate: candidateID, Template0: true, Empty: true, RoutineLoginDenied: true, OperatorSocketOnly: true, HBASHA256: hex.EncodeToString(hash[:]), IssuedAt: time.Now()}
	for _, name := range []string{"ledger", "storage", "receipt"} {
		if os.Mkdir(filepath.Join(f.root, name), 0700) != nil {
			t.Fatal("private restore directories")
		}
	}
	encoded, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal("receipt encoding")
	}
	receiptFile := filepath.Join(f.root, "receipt", "operator.json")
	f.write(receiptFile, string(encoded), 0600)
	existing, e := os.ReadFile(f.serviceFile)
	if e != nil {
		t.Fatal("service fixture")
	}
	f.write(f.serviceFile, string(existing)+fmt.Sprintf("[restore_operator]\nhost=%s\nport=6543\ndbname=gated\nuser=operator\nsslmode=disable\n[restore_api]\nhost=%s\nport=6543\ndbname=gated\nuser=nmcp_api\nsslmode=disable\n", f.socket, f.socket), 0600)
	restorePass := filepath.Join(f.root, "operator-restore.pgpass")
	f.write(restorePass, "# peer-only candidate, no reusable restore password\n", 0600)
	settings := RestoreSettings{Root: b.Root, LedgerRoot: filepath.Join(f.root, "ledger"), ReceiptFile: receiptFile, RestoreTool: b.RestoreTool, Service: "restore_operator", APIService: "restore_api", ServiceFile: f.serviceFile, PassFile: restorePass, StorageRoot: filepath.Join(f.root, "storage"), FileBaseURL: "https://files.example.test/files/"}
	op, e := New(source).Restore(ctx, run.ID, settings)
	if e != nil || op.Phase != "validated" {
		t.Fatalf("actual Service.Restore failed at sanitized phase %s", op.FailedPhase)
	}
	durable, e := LoadRestoreOperation(settings.LedgerRoot, op.ID)
	if e != nil || durable.Phase != "validated" || durable.Candidate != candidateID || durable.RTO <= 0 || durable.RPO < 0 {
		t.Fatal("full Restore durable identity/metrics journal missing")
	}
	restored, e := pgxpool.New(ctx, peerDSN)
	if e != nil {
		t.Fatal("restored operator pool")
	}
	defer restored.Close()
	for _, query := range pg17CatalogProofQueries {
		if pg17Scalar(t, ctx, source, query) != pg17Scalar(t, ctx, restored, query) {
			t.Fatal("full Service.Restore schema/owner/ACL/definer/sequence/data inequality")
		}
	}
	var restoredMode, sourceMode string
	if source.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1`).Scan(&sourceMode) != nil || restored.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1`).Scan(&restoredMode) != nil || sourceMode != "maintenance" || restoredMode != "maintenance" {
		t.Fatal("full Restore did not keep source and candidate fenced")
	}
	t.Logf("actual Service.Restore validated: root peer operator, routine UID/HBA/TCP negatives, actual nmcp_api HTTP proof, all-owner equality; rpo_ms=%d rto_ms=%d", op.RPO.Milliseconds(), op.RTO.Milliseconds())
}

func pg17CommitUnknownAndRetention(t *testing.T, f *pg17Fixture, source *pgxpool.Pool, service *Service, b BackupSettings, oldID string) {
	t.Helper()
	calls := 0
	service.afterBackupCommit = func(ctx context.Context) error {
		calls++
		// The causal seam may run only after the actual durable success exists.
		var successes int
		if source.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE status='succeeded'`).Scan(&successes) != nil || successes < 2 {
			t.Fatal("lost acknowledgement seam preceded real commit")
		}
		return errors.New("lost success acknowledgement")
	}
	latest, err := service.Backup(f.ctx, b, false)
	service.afterBackupCommit = nil
	if err != nil || latest.Status != "succeeded" || calls != 1 {
		t.Fatal("real committed backup was not reconciled after acknowledgement loss")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='backup_run' AND $1::uuid=ANY(affected_ids) AND outcome='succeeded'`, latest.ID) != "1" {
		t.Fatal("committed success audit duplicated or missing after acknowledgement loss")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='backup_run' AND $1::uuid=ANY(affected_ids) AND outcome='failed'`, latest.ID) != "0" {
		t.Fatal("committed success was incorrectly rewritten as failure")
	}
	f.sql(source, `UPDATE backup_runs SET started_at=clock_timestamp()-interval '40 days' WHERE id=$1`, oldID)
	f.sql(source, `UPDATE backup_runs SET started_at=clock_timestamp()-interval '31 days' WHERE id=$1`, latest.ID)
	beforeSnapshot := pg17Scalar(t, f.ctx, source, `SELECT config_snapshot::text||manifest::text FROM backup_runs WHERE id=$1`, latest.ID)
	// Corrupt or remove the newest dump while an older verified artifact is
	// expired. A succeeded history ID alone must never justify deleting it.
	newestDump := filepath.Join(b.Root, latest.ID, "dump")
	original, readErr := os.ReadFile(newestDump)
	if readErr != nil || len(original) == 0 {
		t.Fatal("read real keeper bytes")
	}
	for _, fault := range []string{"corrupt", "missing"} {
		if fault == "corrupt" {
			file, openErr := os.OpenFile(newestDump, os.O_WRONLY, 0)
			if openErr != nil {
				t.Fatal("inject keeper corruption")
			}
			_, writeErr := file.WriteAt([]byte{original[0] ^ 0xff}, 0)
			syncErr := file.Sync()
			closeErr := file.Close()
			if writeErr != nil || syncErr != nil || closeErr != nil {
				t.Fatal("persist keeper corruption")
			}
		} else if os.Rename(newestDump, newestDump+".missing") != nil {
			t.Fatal("inject missing keeper")
		}
		refused, cleanupErr := service.CleanupBackups(f.ctx, b, false)
		if cleanupErr == nil || len(refused.Deleted) != 0 {
			t.Fatal("cleanup deleted older good backup with an unusable newest keeper")
		}
		oldStore, openErr := backupartifact.Open(b.Root, backupartifact.Options{ReadOnly: true})
		if openErr != nil {
			t.Fatal("verify old keeper protection")
		}
		_, verifyErr := oldStore.Verify(oldID)
		oldStore.Close()
		if verifyErr != nil {
			t.Fatal("older usable artifact lost on invalid newest keeper")
		}
		if fault == "missing" {
			if os.Rename(newestDump+".missing", newestDump) != nil {
				t.Fatal("restore keeper path")
			}
		} else {
			file, openErr := os.OpenFile(newestDump, os.O_WRONLY, 0)
			if openErr != nil {
				t.Fatal("restore keeper bytes")
			}
			_, writeErr := file.WriteAt(original, 0)
			syncErr := file.Sync()
			closeErr := file.Close()
			if writeErr != nil || syncErr != nil || closeErr != nil {
				t.Fatal("persist restored keeper bytes")
			}
		}
	}
	cleanup, err := service.CleanupBackups(f.ctx, b, false)
	if err != nil || len(cleanup.Deleted) != 1 || cleanup.Deleted[0] != oldID {
		t.Fatal("retention did not remove exactly the old success")
	}
	store, err := backupartifact.Open(b.Root, backupartifact.Options{ReadOnly: true})
	if err != nil {
		t.Fatal("retention proof root")
	}
	defer store.Close()
	if _, err = store.Verify(latest.ID); err != nil {
		t.Fatal("retention removed last aged successful backup")
	}
	if _, err = store.Verify(oldID); err == nil {
		t.Fatal("retention left deleted bytes")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT config_snapshot::text||manifest::text FROM backup_runs WHERE id=$1`, latest.ID) != beforeSnapshot {
		t.Fatal("retention mutated immutable effective config or manifest")
	}
	again, err := service.CleanupBackups(f.ctx, b, false)
	if err != nil || len(again.Deleted) != 0 {
		t.Fatal("retention did not converge while preserving the last success")
	}
	t.Log("actual durable commit acknowledgement loss reconciled; actual aged last-one retention preserved")
}

func TestBackupPG17LifecycleMigration(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: actual lifecycle migration NOT executed")
	}
	f := newPG17Fixture(t, bin)
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrap(false)
	f.sql(operator, "ALTER ROLE nmcp_migrator PASSWORD '"+f.passwords["nmcp_migrator"]+"'")
	f.sql(operator, "ALTER ROLE nmcp_api PASSWORD '"+f.passwords["nmcp_api"]+"'")
	migrator := f.pool("nmcp_migrator", "source")
	m, err := database.NewMigrator(migrator)
	if err != nil {
		t.Fatal("migration constructor")
	}
	if err = m.Up(f.ctx); err != nil {
		t.Fatalf("actual PG17 latest migration failed: %v", err)
	}
	status, err := m.Status(f.ctx)
	if err != nil || !status.Ready() || status.CurrentVersion != 23 {
		t.Fatal("actual PG17 lifecycle migration not ready")
	}
	tx, err := migrator.Begin(f.ctx)
	if err != nil {
		t.Fatal("begin normal fence")
	}
	if err = backupNormal(f.ctx, tx); err != nil {
		rollback(tx)
		t.Fatal("actual closed fence capability failed")
	}
	rollback(tx)
	api := f.pool("nmcp_api", "source")
	if _, err = api.Exec(f.ctx, `SELECT nmcp_backup_normal_fence()`); err == nil {
		t.Fatal("PUBLIC/API may invoke backup-only fence")
	}
	t.Log("actual PostgreSQL17 latest migration23 and closed normal-fence capability passed; no backup-role substitute grants")
}

func pg17FailureJournalOracles(t *testing.T, f *pg17Fixture, source *pgxpool.Pool, service *Service, b BackupSettings) {
	t.Helper()
	runningAck := 0
	service.afterBackupRunningCommit = func(ctx context.Context) error {
		runningAck++
		if pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs WHERE status='running'`) != "1" {
			t.Fatal("initial commit acknowledgement seam preceded actual commit")
		}
		return errors.New("lost initial history commit acknowledgement")
	}
	initial, err := service.Backup(f.ctx, b, false)
	service.afterBackupRunningCommit = nil
	if err == nil || runningAck != 1 || initial.Status != "failed" || initial.Notification != "succeeded" {
		t.Fatal("initial committed running history lost reply was not independently failed and notified")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM backup_runs WHERE status='running'`) != "0" {
		t.Fatal("initial history acknowledgement loss stranded a running row")
	}
	f.sql(source, `CREATE FUNCTION proof.reject_backup_failure_audit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.command='backup_run' AND NEW.outcome='failed' THEN RAISE EXCEPTION 'causal audit rejection' USING ERRCODE='23514'; END IF; RETURN NEW; END;$$; ALTER FUNCTION proof.reject_backup_failure_audit() OWNER TO nmcp_migrator; CREATE TRIGGER reject_backup_failure_audit BEFORE INSERT ON admin_audit FOR EACH ROW EXECUTE FUNCTION proof.reject_backup_failure_audit()`)
	broken := b
	broken.DumpTool = filepath.Join(f.root, "missing-pg_dump")
	failed, err := service.Backup(f.ctx, broken, false)
	if err == nil || failed.Status != "failed" || failed.Notification != "succeeded" || !failed.AuditPending {
		t.Fatal("rejected audit rolled back mandatory failure/notification/retry evidence")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM backup_runs WHERE status='running'`) != "0" {
		t.Fatal("rejected audit stranded running state")
	}
	f.sql(source, `DROP TRIGGER reject_backup_failure_audit ON admin_audit; DROP FUNCTION proof.reject_backup_failure_audit()`)
	another, err := service.Backup(f.ctx, broken, false)
	if err == nil || another.Status != "failed" {
		t.Fatal("broken tool unexpectedly succeeded")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT failure_audit_pending::text FROM backup_runs WHERE id=$1`, failed.ID) != "false" {
		t.Fatal("durable failed audit retry did not converge")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='backup_run' AND $1::uuid=ANY(affected_ids) AND outcome='failed'`, failed.ID) != "1" {
		t.Fatal("retry omitted or duplicated failure audit")
	}
	t.Log("actual initial-commit acknowledgement loss and rejected audit preserve failed history, notification and durable retry")
}

func pg17LockLossNoRivalHistory(t *testing.T, f *pg17Fixture, source *pgxpool.Pool, service *Service, b BackupSettings) {
	t.Helper()
	called := 0
	service.afterBackupRunningCommit = func(ctx context.Context) error {
		called++
		var pid int
		if source.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&pid) != nil {
			t.Fatal("locate exact backup lock backend")
		}
		var signalled bool
		if source.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&signalled) != nil || !signalled {
			t.Fatal("terminate real backup lock backend")
		}
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var present bool
			if source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory')`, pid).Scan(&present) != nil {
				t.Fatal("observe terminated lock backend")
			}
			if !present {
				break
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("terminated backup lock not released")
			}
		}
		before := pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`)
		competitor, err := New(service.Pool).Backup(ctx, b, false)
		if !errors.Is(err, ErrConflict) || competitor.Outcome != "overlap_conflict" || competitor.ID != "" {
			t.Fatal("rival ignored filesystem operation fence after PG lock loss")
		}
		if pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs`) != before || pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM backup_runs WHERE status='running'`) != "1" {
			t.Fatal("rival falsely recovered or mutated surviving original history")
		}
		return errors.New("actual lock session terminated")
	}
	original, err := service.Backup(f.ctx, b, false)
	service.afterBackupRunningCommit = nil
	if err == nil || original.Status != "failed" || called != 1 {
		t.Fatal("real lost-lock original did not independently journal failure")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT count(*)::text FROM backup_runs WHERE status='running'`) != "0" {
		t.Fatal("lost lock left running history")
	}
	t.Log("actual PG advisory backend terminated; independent root lock blocked rival history/recovery until failure reconciliation")
}

func pg17InterruptedRealDump(t *testing.T, f *pg17Fixture, source *pgxpool.Pool, service *Service, b BackupSettings) {
	t.Helper()
	marker := filepath.Join(f.root, "real-dump-prefix-observed")
	faultTool := filepath.Join(f.root, "interrupt-real-pg_dump")
	script := fmt.Sprintf(`#!/usr/bin/python3
import os, subprocess, sys
real = %q
if sys.argv[1:] == ['--version']:
    os.execve(real, [real, '--version'], os.environ)
p = subprocess.Popen([real, *sys.argv[1:]], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=os.environ)
try:
    prefix = p.stdout.read(4096)
    if len(prefix) != 4096 or prefix[:5] != b'PGDMP':
        raise RuntimeError('real custom dump prefix not observed')
    os.write(1, prefix)
    with open(%q, 'x') as receipt:
        receipt.write('PGDMP:4096')
finally:
    p.terminate()
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait(timeout=5)
sys.exit(24)
`, b.DumpTool, marker)
	f.write(faultTool, script, 0700)
	broken := b
	broken.DumpTool = faultTool
	before := pg17Scalar(t, f.ctx, source, `SELECT max(started_at)::text FROM backup_runs WHERE status='succeeded'`)
	failed, err := service.Backup(f.ctx, broken, false)
	observed, readErr := os.ReadFile(marker)
	if err == nil || failed.Status != "failed" || failed.Notification != "succeeded" || readErr != nil || string(observed) != "PGDMP:4096" {
		t.Fatal("real pg_dump stream interruption did not preserve failed history and notification")
	}
	if pg17Scalar(t, f.ctx, source, `SELECT max(started_at)::text FROM backup_runs WHERE status='succeeded'`) != before {
		t.Fatal("interrupted dump advanced last success")
	}
	for _, path := range []string{filepath.Join(b.Root, failed.ID), filepath.Join(b.Root, ".tmp-"+failed.ID)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatal("interrupted real dump exposed successful or abandoned staging bytes")
		}
	}
	t.Log("actual restricted pg_dump emitted 4096-byte custom stream then was terminated; no success publish/history and durable failure notification")
}
