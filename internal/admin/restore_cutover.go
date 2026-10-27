package admin

// Cutover is deliberately a separate operator action, never a Restore retry or
// a maintenance resume. An interrupted cutover leaves services stopped and both
// databases in maintenance; its append-only journal prevents automatic replay.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgservicefile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"golang.org/x/sys/unix"
)

var cutoverName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var cutoverHBAPath = regexp.MustCompile(`^/etc/postgresql/[0-9]+/[A-Za-z0-9_-]+/pg_hba\.conf$`)
var errCutover = fmt.Errorf("restore cutover failed; inspect external operation ledger; do not resume or restart services")

// Only this closed set of system actions is available to the state machine.
// Tests replace the interface, not production commands, paths, or credentials.
type cutoverActions interface {
	Preflight(context.Context, RestoreOperation) error
	Stop(context.Context) error
	Switch(context.Context, RestoreOperation) error
	VerifyTargets(context.Context, RestoreOperation) error
	Start(context.Context) error
	VerifyLive(context.Context, RestoreOperation) error
}

type cutoverSave func(string, RestoreOperation) error

func runCutover(ctx context.Context, root string, op RestoreOperation, a cutoverActions, save cutoverSave) (RestoreOperation, error) {
	if op.Phase != "validated" || op.Source.ClusterID != op.Candidate.ClusterID || op.Source.DatabaseOID == op.Candidate.DatabaseOID || !cutoverName.MatchString(op.Source.DatabaseName) || !cutoverName.MatchString(op.Candidate.DatabaseName) {
		return op, errCutover
	}
	phase := func(p string) error {
		op.Phase = p
		op.UpdatedAt = time.Now()
		op.RTO = time.Since(op.StartedAt)
		return save(root, op)
	}
	// Authorize once, before even preflight. A crash or failed preflight requires
	// operator investigation, not replay of a partially applied side effect.
	if phase("cutover_authorized") != nil {
		return op, errCutover
	}
	steps := []struct {
		phase string
		run   func() error
	}{
		{"cutover_preflight", func() error { return a.Preflight(ctx, op) }},
		{"cutover_stopped", func() error { return a.Stop(ctx) }},
		{"cutover_switched", func() error { return a.Switch(ctx, op) }},
		{"cutover_targets_verified", func() error { return a.VerifyTargets(ctx, op) }},
		{"cutover_started", func() error { return a.Start(ctx) }},
		{"cutover_verified", func() error { return a.VerifyLive(ctx, op) }},
	}
	for _, step := range steps {
		if e := step.run(); e != nil {
			// In particular, a partial Start/verification must not leave a worker or
			// timer running. Use an independent bounded context even after cancellation.
			stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			_ = a.Stop(stopCtx)
			cancel()
			op.FailedPhase = step.phase
			_ = phase("cutover_failed")
			return op, errCutover
		}
		if phase(step.phase) != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			_ = a.Stop(stopCtx)
			cancel()
			return op, errCutover
		}
	}
	return op, nil
}

// RestoreCutover is root/operator-only. Settings never select systemctl, units,
// live configuration, HBA destinations, or HTTP destinations. No automatic
// resume occurs and no source database is dropped or unfenced.
func (s *Service) RestoreCutover(ctx context.Context, operationID string, c RestoreSettings) (RestoreOperation, error) {
	var op RestoreOperation
	if os.Geteuid() != 0 || s.Pool == nil || c.validate() != nil {
		return op, ErrValidation
	}
	op, e := LoadRestoreOperation(c.LedgerRoot, operationID)
	if e != nil {
		return op, errCutover
	}
	unlock, e := s.sessionLock(ctx, "maintenance")
	if e != nil {
		return op, errCutover
	}
	defer unlock()
	actual, e := restoreIdentity(ctx, s.Pool)
	if e != nil || actual != op.Source {
		return op, errCutover
	}
	target, e := restorePool(ctx, c, c.Service)
	if e != nil {
		return op, errCutover
	}
	defer target.Close()
	sourceConfig, targetConfig := s.Pool.Config().ConnConfig, target.Config().ConnConfig
	// A copied physical cluster may share system_identifier. The fixed dbname
	// rewrite is authorized only for the exact same operator socket endpoint.
	if !filepath.IsAbs(sourceConfig.Host) || sourceConfig.Host != targetConfig.Host || sourceConfig.Port != targetConfig.Port {
		return op, errCutover
	}
	actual, e = restoreIdentity(ctx, target)
	if e != nil || actual != op.Candidate {
		return op, errCutover
	}
	targetUnlock, e := New(target).sessionLock(ctx, "maintenance")
	if e != nil {
		return op, errCutover
	}
	defer targetUnlock()
	for _, p := range []*pgxpool.Pool{s.Pool, target} {
		state, e := New(p).MaintenanceStatus(ctx)
		if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
			return op, errCutover
		}
		graph, e := restoreRoleGraph(ctx, p)
		if e != nil || graph != op.RoleGraphSHA256 {
			return op, errCutover
		}
	}
	a := &systemCutover{source: s.Pool, target: target, settings: c}
	return runCutover(ctx, c.LedgerRoot, op, a, SaveRestoreOperation)
}

type cutoverFile struct {
	path          string
	before, after []byte
	mode          uint32
	uid, gid      int
}
type cutoverProbe struct {
	config    *pgx.ConnConfig
	role, app string
	pass      string
}
type systemCutover struct {
	source, target *pgxpool.Pool
	settings       RestoreSettings
	files          []cutoverFile
	probes         []cutoverProbe
	hba            cutoverFile
	oldPIDs        map[string]int
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	b, e := cmd.Output()
	if e != nil {
		return "", errCutover
	}
	return strings.TrimSpace(string(b)), nil
}
func unitPID(ctx context.Context, unit string) (int, error) {
	s, e := systemctl(ctx, "show", "--property=MainPID", "--value", unit)
	if e != nil {
		return 0, e
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 0 {
		return 0, errCutover
	}
	return n, nil
}

// safeCutoverFile pins every ancestor without symlink traversal. Root and the
// PostgreSQL service owner may own HBA ancestors; neither group nor world may
// write them. Configuration under /etc/nmcp must be root-owned throughout.
func safeCutoverFile(path string, hba bool) (cutoverFile, error) {
	var out cutoverFile
	out.path = path
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return out, errCutover
	}
	pgUID := uint32(0)
	if hba {
		u, e := user.Lookup("postgres")
		if e != nil {
			return out, errCutover
		}
		n, e := strconv.ParseUint(u.Uid, 10, 32)
		if e != nil {
			return out, errCutover
		}
		pgUID = uint32(n)
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return out, errCutover
	}
	defer func() { unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		n, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return out, errCutover
		}
		unix.Close(fd)
		fd = n
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Mode&0022 != 0 || (st.Uid != 0 && (!hba || st.Uid != pgUID)) {
			return out, errCutover
		}
	}
	n, e := unix.Openat(fd, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return out, errCutover
	}
	f := os.NewFile(uintptr(n), path)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(n, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || st.Size > 1<<20 || (st.Uid != 0 && (!hba || st.Uid != pgUID)) {
		return out, errCutover
	}
	if !hba && st.Mode&0077 != 0 {
		return out, errCutover
	}
	b, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil {
		return out, errCutover
	}
	out.before = b
	out.mode = st.Mode & 0777
	out.uid = int(st.Uid)
	out.gid = int(st.Gid)
	return out, nil
}

// replaceCutoverFile performs a content+metadata compare before rename. The
// parent directory is descriptor-pinned and revalidated; all target files are
// fixed production paths. A post-rename failure is not rolled back silently.
func replaceCutoverFile(f cutoverFile) error {
	now, e := safeCutoverFile(f.path, cutoverHBAPath.MatchString(f.path))
	if e != nil || string(now.before) != string(f.before) || now.uid != f.uid || now.gid != f.gid || now.mode != f.mode {
		return errCutover
	}
	// Open the parent again with O_NOFOLLOW on every ancestor, not just the last.
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return errCutover
	}
	defer func() { unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(f.path), "/"), "/") {
		n, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return errCutover
		}
		unix.Close(fd)
		fd = n
	}
	return publishCutoverFile(fd, f, nil)
}

// publishCutoverFile receives an already trusted pinned directory. The narrow
// hook injects syscall-boundary faults in tests only; production always nil.
func publishCutoverFile(fd int, f cutoverFile, fault func(string) error) error {
	check := func() error {
		n, e := unix.Openat(fd, filepath.Base(f.path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if e != nil {
			return errCutover
		}
		file := os.NewFile(uintptr(n), "current")
		defer file.Close()
		var st unix.Stat_t
		if unix.Fstat(n, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(f.uid) || st.Gid != uint32(f.gid) || st.Mode&0777 != f.mode || st.Size > 1<<20 {
			return errCutover
		}
		b, e := io.ReadAll(io.LimitReader(file, 1<<20))
		if e != nil || string(b) != string(f.before) {
			return errCutover
		}
		return nil
	}
	if check() != nil {
		return errCutover
	}
	inject := func(p string) error {
		if fault != nil {
			return fault(p)
		}
		return nil
	}
	name := ".nmcp-cutover-" + newID()
	n, e := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errCutover
	}
	file := os.NewFile(uintptr(n), name)
	if e = inject("write"); e == nil {
		_, e = file.Write(f.after)
	}
	if e == nil {
		e = file.Chown(f.uid, f.gid)
	}
	if e == nil {
		e = file.Chmod(os.FileMode(f.mode))
	}
	if e == nil {
		e = inject("file_sync")
	}
	if e == nil {
		e = file.Sync()
	}
	ce := file.Close()
	if e != nil || ce != nil {
		return errCutover
	}
	if check() != nil || inject("rename") != nil {
		return errCutover
	}
	if unix.Renameat(fd, name, fd, filepath.Base(f.path)) != nil {
		return errCutover
	}
	if inject("dir_sync") != nil || unix.Fsync(fd) != nil {
		return errCutover
	}
	return nil
}

func rewriteCutoverEnv(b []byte, source, candidate, role, app string) ([]byte, *pgx.ConnConfig, error) {
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "PG") || strings.HasPrefix(trim, "export ") || strings.HasPrefix(trim, "NMCP_DATABASE_URL") && !strings.HasPrefix(line, "NMCP_DATABASE_URL=") {
			return nil, nil, errCutover
		}
	}
	found := false
	var config *pgx.ConnConfig
	for n, line := range lines {
		if !strings.HasPrefix(line, "NMCP_DATABASE_URL=") {
			continue
		}
		if found {
			return nil, nil, errCutover
		}
		found = true
		raw := strings.TrimPrefix(line, "NMCP_DATABASE_URL=")
		u, e := url.Parse(raw)
		if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() != role || u.Hostname() != "127.0.0.1" || u.Fragment != "" || u.Path != "/"+source {
			return nil, nil, errCutover
		}
		// No ambient service, passfile, role, host override or multi-host routing.
		q := u.Query()
		for k := range q {
			if k != "sslmode" && k != "connect_timeout" && k != "application_name" {
				return nil, nil, errCutover
			}
		}
		if q.Get("sslmode") != "disable" {
			return nil, nil, errCutover
		}
		if _, ok := u.User.Password(); !ok {
			return nil, nil, errCutover
		}
		u.Path = "/" + candidate
		u.RawPath = ""
		q.Set("application_name", app)
		u.RawQuery = q.Encode()
		config, e = pgx.ParseConfig(u.String())
		if e != nil || len(config.Fallbacks) != 0 || config.Database != candidate || config.User != role {
			return nil, nil, errCutover
		}
		for k := range config.RuntimeParams {
			if k != "application_name" {
				return nil, nil, errCutover
			}
		}
		lines[n] = "NMCP_DATABASE_URL=" + u.String()
	}
	if !found {
		return nil, nil, errCutover
	}
	return []byte(strings.Join(lines, "\n")), config, nil
}
func cutoverApp(id, suffix string) string {
	return "nmcp_restore_" + strings.ReplaceAll(id, "-", "") + "_" + suffix
}

func rewriteCutoverServices(b []byte, op RestoreOperation, host string, port uint16) ([]byte, []cutoverProbe, error) {
	sf, e := pgservicefile.ParseServicefile(strings.NewReader(string(b)))
	if e != nil || len(sf.Services) != 2 {
		return nil, nil, errCutover
	}
	var out strings.Builder
	var probes []cutoverProbe
	seen := map[string]bool{}
	for _, role := range []string{"nmcp_backup_control", "nmcp_backup_dump"} {
		s, e := sf.GetService(role)
		if e != nil || seen[s.Name] {
			return nil, nil, errCutover
		}
		seen[s.Name] = true
		if s.Settings["host"] != host || s.Settings["port"] != strconv.Itoa(int(port)) || s.Settings["dbname"] != op.Source.DatabaseName || s.Settings["user"] != role {
			return nil, nil, errCutover
		}
		for k := range s.Settings {
			if k == "application_name" {
				if !validPriorCutoverApp(s.Settings[k], strings.TrimPrefix(role, "nmcp_")) {
					return nil, nil, errCutover
				}
				continue
			}
			if k != "host" && k != "port" && k != "dbname" && k != "user" && k != "sslmode" {
				return nil, nil, errCutover
			}
		}
		s.Settings["dbname"] = op.Candidate.DatabaseName
		s.Settings["application_name"] = cutoverApp(op.ID, strings.TrimPrefix(role, "nmcp_"))
		fmt.Fprintf(&out, "[%s]\n", role)
		keys := make([]string, 0, len(s.Settings))
		for k := range s.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var dsn strings.Builder
		for _, k := range keys {
			v := s.Settings[k]
			fmt.Fprintf(&out, "%s=%s\n", k, v)
			fmt.Fprintf(&dsn, "%s=%s ", k, cutoverQuote(v))
		}
		out.WriteString("\n")
		pass := "/etc/nmcp/credentials/backup-" + strings.TrimPrefix(role, "nmcp_backup_") + ".pgpass"
		cfg, e := pgx.ParseConfig(dsn.String() + "password='' passfile=" + cutoverQuote(pass))
		if e != nil {
			return nil, nil, errCutover
		}
		for k := range cfg.RuntimeParams {
			if k != "application_name" {
				return nil, nil, errCutover
			}
		}
		probes = append(probes, cutoverProbe{cfg, role, s.Settings["application_name"], pass})
	}
	return []byte(out.String()), probes, nil
}
func cutoverQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}

// One exact credential is accepted, optionally repeated for the two equivalent
// Unix socket lookup aliases. Wildcards, other identities and duplicate aliases
// are never authority to retarget another database or login.
func rewriteCutoverPass(b []byte, source, candidate, role, host string, port uint16, aliases ...string) ([]byte, error) {
	lines := strings.Split(string(b), "\n")
	count := 0
	permitted := map[string]bool{host: true}
	seenHosts := map[string]bool{}
	if len(aliases) > 1 {
		return nil, errCutover
	}
	for _, alias := range aliases {
		if host != "localhost" || !filepath.IsAbs(alias) || filepath.Clean(alias) != alias {
			return nil, errCutover
		}
		permitted[alias] = true
	}
	password := ""
	for n, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var fields []string
		var s strings.Builder
		escaped := false
		for _, r := range l {
			if escaped {
				s.WriteRune(r)
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == ':' {
				fields = append(fields, s.String())
				s.Reset()
			} else {
				s.WriteRune(r)
			}
		}
		if escaped {
			return nil, errCutover
		}
		fields = append(fields, s.String())
		if len(fields) != 5 || (!permitted[fields[0]] || seenHosts[fields[0]]) || fields[1] != strconv.Itoa(int(port)) || fields[2] != source || fields[3] != role || fields[4] == "" {
			return nil, errCutover
		}
		if count > 0 && fields[4] != password {
			return nil, errCutover
		}
		password = fields[4]
		seenHosts[fields[0]] = true
		count++
		fields[2] = candidate
		for i := range fields {
			fields[i] = strings.ReplaceAll(strings.ReplaceAll(fields[i], "\\", "\\\\"), ":", "\\:")
		}
		lines[n] = strings.Join(fields, ":")
	}
	if count < 1 || count > len(permitted) {
		return nil, errCutover
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func cutoverHBA(b []byte, op RestoreOperation) []byte {
	roles := "nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control"
	var out strings.Builder
	fmt.Fprintf(&out, "# nmcp explicit cutover %s; source remains fenced\n", op.ID)
	for _, database := range []string{op.Source.DatabaseName, op.Candidate.DatabaseName} {
		db := `"` + database + `"`
		if database == op.Candidate.DatabaseName {
			fmt.Fprintf(&out, "local %s %s scram-sha-256\nhost %s %s 127.0.0.1/32 scram-sha-256\nhost %s %s ::1/128 scram-sha-256\n", db, roles, db, roles, db, roles)
		}
		fmt.Fprintf(&out, "local %s %s reject\nhost %s %s 0.0.0.0/0 reject\nhost %s %s ::/0 reject\n", db, roles, db, roles, db, roles)
	}
	out.Write(b)
	return []byte(out.String())
}

func validateCutoverBackupUnit(env, credentials string) error {
	expected := map[string]string{"PGSERVICEFILE": "pg_service.conf", "PGPASSFILE": "control.pgpass", "NMCP_BACKUP_DUMP_PASSFILE": "dump.pgpass"}
	seen := map[string]bool{}
	for _, token := range strings.Fields(env) {
		pair := strings.SplitN(strings.Trim(token, "\""), "=", 2)
		if len(pair) != 2 {
			return errCutover
		}
		if leaf, ok := expected[pair[0]]; ok {
			if seen[pair[0]] || (pair[1] != "%d/"+leaf && pair[1] != "/run/credentials/nmcp-backup.service/"+leaf) {
				return errCutover
			}
			seen[pair[0]] = true
		} else if strings.HasPrefix(pair[0], "PG") {
			return errCutover
		}
	}
	if len(seen) != len(expected) {
		return errCutover
	}
	want := map[string]bool{"pg_service.conf:/etc/nmcp/backup.pg_service.conf": false, "dump.pgpass:/etc/nmcp/credentials/backup-dump.pgpass": false, "control.pgpass:/etc/nmcp/credentials/backup-control.pgpass": false}
	for _, token := range strings.Fields(credentials) {
		token = strings.Trim(token, "\"")
		if found, ok := want[token]; !ok || found {
			return errCutover
		}
		want[token] = true
	}
	for _, found := range want {
		if !found {
			return errCutover
		}
	}
	return nil
}

func (a *systemCutover) Preflight(ctx context.Context, op RestoreOperation) error {
	// Migration must not be triggered against the old admin.env on restart.
	state, e := systemctl(ctx, "is-active", "nmcp-migrate.service")
	if e != nil || state != "active" {
		return errCutover
	}
	a.oldPIDs = map[string]int{}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		p, e := unitPID(ctx, unit)
		if e != nil || p <= 0 {
			return errCutover
		}
		a.oldPIDs[unit] = p
	}
	for _, item := range []struct{ file, role, suffix string }{{"api.env", "nmcp_api", "api"}, {"worker.env", "nmcp_worker", "worker"}, {"admin.env", "nmcp_migrator", "admin"}} {
		f, e := safeCutoverFile("/etc/nmcp/"+item.file, false)
		if e != nil {
			return e
		}
		if item.suffix != "admin" && cutoverRuntimeStorage(f.before, a.settings) != nil {
			return errCutover
		}
		b, cfg, e := rewriteCutoverEnv(f.before, op.Source.DatabaseName, op.Candidate.DatabaseName, item.role, cutoverApp(op.ID, item.suffix))
		if e != nil {
			return e
		}
		if item.file == "api.env" && !strings.Contains("\n"+string(f.before)+"\n", "\nNMCP_API_ADDR=127.0.0.1:8080\n") {
			return errCutover
		}
		if cfg.Port != a.source.Config().ConnConfig.Port {
			return errCutover
		}
		f.after = b
		a.files = append(a.files, f)
		a.probes = append(a.probes, cutoverProbe{cfg, item.role, cutoverApp(op.ID, item.suffix), ""})
	}
	// backup.env must load exactly the fixed service source; no credentials or
	// arbitrary environment routing may override the rewritten service file.
	f, e := safeCutoverFile("/etc/nmcp/backup.env", false)
	if e != nil {
		return e
	}
	required := map[string]string{"NMCP_DATABASE_URL": "service=nmcp_backup_control", "NMCP_BACKUP_DUMP_SERVICE": "nmcp_backup_dump"}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(f.before), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return errCutover
		}
		if v, ok := required[parts[0]]; ok {
			if seen[parts[0]] || parts[1] != v {
				return errCutover
			}
			seen[parts[0]] = true
		}
		if strings.HasPrefix(strings.TrimSpace(parts[0]), "PG") {
			return errCutover
		}
	}
	if len(seen) != len(required) {
		return errCutover
	}
	env, e := systemctl(ctx, "show", "--property=Environment", "--value", "nmcp-backup.service")
	if e != nil {
		return errCutover
	}
	credentials, e := systemctl(ctx, "show", "--property=LoadCredential", "--value", "nmcp-backup.service")
	if e != nil || validateCutoverBackupUnit(env, credentials) != nil {
		return errCutover
	}
	f, e = safeCutoverFile("/etc/nmcp/backup.pg_service.conf", false)
	if e != nil {
		return e
	}
	var aExtra []cutoverProbe
	f.after, aExtra, e = rewriteCutoverServices(f.before, op, a.source.Config().ConnConfig.Host, a.source.Config().ConnConfig.Port)
	if e != nil {
		return e
	}
	a.files = append(a.files, f)
	a.probes = append(a.probes, aExtra...)
	for _, p := range aExtra {
		f, e := safeCutoverFile(p.pass, false)
		if e != nil {
			return e
		}
		// libpq uses localhost for the default Unix socket passfile lookup.
		host := p.config.Host
		var aliases []string
		if filepath.IsAbs(host) {
			aliases = []string{host}
			host = "localhost"
		}
		f.after, e = rewriteCutoverPass(f.before, op.Source.DatabaseName, op.Candidate.DatabaseName, p.role, host, p.config.Port, aliases...)
		if e != nil {
			return e
		}
		a.files = append(a.files, f)
	}
	var path string
	if a.source.QueryRow(ctx, `SELECT current_setting('hba_file')`).Scan(&path) != nil || !cutoverHBAPath.MatchString(path) {
		return errCutover
	}
	a.hba, e = safeCutoverFile(path, true)
	if e != nil {
		return e
	}
	h := sha256.Sum256(a.hba.before)
	if hex.EncodeToString(h[:]) != op.Receipt.HBASHA256 || verifyGate(ctx, a.source, op.Receipt) != nil {
		return errCutover
	}
	a.hba.after = cutoverHBA(a.hba.before, op)
	return nil
}

func (a *systemCutover) Stop(ctx context.Context) error {
	if _, e := systemctl(ctx, "stop", "nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service"); e != nil {
		return e
	}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service", "nmcp-backup.service"} {
		p, e := unitPID(ctx, unit)
		if e != nil || p != 0 {
			return errCutover
		}
	}
	for _, pid := range a.oldPIDs {
		if _, e := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(e) {
			return errCutover
		}
	}
	var count int
	if a.source.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename IN ('nmcp_api','nmcp_worker','nmcp_backup_dump','nmcp_backup_control')`).Scan(&count) != nil || count != 0 {
		return errCutover
	}
	return nil
}
func (a *systemCutover) Switch(ctx context.Context, op RestoreOperation) error {
	// The preprovisioned candidate deliberately denied routine DB access. A
	// database dump does not recreate database-level CONNECT ACLs. Grant only
	// the intended fixed logins while the old HBA gate still denies them; this
	// is part of the explicit operator cutover, never routine provisioning.
	grants, grantErr := cutoverTargetGrants(op.Candidate.DatabaseName)
	if grantErr != nil {
		return grantErr
	}
	if _, grantErr = a.target.Exec(ctx, grants); grantErr != nil {
		return errCutover
	}
	if replaceCutoverFile(a.hba) != nil {
		return errCutover
	}
	var ok bool
	if a.source.QueryRow(ctx, `SELECT pg_reload_conf()`).Scan(&ok) != nil || !ok {
		return errCutover
	}
	h := sha256.Sum256(a.hba.after)
	receipt := op.Receipt
	receipt.HBASHA256 = hex.EncodeToString(h[:])
	// Reload is asynchronous. Bounded polling observes the server-applied file,
	// not merely a successful pg_reload_conf signal or on-disk hash.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if verifyGate(ctx, a.source, receipt) == nil {
			break
		}
		select {
		case <-ctx.Done():
			return errCutover
		case <-deadline.C:
			return errCutover
		case <-tick.C:
		}
	}
	// Persist the new HBA digest independently of the immutable original receipt.
	d, e := privateDirectory(a.settings.LedgerRoot)
	if e != nil {
		return e
	}
	defer d.Close()
	n, e := unix.Openat(int(d.Fd()), op.ID+".cutover-hba-sha256", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errCutover
	}
	f := os.NewFile(uintptr(n), "hba-digest")
	_, e = f.WriteString(receipt.HBASHA256 + "\n")
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil || d.Sync() != nil {
		return errCutover
	}
	for _, f := range a.files {
		if replaceCutoverFile(f) != nil {
			return errCutover
		}
	}
	return nil
}
func (a *systemCutover) VerifyTargets(ctx context.Context, op RestoreOperation) error {
	for _, p := range a.probes {
		cfg := p.config.Copy()
		cfg.ConnectTimeout = 5 * time.Second
		// ParseConfig reads pgpass at parse time; reparse the now-rewritten file.
		if p.pass != "" {
			var e error
			cfg, e = pgx.ParseConfig(cfg.ConnString())
			if e != nil {
				return errCutover
			}
			cfg.ConnectTimeout = 5 * time.Second
		}
		conn, e := pgx.ConnectConfig(ctx, cfg)
		if e != nil {
			return errCutover
		}
		var oid uint32
		var name, login, current string
		var pid int
		e = conn.QueryRow(ctx, `SELECT (SELECT oid FROM pg_database WHERE datname=current_database()),current_database(),session_user,current_user,pg_backend_pid()`).Scan(&oid, &name, &login, &current, &pid)
		var visible bool
		if e == nil {
			e = a.source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datid=$2 AND usename=$3)`, pid, op.Candidate.DatabaseOID, p.role).Scan(&visible)
		}
		_ = conn.Close(ctx)
		if e != nil || !visible || oid != op.Candidate.DatabaseOID || name != op.Candidate.DatabaseName || login != p.role || current != p.role {
			return errCutover
		}
		if p.role == "nmcp_migrator" {
			continue
		}
		old := cfg.Copy()
		old.Database = op.Source.DatabaseName
		denied, e := pgx.ConnectConfig(ctx, old)
		if e == nil {
			_ = denied.Close(ctx)
			return errCutover
		}
		var rejected *pgconn.PgError
		if !errors.As(e, &rejected) || rejected.Code != "28000" || !strings.Contains(rejected.Message, "pg_hba.conf rejects connection") {
			return errCutover
		}
	}
	return nil
}
func (a *systemCutover) Start(ctx context.Context) error {
	// Timer remains stopped until a separate successful maintenance resume.
	_, e := systemctl(ctx, "start", "nmcp-api.service", "nmcp-worker.service")
	return e
}
func (a *systemCutover) VerifyLive(ctx context.Context, op RestoreOperation) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if a.verifyLiveOnce(ctx, op) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errCutover
		case <-deadline.C:
			return errCutover
		case <-tick.C:
		}
	}
}
func (a *systemCutover) verifyLiveOnce(ctx context.Context, op RestoreOperation) error {
	// API pools are lazy: drive real HTTP queries before proving kernel socket ownership.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/readyz", "/media", "/jobs", "/profiles", "/changes"} {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080"+path, nil)
		if e != nil {
			return errCutover
		}
		req.Header.Set("Accept", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return errCutover
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errCutover
		}
	}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		pid, e := unitPID(ctx, unit)
		if e != nil || pid <= 0 || pid == a.oldPIDs[unit] {
			return errCutover
		}
		role := "nmcp_api"
		suffix := "api"
		if unit == "nmcp-worker.service" {
			role = "nmcp_worker"
			suffix = "worker"
		}
		if e := a.verifyProcessSockets(ctx, pid, role, cutoverApp(op.ID, suffix), op.Candidate.DatabaseOID); e != nil {
			return e
		}
	}
	var count int
	if a.source.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename IN ('nmcp_api','nmcp_worker','nmcp_backup_dump','nmcp_backup_control') AND datid<>$1`, op.Candidate.DatabaseOID).Scan(&count) != nil || count != 0 {
		return errCutover
	}
	for _, p := range []*pgxpool.Pool{a.source, a.target} {
		state, e := New(p).MaintenanceStatus(ctx)
		if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
			return errCutover
		}
	}
	return nil
}

// Correlate every PostgreSQL TCP socket owned by the fresh service process
// with the operator's live backend view. An application_name alone can be
// spoofed by a test probe; kernel socket ownership establishes process causality.
func (a *systemCutover) verifyProcessSockets(ctx context.Context, pid int, role, app string, oid uint32) error {
	entries, e := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if e != nil {
		return errCutover
	}
	inodes := map[string]bool{}
	for _, entry := range entries {
		s, e := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()))
		if e == nil && strings.HasPrefix(s, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(s, "socket:["), "]")] = true
		}
	}
	port := fmt.Sprintf("%04X", a.source.Config().ConnConfig.Port)
	ports := map[int]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		b, e := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if e != nil {
			return errCutover
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || !inodes[f[9]] {
				continue
			}
			remote := strings.Split(f[2], ":")
			local := strings.Split(f[1], ":")
			if len(remote) != 2 || len(local) != 2 || remote[1] != port || f[3] != "01" {
				continue
			}
			p, e := strconv.ParseUint(local[1], 16, 16)
			if e != nil {
				return errCutover
			}
			ports[int(p)] = true
		}
	}
	if len(ports) == 0 {
		return errCutover
	}
	for port := range ports {
		var valid bool
		if a.source.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(datid=$1 AND usename=$2 AND application_name=$3 AND client_addr='127.0.0.1'::inet) FROM pg_stat_activity WHERE client_port=$4`, oid, role, app, port).Scan(&valid) != nil || !valid {
			return errCutover
		}
	}
	return nil
}

func cutoverTargetGrants(database string) (string, error) {
	if !cutoverName.MatchString(database) || len(database) > 63 {
		return "", errCutover
	}
	name := pgx.Identifier{database}.Sanitize()
	return "REVOKE CONNECT,TEMPORARY ON DATABASE " + name + " FROM PUBLIC; GRANT CONNECT ON DATABASE " + name + " TO nmcp_api,nmcp_worker,nmcp_migrator,nmcp_backup_control,nmcp_backup_dump", nil
}

// Runtime processes must use the same FS namespace that Restore actually
// checked, not an unrelated valid copy selected only by the operator probe.
func cutoverRuntimeStorage(raw []byte, c RestoreSettings) error {
	expected := map[string]string{"NMCP_STORAGE_ROOT": c.StorageRoot, "NMCP_FILE_BASE_URL": c.FileBaseURL}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		want, required := expected[name]
		if !required {
			continue
		}
		if name == "NMCP_FILE_BASE_URL" {
			normalized, err := config.NormalizeFileBaseURL(value)
			if err != nil {
				return errCutover
			}
			expectedURL, err := config.NormalizeFileBaseURL(want)
			if err != nil {
				return errCutover
			}
			value, want = normalized, expectedURL
		} else if !filepath.IsAbs(value) {
			return errCutover
		} else {
			value = filepath.Clean(value)
		}
		if !ok || seen[name] || value != want {
			return errCutover
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return errCutover
	}
	return nil
}

func validPriorCutoverApp(value, suffix string) bool {
	const prefix = "nmcp_restore_"
	tail := "_" + suffix
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, tail) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(value, prefix), tail)
	if len(id) != 32 {
		return false
	}
	expanded := id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	return uuidPattern.MatchString(expanded)
}
