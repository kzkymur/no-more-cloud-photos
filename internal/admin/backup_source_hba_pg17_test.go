package admin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

// Real authentication proof, not a credential-missing or database ACL denial.
// This exercises the policy renderer; production root discovery/CAS installation
// has its own metadata counterexamples and is not claimed executed by this test.
func TestBackupPG17SourceOnlyHBA(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("actual PG17 source-only HBA proof not executed: bindir absent")
	}
	f := newPG17Fixture(t, bin)
	ctx := f.ctx
	admin := f.pool("operator", "postgres")
	f.sql(admin, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrap(false)
	for _, role := range []string{"nmcp_migrator", "nmcp_backup_dump", "nmcp_backup_control"} {
		f.sql(admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+f.passwords[role]+"'")
	}
	migration := f.pool("nmcp_migrator", "source")
	m, err := database.NewMigrator(migration)
	if err != nil {
		t.Fatal(err)
	}
	if m.Up(ctx) != nil {
		t.Fatal("actual migration fixture")
	}
	migration.Close()
	f.bootstrap(true)
	if pg17Scalar(t, ctx, admin, `SELECT (has_database_privilege('nmcp_backup_dump','postgres','CONNECT') AND has_database_privilege('nmcp_backup_control','template1','CONNECT'))::text`) != "true" {
		t.Fatal("negative would be database ACL, not HBA")
	}
	// Assert Python operator installer and Go promotion render the SAME bytes.
	script := filepath.Join(pg17Repository(t), "scripts/configure-backup-source-hba")
	command := exec.CommandContext(ctx, "/usr/bin/python3", "-c", "import runpy,sys;sys.stdout.write(runpy.run_path(sys.argv[1])['block']('source'))", script)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PYTHONDONTWRITEBYTECODE=1"}
	rendered, err := command.Output()
	if err != nil || string(rendered) != backupSourceHBABlock("source") {
		t.Fatal("operator/promotion HBA policy differs")
	}
	admin.Close()
	// Change only this owned disposable server's listener before restart.
	confPath := filepath.Join(f.data, "postgresql.auto.conf")
	raw, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "listen_addresses = ''", "listen_addresses = '127.0.0.1,::1'"))
	f.write(confPath, string(raw), 0600)
	hba := string(rendered) + "local all all scram-sha-256\nhost all all 127.0.0.1/32 scram-sha-256\nhost all all ::1/128 scram-sha-256\n"
	f.write(filepath.Join(f.data, "pg_hba.conf"), hba, 0600)
	f.command("pg_ctl", nil, "-D", f.data, "-l", filepath.Join(f.root, "restart.log"), "-m", "fast", "-w", "restart")
	pid, err := os.ReadFile(filepath.Join(f.data, "postmaster.pid"))
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "owned.pid"), strings.SplitN(string(pid), "\n", 2)[0], 0600)
	for _, role := range []string{"nmcp_backup_dump", "nmcp_backup_control"} {
		for _, host := range []string{f.socket, "127.0.0.1", "::1"} {
			for _, db := range []string{"source", "postgres", "template1"} {
				cfg, err := pgx.ParseConfig(f.dsn(role, "source"))
				if err != nil {
					t.Fatal(err)
				}
				cfg.Host = host
				cfg.Database = db
				cfg.Password = f.passwords[role]
				cfg.Port = 6543
				conn, err := pgx.ConnectConfig(ctx, cfg)
				if db == "source" {
					if err != nil {
						t.Fatalf("allowed actual source authentication failed role=%s host=%s", role, host)
					}
					var user string
					if conn.QueryRow(ctx, `SELECT session_user`).Scan(&user) != nil || user != role {
						t.Fatal("wrong source login")
					}
					conn.Close(context.Background())
					continue
				}
				if conn != nil {
					conn.Close(context.Background())
				}
				var denied *pgconn.PgError
				if !errors.As(err, &denied) || denied.Code != "28000" || !strings.Contains(denied.Message, "pg_hba.conf rejects connection") {
					t.Fatalf("other database not explicitly HBA-rejected role=%s host=%s db=%s", role, host, db)
				}
			}
		}
	}
	current, err := strconv.Atoi(strings.SplitN(string(pid), "\n", 2)[0])
	if err != nil || current <= 0 {
		t.Fatal("owned restarted postmaster")
	}
	t.Log("actual PG17 source allowed and PUBLIC-CONNECT postgres/template1 rejected by HBA28000 for both backup roles over Unix/IPv4/IPv6; no missing-credential or ACL proxy")
}

func pg17AssertBackupOtherDBHBA(t *testing.T, ctx context.Context, passwords map[string]string, socket string, port uint16, allowed string) {
	t.Helper()
	for _, role := range []string{"nmcp_backup_dump", "nmcp_backup_control"} {
		for _, host := range []string{socket, "127.0.0.1"} {
			for _, db := range []string{allowed, "postgres", "template1"} {
				cfg, err := pgx.ParseConfig("host=" + quoteConnValue(host) + " port=" + strconv.Itoa(int(port)) + " dbname=" + quoteConnValue(db) + " user=" + role + " sslmode=disable passfile='/dev/null'")
				if err != nil {
					t.Fatal("actual role witness config")
				}
				cfg.Password = passwords[role]
				conn, err := pgx.ConnectConfig(ctx, cfg)
				if db == allowed {
					if err != nil {
						t.Fatal("promoted backup source login failed")
					}
					conn.Close(ctx)
					continue
				}
				if conn != nil {
					conn.Close(ctx)
				}
				var rejected *pgconn.PgError
				if !errors.As(err, &rejected) || rejected.Code != "28000" || !strings.Contains(rejected.Message, "pg_hba.conf rejects connection") {
					t.Fatal("promoted backup role escaped source-only HBA")
				}
			}
		}
	}
}
