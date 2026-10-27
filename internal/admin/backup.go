package admin

// Backup orchestration deliberately keeps the dump identity separate from the
// control connection. A session lock spans the external tool, not a long SQL
// transaction; history commits never precede durable artifact publication.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgservicefile"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

var errBackup = errors.New("backup operation failed")
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type BackupSettings struct {
	Root        string
	DumpService string
	ServiceFile string
	PassFile    string
	DumpTool    string
	RestoreTool string
	NotifyHook  string
}

func backupSettings() (BackupSettings, error) {
	b := BackupSettings{Root: os.Getenv("NMCP_BACKUP_ROOT"), DumpService: os.Getenv("NMCP_BACKUP_DUMP_SERVICE"), ServiceFile: os.Getenv("PGSERVICEFILE"), PassFile: os.Getenv("NMCP_BACKUP_DUMP_PASSFILE"), DumpTool: "/usr/bin/pg_dump", RestoreTool: "/usr/bin/pg_restore", NotifyHook: os.Getenv("NMCP_BACKUP_NOTIFY_HOOK")}
	if !filepath.IsAbs(b.Root) || !serviceName.MatchString(b.DumpService) || !filepath.IsAbs(b.ServiceFile) || !filepath.IsAbs(b.PassFile) {
		return b, ErrValidation
	}
	for name, target := range map[string]*string{"NMCP_BACKUP_PG_DUMP": &b.DumpTool, "NMCP_BACKUP_PG_RESTORE": &b.RestoreTool} {
		if value := os.Getenv(name); value != "" {
			if !filepath.IsAbs(value) {
				return b, ErrValidation
			}
			*target = value
		}
	}
	return b, nil
}

func (b BackupSettings) toolEnv() []string {
	return []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "PGSERVICE=" + b.DumpService, "PGSERVICEFILE=" + b.ServiceFile, "PGPASSFILE=" + b.PassFile}
}

// tool discards raw stderr, which may include service addresses or credentials.
// Only the fixed phase error is persisted or printed by callers.
func (b BackupSettings) tool(ctx context.Context, path string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = b.toolEnv()
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return errBackup
	}
	return nil
}

type BackupRun struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Outcome      string     `json:"outcome,omitempty"`
	Notification string     `json:"notification_status"`
	ErrorCode    *string    `json:"error_code,omitempty"`
	AuditPending bool       `json:"audit_pending,omitempty"`
}

func (s *Service) BackupList(ctx context.Context) ([]BackupRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text,status,started_at,finished_at,notification_status,error_code,failure_audit_pending FROM backup_runs ORDER BY started_at DESC,id DESC LIMIT 1000`)
	if err != nil {
		return nil, errBackup
	}
	defer rows.Close()
	result := []BackupRun{}
	for rows.Next() {
		var r BackupRun
		if err := rows.Scan(&r.ID, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Notification, &r.ErrorCode, &r.AuditPending); err != nil {
			return nil, errBackup
		}
		result = append(result, r)
	}
	if rows.Err() != nil {
		return nil, errBackup
	}
	return result, nil
}

// backupDue uses database time, the current typed interval, and the snapshot
// cutoff of the last committed success (not the timer schedule or local clock).
func backupDue(ctx context.Context, tx pgx.Tx, ifDue bool) (Configuration, time.Time, bool, error) {
	var c Configuration
	var now time.Time
	var due bool
	err := tx.QueryRow(ctx, `SELECT deleted_media_retention_days,superseded_rendition_retention_days,default_timezone,db_backup_interval_hours,db_backup_retention_days,clock_timestamp(),
 NOT EXISTS(SELECT 1 FROM backup_runs WHERE status='succeeded' AND started_at > clock_timestamp()-make_interval(hours=>system_config.db_backup_interval_hours)) FROM system_config WHERE id=1`).Scan(&c.DeletedMediaRetentionDays, &c.SupersededRenditionRetentionDays, &c.DefaultTimezone, &c.DBBackupIntervalHours, &c.DBBackupRetentionDays, &now, &due)
	return c, now, !ifDue || due, err
}

func (s *Service) backupFailure(id, phase string, b BackupSettings) error {
	// Cancellation must not cancel the independent durable failure journal.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return errBackup
	}
	defer rollback(tx)
	var started time.Time
	err = tx.QueryRow(ctx, `UPDATE backup_runs SET status='failed',finished_at=clock_timestamp(),error_code=$2,error_message='backup failed; inspect sanitized phase',notification_status='pending',notification_error=NULL WHERE id=$1 AND status='running' RETURNING started_at`, id, phase).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		rollback(tx)
		var status string
		if s.Pool.QueryRow(ctx, `SELECT status FROM backup_runs WHERE id=$1`, id).Scan(&status) == nil && status == "succeeded" {
			return nil
		}
		return errBackup
	}
	if err != nil {
		return errBackup
	}
	// A rejected audit must not roll back the mandatory failure journal or skip
	// notification. The retry flag is durable in the same mandatory transaction.
	if _, err = tx.Exec(ctx, `SAVEPOINT backup_failure_audit`); err != nil {
		return errBackup
	}
	auditErr := s.audit(ctx, tx, "backup_run", map[string]string{"phase": phase}, []string{id}, "failed", started)
	if auditErr != nil {
		if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT backup_failure_audit`); err != nil {
			return errBackup
		}
		if _, err = tx.Exec(ctx, `UPDATE backup_runs SET failure_audit_pending=true WHERE id=$1`, id); err != nil {
			return errBackup
		}
	}
	if _, err = tx.Exec(ctx, `RELEASE SAVEPOINT backup_failure_audit`); err != nil {
		return errBackup
	}
	if err = tx.Commit(ctx); err != nil {
		return errBackup
	}

	return s.backupNotify(ctx, id, phase, b)
}

func (s *Service) backupNotify(ctx context.Context, id, phase string, b BackupSettings) error {
	status := "failed"
	if filepath.IsAbs(b.NotifyHook) {
		hookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(hookCtx, b.NotifyHook)
		// No inherited credentials, shell, user-controlled argv, or raw tool output.
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
		metadata, _ := json.Marshal(map[string]string{"backup_id": id, "phase": phase, "status": "failed"})
		cmd.Stdin = strings.NewReader(string(metadata))
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		if cmd.Run() == nil {
			status = "succeeded"
		}
	}
	var note any
	if status == "failed" {
		note = "notification hook missing or failed"
	}
	_, err := s.Pool.Exec(ctx, `UPDATE backup_runs SET notification_status=$2,notification_error=$3 WHERE id=$1 AND status='failed'`, id, status, note)
	if err != nil {
		return errBackup
	}
	return errBackup
}

// inventory rejects unsupported RLS/large objects and proves that the dedicated
// login can read every non-system relation, including all definer-owned data.
func backupInventory(ctx context.Context, conn *pgx.Conn) (string, error) {
	var role string
	var unsafe bool
	var version string
	err := conn.QueryRow(ctx, `SELECT current_user,current_setting('server_version'),
 EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls))
 OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user))
 OR EXISTS(SELECT 1 FROM pg_largeobject_metadata)
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp' AND (c.relkind='f' OR r.rolname NOT IN ('nmcp_migrator','nmcp_purge_function_owner','nmcp_check_function_owner','nmcp_repair_function_owner')))
 OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_roles r ON r.oid=p.proowner WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp' AND r.rolname NOT IN ('nmcp_migrator','nmcp_purge_function_owner','nmcp_check_function_owner','nmcp_repair_function_owner'))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp' AND c.relkind IN ('r','p','m','S') AND (c.relrowsecurity OR NOT has_schema_privilege(current_user,n.oid,'USAGE') OR CASE WHEN c.relkind='S' THEN NOT has_sequence_privilege(current_user,c.oid,'SELECT') ELSE NOT has_table_privilege(current_user,c.oid,'SELECT') END))`).Scan(&role, &version, &unsafe)
	if err != nil || unsafe || role != "nmcp_backup_dump" {
		return "", errBackup
	}
	return version, nil
}

var postgresToolVersion = regexp.MustCompile(`^pg_(?:dump|restore) \(PostgreSQL\) ([0-9]+)(?:\.[0-9]+)*(?:[ \n]|$)`)

func toolMajor(s string) string {
	match := postgresToolVersion.FindStringSubmatch(strings.TrimSpace(s))
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func (b BackupSettings) versions(ctx context.Context, server string) (string, error) {
	var output limitedOutput
	if err := b.tool(ctx, b.DumpTool, []string{"--version"}, &output); err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(output))
	if toolMajor(version) != strings.SplitN(server, ".", 2)[0] {
		return "", errBackup
	}
	return version, nil
}

type limitedOutput []byte

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(*b)+len(p) > 4096 {
		return 0, fmt.Errorf("tool metadata exceeds limit")
	}
	*b = append(*b, p...)
	return len(p), nil
}

// dumpConnect resolves credentials from the dump-only passfile. It must never
// borrow PGPASSFILE, PGPASSWORD, or a password embedded in the control DSN.
func (b BackupSettings) dumpConnect(ctx context.Context) (*pgx.Conn, error) {
	info, err := os.Lstat(b.PassFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errBackup
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errBackup
	}
	// Reject ambient libpq overrides rather than allow the Go inventory and
	// scrubbed external tool to inspect different servers or identities.
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		if strings.HasPrefix(key, "PG") && key != "PGPASSFILE" && key != "PGSERVICEFILE" {
			return nil, errBackup
		}
	}
	services, err := pgservicefile.ReadServicefile(b.ServiceFile)
	if err != nil {
		return nil, errBackup
	}
	service, err := services.GetService(b.DumpService)
	if err != nil {
		return nil, errBackup
	}
	allowed := map[string]bool{"host": true, "port": true, "dbname": true, "user": true, "sslmode": true, "sslrootcert": true, "sslcert": true, "sslkey": true, "connect_timeout": true, "application_name": true}
	for key := range service.Settings {
		if !allowed[key] {
			return nil, errBackup
		}
	}
	for _, key := range []string{"host", "port", "dbname", "user", "sslmode"} {
		if service.Settings[key] == "" {
			return nil, errBackup
		}
	}
	cfg, err := pgx.ParseConfig("service=" + b.DumpService + " servicefile=" + quoteConnValue(b.ServiceFile) + " passfile=" + quoteConnValue(b.PassFile))
	if err != nil {
		return nil, errBackup
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, errBackup
	}
	return conn, nil
}

type backupDBIdentity struct {
	Name    string
	OID     uint32
	Address string
	Port    int
	Started time.Time
}

const backupIdentitySQL = `SELECT current_database(),(SELECT oid FROM pg_database WHERE datname=current_database()),COALESCE(inet_server_addr()::text,''),COALESCE(inet_server_port(),0),pg_postmaster_start_time()`

func backupIdentity(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (backupDBIdentity, error) {
	var r backupDBIdentity
	err := q.QueryRow(ctx, backupIdentitySQL).Scan(&r.Name, &r.OID, &r.Address, &r.Port, &r.Started)
	return r, err
}

func quoteConnValue(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

// Root administrative cleanup may delete only the fixed dedicated backup UID's
// files. It still cannot publish on that UID's behalf. Routine runs use euid.
func backupArtifactOptions(root string, readOnly bool) (backupartifact.Options, error) {
	opts := backupartifact.Options{ReadOnly: readOnly, Lock: true}
	if os.Geteuid() != 0 {
		return opts, nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		return opts, ErrValidation
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return opts, ErrValidation
	}
	if stat.Uid == 0 {
		return opts, nil
	}
	u, err := user.Lookup("nmcp-backup")
	if err != nil {
		return opts, ErrValidation
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return opts, ErrValidation
	}
	owner := uint32(uid)
	opts.OwnerUID = &owner
	return opts, nil
}

func backupNormal(ctx context.Context, tx pgx.Tx) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT nmcp_backup_normal_fence()`).Scan(&ok); err != nil {
		return classified(err)
	}
	if !ok {
		return ErrConflict
	}
	return nil
}

func (s *Service) retryBackupFailureAudit(ctx context.Context, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return errBackup
	}
	defer rollback(tx)
	var pending bool
	var phase string
	var started time.Time
	if err = tx.QueryRow(ctx, `SELECT failure_audit_pending,COALESCE(error_code,'failed'),started_at FROM backup_runs WHERE id=$1 AND status='failed' FOR UPDATE`, id).Scan(&pending, &phase, &started); err != nil {
		return errBackup
	}
	if !pending {
		return nil
	}
	if err = s.audit(ctx, tx, "backup_run", map[string]string{"phase": phase}, []string{id}, "failed", started); err != nil {
		return errBackup
	}
	if _, err = tx.Exec(ctx, `UPDATE backup_runs SET failure_audit_pending=false WHERE id=$1`, id); err != nil {
		return errBackup
	}
	if tx.Commit(ctx) != nil {
		return errBackup
	}
	return nil
}

func (s *Service) backupResult(ctx context.Context, id string) (BackupRun, error) {
	var r BackupRun
	err := s.Pool.QueryRow(ctx, `SELECT id::text,status,started_at,finished_at,notification_status,error_code,failure_audit_pending FROM backup_runs WHERE id=$1`, id).Scan(&r.ID, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Notification, &r.ErrorCode, &r.AuditPending)
	r.Outcome = r.Status
	return r, err
}
