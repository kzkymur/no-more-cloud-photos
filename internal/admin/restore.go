package admin

// Restore prepares, but never promotes, an operator-provisioned database. The
// external journal is the authority: dump contents cannot authorize a restore.
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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgpassfile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/httpapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"golang.org/x/sys/unix"
)

var errRestore = errors.New("restore preparation failed; source remains fenced after maintenance entry")

type RestoreSettings struct {
	RestoreTool                                string
	Root, LedgerRoot, ReceiptFile              string
	Service, APIService, ServiceFile, PassFile string
	StorageRoot, FileBaseURL                   string
}

func restoreSettings() (RestoreSettings, error) {
	c := RestoreSettings{Root: os.Getenv("NMCP_BACKUP_ROOT"), LedgerRoot: os.Getenv("NMCP_RESTORE_LEDGER_ROOT"), ReceiptFile: os.Getenv("NMCP_RESTORE_RECEIPT"), Service: os.Getenv("NMCP_RESTORE_SERVICE"), APIService: os.Getenv("NMCP_RESTORE_API_SERVICE"), ServiceFile: os.Getenv("PGSERVICEFILE"), PassFile: os.Getenv("NMCP_RESTORE_PASSFILE"), StorageRoot: os.Getenv("NMCP_STORAGE_ROOT"), FileBaseURL: os.Getenv("NMCP_FILE_BASE_URL")}
	c.RestoreTool = os.Getenv("NMCP_BACKUP_PG_RESTORE")
	if c.RestoreTool == "" {
		c.RestoreTool = "/usr/bin/pg_restore"
	}
	return c, c.validate()
}
func (c RestoreSettings) validate() error {
	if _, err := config.NormalizeFileBaseURL(c.FileBaseURL); err != nil {
		return ErrValidation
	}
	if c.RestoreTool != "" && (!filepath.IsAbs(c.RestoreTool) || filepath.Clean(c.RestoreTool) != c.RestoreTool) {
		return ErrValidation
	}
	for _, p := range []string{c.Root, c.LedgerRoot, c.ReceiptFile, c.ServiceFile, c.PassFile, c.StorageRoot} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return ErrValidation
		}
	}
	if !serviceName.MatchString(c.Service) || !serviceName.MatchString(c.APIService) || c.Service == c.APIService {
		return ErrValidation
	}
	for _, root := range []string{c.Root, c.StorageRoot} {
		if pathWithin(c.LedgerRoot, root) || pathWithin(root, c.LedgerRoot) {
			return ErrValidation
		}
	}
	return nil
}
func pathWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

type RestoreIdentity struct {
	ClusterID    string `json:"cluster_id"`
	DatabaseOID  uint32 `json:"database_oid"`
	DatabaseName string `json:"database_name"`
	Owner        string `json:"owner"`
}

// Receipt is a private operator attestation, not a database-supplied claim.
// The operator must verify template0 provenance and HBA rule ordering, including
// every routine UID/TCP route, before signing this receipt with filesystem ownership.
type RestoreReceipt struct {
	Source             RestoreIdentity `json:"source"`
	Candidate          RestoreIdentity `json:"candidate"`
	Template0          bool            `json:"template0"`
	Empty              bool            `json:"empty"`
	RoutineLoginDenied bool            `json:"routine_login_denied"`
	OperatorSocketOnly bool            `json:"operator_socket_only"`
	HBASHA256          string          `json:"hba_sha256"`
	IssuedAt           time.Time       `json:"issued_at"`
}
type RestoreOperation struct {
	FailedPhase     string          `json:"failed_phase,omitempty"`
	RoleGraphSHA256 string          `json:"role_graph_sha256"`
	ID              string          `json:"id"`
	BackupID        string          `json:"backup_id"`
	DumpSHA256      string          `json:"dump_sha256"`
	Source          RestoreIdentity `json:"source"`
	Candidate       RestoreIdentity `json:"candidate"`
	Receipt         RestoreReceipt  `json:"receipt"`
	Phase           string          `json:"phase"`
	StartedAt       time.Time       `json:"started_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	SnapshotCutoff  time.Time       `json:"snapshot_cutoff"`
	RPO             time.Duration   `json:"rpo_ns"`
	RTO             time.Duration   `json:"rto_ns"`
}

func validIdentity(i RestoreIdentity) bool {
	return i.ClusterID != "" && i.DatabaseOID != 0 && i.DatabaseName != "" && i.Owner != ""
}
func validReceipt(r RestoreReceipt, now time.Time) bool {
	b, e := hex.DecodeString(r.HBASHA256)
	return validIdentity(r.Source) && validIdentity(r.Candidate) && (r.Source.ClusterID != r.Candidate.ClusterID || r.Source.DatabaseOID != r.Candidate.DatabaseOID) && r.Template0 && r.Empty && r.RoutineLoginDenied && r.OperatorSocketOnly && e == nil && len(b) == 32 && !r.IssuedAt.IsZero() && !r.IssuedAt.After(now) && now.Sub(r.IssuedAt) <= time.Hour
}

// privateDirectory walks every component without following symlinks and pins it.
func privateDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrValidation
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, errRestore
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, errRestore
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		f.Close()
		return nil, errRestore
	}
	return f, nil
}
func privateRead(dir *os.File, name string) ([]byte, error) {
	fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, errRestore
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size > 1<<20 {
		return nil, errRestore
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
func decodePrivate(path string, v any) error {
	d, e := privateDirectory(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	b, e := privateRead(d, filepath.Base(path))
	if e != nil {
		return e
	}
	return decodeRestore(b, v)
}
func decodeRestore(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return errRestore
	}
	return nil
}

// Journal snapshots are append-only O_EXCL files. Earlier evidence survives a
// failed write or fsync. A phase cannot be silently overwritten/re-executed.
func SaveRestoreOperation(root string, op RestoreOperation) error {
	if !uuidPattern.MatchString(op.ID) || !uuidPattern.MatchString(op.BackupID) || !serviceName.MatchString(op.Phase) || !validIdentity(op.Source) || !validIdentity(op.Candidate) {
		return ErrValidation
	}
	d, e := privateDirectory(root)
	if e != nil {
		return e
	}
	defer d.Close()
	b, e := json.Marshal(op)
	if e != nil {
		return errRestore
	}
	name := op.ID + "." + op.Phase + ".json"
	fd, e := unix.Openat(int(d.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errRestore
	}
	f := os.NewFile(uintptr(fd), name)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil || d.Sync() != nil {
		return errRestore
	}
	return nil
}
func LoadRestoreOperation(root, id string) (RestoreOperation, error) {
	var op RestoreOperation
	if !uuidPattern.MatchString(id) {
		return op, ErrValidation
	}
	d, e := privateDirectory(root)
	if e != nil {
		return op, e
	}
	defer d.Close()
	names, e := d.Readdirnames(-1)
	if e != nil {
		return op, errRestore
	}
	for _, n := range names {
		if !strings.HasPrefix(n, id+".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		b, e := privateRead(d, n)
		if e != nil {
			return op, e
		}
		var next RestoreOperation
		if decodeRestore(b, &next) != nil || next.ID != id {
			return op, errRestore
		}
		if op.ID != "" && (next.BackupID != op.BackupID || next.DumpSHA256 != op.DumpSHA256 || next.Source != op.Source || next.Candidate != op.Candidate || next.Receipt != op.Receipt || !next.StartedAt.Equal(op.StartedAt) || !next.SnapshotCutoff.Equal(op.SnapshotCutoff)) {
			return RestoreOperation{}, errRestore
		}
		if next.Phase == "" || !serviceName.MatchString(next.Phase) || !validIdentity(next.Source) || !validIdentity(next.Candidate) || next.UpdatedAt.IsZero() {
			return RestoreOperation{}, errRestore
		}
		if op.ID == "" || next.UpdatedAt.After(op.UpdatedAt) {
			op = next
		}
	}
	if op.ID == "" {
		return op, errRestore
	}
	return op, nil
}
func reserveCandidate(root string, i RestoreIdentity, id string) error {
	d, e := privateDirectory(root)
	if e != nil {
		return e
	}
	defer d.Close()
	b, _ := json.Marshal(i)
	hash := sha256.Sum256(b)
	name := fmt.Sprintf("candidate-%x", hash)
	fd, e := unix.Openat(int(d.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errRestore
	}
	f := os.NewFile(uintptr(fd), name)
	_, e = f.WriteString(id)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil || d.Sync() != nil {
		return errRestore
	}
	return nil
}
func restoreIdentity(ctx context.Context, p *pgxpool.Pool) (RestoreIdentity, error) {
	var i RestoreIdentity
	e := p.QueryRow(ctx, `SELECT (pg_control_system()).system_identifier::text,d.oid,d.datname,pg_get_userbyid(d.datdba) FROM pg_database d WHERE d.datname=current_database()`).Scan(&i.ClusterID, &i.DatabaseOID, &i.DatabaseName, &i.Owner)
	return i, e
}

// restoreConnections is a single immutable resolution used by every probe and
// the tool. Neither libpq nor pgx gets to fill missing fields from ambient state.
type restoreConnections struct {
	operator, api *pgxpool.Config
	service, pass *os.File
}

func (r *restoreConnections) Close() { r.service.Close(); r.pass.Close() }
func restoreSnapshot(name string, data []byte) (*os.File, error) {
	fd, e := unix.MemfdCreate(name, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if e != nil {
		return nil, errRestore
	}
	f := os.NewFile(uintptr(fd), name)
	if unix.Fchmod(fd, 0600) != nil {
		f.Close()
		return nil, errRestore
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return nil, errRestore
	}
	if _, e = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); e != nil {
		f.Close()
		return nil, errRestore
	}
	return f, nil
}
func resolveRestoreConnections(c RestoreSettings) (*restoreConnections, error) {
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		if strings.HasPrefix(key, "PG") && key != "PGSERVICEFILE" && key != "PGPASSFILE" {
			return nil, errRestore
		}
	}
	// Pin the parent directory without following any symlink. Input files
	// themselves must be private/current-UID, but the directory may be traversable
	// by the server UID (the distinct-UID peer-auth operator fixture needs this).
	read := func(path string) ([]byte, error) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errRestore
		}
		fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, errRestore
		}
		for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
			if part == "" {
				continue
			}
			next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			unix.Close(fd)
			if e != nil {
				return nil, errRestore
			}
			fd = next
		}
		d := os.NewFile(uintptr(fd), filepath.Dir(path))
		defer d.Close()
		return privateRead(d, filepath.Base(path))
	}
	data, e := read(c.ServiceFile)
	if e != nil {
		return nil, errRestore
	}
	passwords, e := read(c.PassFile)
	if e != nil {
		return nil, errRestore
	}
	pass, e := pgpassfile.ParsePassfile(bytes.NewReader(passwords))
	if e != nil {
		return nil, errRestore
	}
	sections := map[string]map[string]string{}
	var fields map[string]string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := line[1 : len(line)-1]
			if !serviceName.MatchString(name) || strings.EqualFold(name, "default") || sections[name] != nil {
				return nil, errRestore
			}
			fields = map[string]string{}
			sections[name] = fields
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || fields == nil || value == "" || fields[key] != "" {
			return nil, errRestore
		}
		// A closed vocabulary excludes options, role, password, includes, TLS and
		// service/default indirection. Even unused sections must be unambiguous.
		switch key {
		case "host", "port", "dbname", "user", "sslmode":
		default:
			return nil, errRestore
		}
		fields[key] = value
	}
	var service, pinnedPass strings.Builder
	escapePass := func(v string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(v) }
	parse := func(name string) (*pgxpool.Config, error) {
		f := sections[name]
		if len(f) != 5 {
			return nil, errRestore
		}
		port, e := strconv.ParseUint(f["port"], 10, 16)
		if e != nil || port == 0 || !filepath.IsAbs(f["host"]) || filepath.Clean(f["host"]) != f["host"] || strings.ContainsAny(f["host"], ",\n\r") || f["sslmode"] != "disable" {
			return nil, errRestore
		}
		f["port"] = strconv.FormatUint(port, 10)
		var dsn strings.Builder
		fmt.Fprintf(&service, "[%s]\n", name)
		for _, key := range []string{"host", "port", "dbname", "user", "sslmode"} {
			v := f[key]
			if v == "" || strings.ContainsAny(v, "\n\r\x00") {
				return nil, errRestore
			}
			fmt.Fprintf(&service, "%s=%s\n", key, v)
			fmt.Fprintf(&dsn, "%s=%s ", key, quoteConnValue(v))
		}
		// Empty password plus explicit null passfile suppress pgx default credentials.
		cfg, e := pgxpool.ParseConfig(dsn.String() + "password='' passfile='/dev/null'")
		if e != nil {
			return nil, errRestore
		}
		cfg.ConnConfig.Password = pass.FindPassword(f["host"], f["port"], f["dbname"], f["user"])
		if cfg.ConnConfig.Password == "" {
			cfg.ConnConfig.Password = pass.FindPassword("localhost", f["port"], f["dbname"], f["user"])
		}
		if strings.ContainsAny(cfg.ConnConfig.Password, "\r\n\x00") || len(cfg.ConnConfig.Fallbacks) != 0 || len(cfg.ConnConfig.RuntimeParams) != 0 {
			return nil, errRestore
		}
		// An empty private passfile deliberately supports peer authentication.
		// It cannot fall back to environment or the operator home directory.
		// Canonical exact entries ensure libpq socket/default-directory password
		// matching cannot choose a different credential than the pgx probe.
		for _, host := range []string{f["host"], "localhost"} {
			fmt.Fprintf(&pinnedPass, "%s:%s:%s:%s:%s\n", escapePass(host), f["port"], escapePass(f["dbname"]), escapePass(f["user"]), escapePass(cfg.ConnConfig.Password))
		}
		cfg.MaxConns = 3
		return cfg, nil
	}
	operator, e := parse(c.Service)
	if e != nil {
		return nil, e
	}
	api, e := parse(c.APIService)
	if e != nil {
		return nil, e
	}
	a, b := operator.ConnConfig, api.ConnConfig
	if a.Host != b.Host || a.Port != b.Port || a.Database != b.Database || b.User != "nmcp_api" {
		return nil, errRestore
	}
	r := &restoreConnections{operator: operator, api: api}
	r.service, e = restoreSnapshot("restore-service", []byte(service.String()))
	if e != nil {
		return nil, e
	}
	r.pass, e = restoreSnapshot("restore-pass", []byte(pinnedPass.String()))
	if e != nil {
		r.service.Close()
		return nil, e
	}
	return r, nil
}
func (r *restoreConnections) command(ctx context.Context, c RestoreSettings) *exec.Cmd {
	tool := c.RestoreTool
	if tool == "" {
		tool = "/usr/bin/pg_restore"
	}
	cmd := exec.CommandContext(ctx, tool, "--no-password", "--single-transaction", "--exit-on-error", "--dbname=service="+c.Service)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "PGSERVICE=" + c.Service, "PGSERVICEFILE=/proc/self/fd/3", "PGPASSFILE=/proc/self/fd/4"}
	cmd.ExtraFiles = []*os.File{r.service, r.pass}
	return cmd
}
func restorePool(ctx context.Context, c RestoreSettings, service string) (*pgxpool.Pool, error) {
	r, e := resolveRestoreConnections(c)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	cfg := r.operator
	if service == c.APIService {
		cfg = r.api
	} else if service != c.Service {
		return nil, errRestore
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}
func restoreRoleGraph(ctx context.Context, p *pgxpool.Pool) (string, error) {
	var roles, members []byte
	if p.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit,rolconfig) ORDER BY rolname),'[]'::jsonb) FROM pg_roles WHERE starts_with(rolname,'nmcp_')`).Scan(&roles) != nil {
		return "", errRestore
	}
	if p.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(r.rolname,m.rolname,g.rolname,a.admin_option,a.inherit_option,a.set_option) ORDER BY r.rolname,m.rolname,g.rolname),'[]'::jsonb) FROM pg_auth_members a JOIN pg_roles r ON r.oid=a.roleid JOIN pg_roles m ON m.oid=a.member JOIN pg_roles g ON g.oid=a.grantor WHERE starts_with(r.rolname,'nmcp_') OR starts_with(m.rolname,'nmcp_')`).Scan(&members) != nil {
		return "", errRestore
	}
	h := sha256.New()
	h.Write(roles)
	h.Write([]byte{'\n'})
	h.Write(members)
	return hex.EncodeToString(h.Sum(nil)), nil
}
func verifyGate(ctx context.Context, p *pgxpool.Pool, r RestoreReceipt) error {
	// Includes are deliberately unsupported: a single pinned HBA receipt cannot
	// authorize mutable include trees. Require a reload newer than the file and
	// a fully parseable main file, rather than hashing an unapplied edit.
	var loaded bool
	if p.QueryRow(ctx, `SELECT pg_conf_load_time()>=(pg_stat_file(current_setting('hba_file'))).modification AND NOT EXISTS(SELECT 1 FROM pg_hba_file_rules WHERE error IS NOT NULL OR file_name<>current_setting('hba_file'))`).Scan(&loaded) != nil || !loaded {
		return errRestore
	}
	var hba string
	if p.QueryRow(ctx, `SELECT pg_read_file(current_setting('hba_file'))`).Scan(&hba) != nil {
		return errRestore
	}
	hash := sha256.Sum256([]byte(hba))
	if hex.EncodeToString(hash[:]) != r.HBASHA256 {
		return errRestore
	}
	return nil
}

// RestoreMaintenanceEnter uses the existing audited maintenance mutation path.
// An existing maintenance owner is never stolen by an unrelated restore.
func (s *Service) RestoreMaintenanceEnter(ctx context.Context, id string) error {
	if !uuidPattern.MatchString(id) {
		return ErrValidation
	}
	return s.mutate(ctx, "restore_enter", "maintenance-state", map[string]string{"operation_id": id}, []string{id}, func(tx pgx.Tx) error {
		var mode string
		var owner *string
		if e := tx.QueryRow(ctx, `SELECT mode,owner FROM maintenance_state WHERE id=1 FOR UPDATE NOWAIT`).Scan(&mode, &owner); e != nil {
			return e
		}
		if mode == "maintenance" {
			if owner != nil && *owner == id {
				return nil
			}
			return ErrConflict
		}
		_, e := tx.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='operator restore',owner=$1,entered_at=clock_timestamp(),successful_check_report=NULL,updated_at=clock_timestamp() WHERE id=1`, id)
		return e
	})
}

// Restore has no cleanup/drop path. Failed or interrupted candidates require a
// new operator-provisioned DB and receipt; neither arbitrary reuse nor retry of
// a possibly committed pg_restore is authorized.
func (s *Service) Restore(ctx context.Context, backupID string, c RestoreSettings) (op RestoreOperation, retErr error) {
	started := time.Now()
	if c.validate() != nil || !uuidPattern.MatchString(backupID) || s.Pool == nil {
		return op, ErrValidation
	}
	defer func() {
		if retErr != nil {
			if op.ID != "" && op.Phase != "failed" {
				op.FailedPhase = op.Phase
				op.Phase = "failed"
				op.UpdatedAt = time.Now()
				op.RTO = time.Since(started)
				_ = SaveRestoreOperation(c.LedgerRoot, op)
			}
			retErr = errRestore
		}
	}()
	connections, e := resolveRestoreConnections(c)
	if e != nil {
		return op, e
	}
	defer connections.Close()
	var receipt RestoreReceipt
	if decodePrivate(c.ReceiptFile, &receipt) != nil || !validReceipt(receipt, time.Now()) {
		return op, errRestore
	}
	artifactOptions := backupartifact.Options{ReadOnly: true, Lock: true}
	var artifactStat unix.Stat_t
	if unix.Lstat(c.Root, &artifactStat) != nil {
		return op, errRestore
	}
	if os.Geteuid() == 0 && artifactStat.Uid != 0 {
		u, err := user.Lookup("nmcp-backup")
		if err == nil {
			uid, err := strconv.ParseUint(u.Uid, 10, 32)
			if err != nil {
				return op, errRestore
			}
			owner := uint32(uid)
			artifactOptions.OwnerUID = &owner
		} else {
			return op, errRestore
		}
	}
	store, e := backupartifact.Open(c.Root, artifactOptions)
	if e != nil {
		return op, e
	}
	defer store.Close()
	manifest, e := store.Verify(backupID)
	if e != nil || manifest.ID != backupID || manifest.SnapshotCutoff.After(started) {
		return op, errRestore
	}
	unlock, e := s.sessionLock(ctx, "maintenance")
	if e != nil {
		return op, e
	}
	defer unlock()
	source, e := restoreIdentity(ctx, s.Pool)
	if e != nil || source != receipt.Source {
		return op, errRestore
	}
	// Bind the private artifact to a committed success in the pinned SOURCE,
	// not just a same-named file copied from another database/cluster.
	var historyHash string
	var historySize int64
	var historyManifest []byte
	if e = s.Pool.QueryRow(ctx, `SELECT sha256,size_bytes,manifest FROM backup_runs WHERE id=$1 AND status='succeeded' AND retention_deleted_at IS NULL`, backupID).Scan(&historyHash, &historySize, &historyManifest); e != nil {
		return op, errRestore
	}
	manifestJSON, marshalErr := json.Marshal(manifest)
	if marshalErr != nil || historyHash != manifest.DumpSHA256 || historySize != manifest.DumpSize || !backupJSONEqual(manifestJSON, historyManifest) {
		return op, errRestore
	}
	candidate, e := pgxpool.NewWithConfig(ctx, connections.operator)
	if e != nil {
		return op, e
	}
	defer candidate.Close()
	identity, e := restoreIdentity(ctx, candidate)
	if e != nil || identity != receipt.Candidate || verifyGate(ctx, candidate, receipt) != nil {
		return op, errRestore
	}
	var empty bool
	if candidate.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) acl WHERE d.datname=current_database() AND acl.grantee=0 AND acl.privilege_type='CONNECT') AND NOT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname NOT IN ('public','pg_catalog','information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp%') AND NOT EXISTS(SELECT 1 FROM pg_extension WHERE extname<>'plpgsql') AND NOT EXISTS(SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public') AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%') AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema')) AND NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND pid<>pg_backend_pid())`).Scan(&empty) != nil || !empty {
		return op, errRestore
	}
	roleGraph, e := restoreRoleGraph(ctx, s.Pool)
	if e != nil {
		return op, e
	}
	candidateGraph, e := restoreRoleGraph(ctx, candidate)
	if e != nil || candidateGraph != roleGraph {
		return op, errRestore
	}
	op = RestoreOperation{RoleGraphSHA256: roleGraph, ID: newID(), BackupID: backupID, DumpSHA256: manifest.DumpSHA256, Source: source, Candidate: identity, Receipt: receipt, Phase: "prepared", StartedAt: started, UpdatedAt: time.Now(), SnapshotCutoff: manifest.SnapshotCutoff, RPO: started.Sub(manifest.SnapshotCutoff)}
	if reserveCandidate(c.LedgerRoot, identity, op.ID) != nil || SaveRestoreOperation(c.LedgerRoot, op) != nil {
		return op, errRestore
	}
	phase := func(name string) error {
		op.Phase = name
		op.UpdatedAt = time.Now()
		op.RTO = time.Since(started)
		return SaveRestoreOperation(c.LedgerRoot, op)
	}
	if s.RestoreMaintenanceEnter(ctx, op.ID) != nil {
		return op, errRestore
	}
	if phase("maintenance") != nil {
		return op, errRestore
	}
	e = store.WithDump(backupID, func(f *os.File) error {
		// Recheck the exact ledger digest on the descriptor passed to the tool.
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != op.DumpSHA256 {
			return errRestore
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errRestore
		}
		cmd := connections.command(ctx, c)
		cmd.Stdin = f
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 5 * time.Second
		if cmd.Run() != nil {
			return errRestore
		}
		return nil
	})
	if e != nil {
		return op, e
	}
	// Override imported normal state and abandoned own-running backup history in
	// one transaction. No synthesized succeeded row for the restored backup.
	tx, e := candidate.Begin(ctx)
	if e != nil {
		return op, e
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='operator restore',owner=$1,entered_at=clock_timestamp(),successful_check_report=NULL,updated_at=clock_timestamp() WHERE id=1`, op.ID); e != nil {
		return op, e
	}
	if _, e = tx.Exec(ctx, `UPDATE backup_runs SET status='failed',finished_at=clock_timestamp(),error_code='restored_incomplete',error_message='running backup imported from snapshot',notification_status='pending',notification_error=NULL WHERE status='running'`); e != nil {
		return op, e
	}
	if tx.Commit(ctx) != nil || phase("restored") != nil {
		return op, errRestore
	}
	if actual, e := restoreIdentity(ctx, candidate); e != nil || actual != identity || verifyGate(ctx, candidate, receipt) != nil {
		return op, errRestore
	}
	if graph, e := restoreRoleGraph(ctx, candidate); e != nil || graph != op.RoleGraphSHA256 {
		return op, errRestore
	}
	migrator, e := database.NewMigrator(candidate)
	if e != nil {
		return op, e
	}
	status, e := migrator.Status(ctx)
	if e != nil || !status.Ready() {
		return op, errRestore
	}
	target := New(candidate)
	config, e := target.ConfigShow(ctx)
	if e != nil {
		return op, e
	}
	encoded, _ := json.Marshal(config)
	var expected Configuration
	if json.Unmarshal(manifest.Config, &expected) != nil {
		return op, errRestore
	}
	expectedJSON, _ := json.Marshal(expected)
	if !bytes.Equal(encoded, expectedJSON) {
		return op, errRestore
	}
	for name, value := range map[string]string{"default_timezone": config.DefaultTimezone, "db_backup_interval_hours": fmt.Sprint(config.DBBackupIntervalHours), "db_backup_retention_days": fmt.Sprint(config.DBBackupRetentionDays)} {
		if _, e := ConfigValue(name, value); e != nil {
			return op, e
		}
	}
	files, e := storage.Open(c.StorageRoot, storage.Options{})
	if e != nil {
		return op, e
	}
	defer files.Close()
	if _, e = target.Check(ctx, files, reconciliation.ScopeAll); e != nil {
		return op, e
	}
	if e = restoreAPIProofResolved(ctx, c, connections, candidate, identity); e != nil {
		return op, e
	}
	if verifyGate(ctx, candidate, receipt) != nil {
		return op, errRestore
	}
	state, e := s.MaintenanceStatus(ctx)
	if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
		return op, errRestore
	}
	if phase("validated") != nil {
		return op, errRestore
	}
	return op, nil
}
func restoreAPIProof(ctx context.Context, c RestoreSettings, operator *pgxpool.Pool, want RestoreIdentity) error {
	connections, e := resolveRestoreConnections(c)
	if e != nil {
		return e
	}
	defer connections.Close()
	return restoreAPIProofResolved(ctx, c, connections, operator, want)
}
func restoreAPIProofResolved(ctx context.Context, c RestoreSettings, connections *restoreConnections, operator *pgxpool.Pool, want RestoreIdentity) error {
	api, e := pgxpool.NewWithConfig(ctx, connections.api)
	if e != nil {
		return e
	}
	defer api.Close()
	// No SET ROLE, inherited superuser, or privileged proxy proof: the actual
	// login must be nmcp_api and must not have bypass privileges.
	var session, current, name string
	var oid uint32
	var safe bool
	if api.QueryRow(ctx, `SELECT session_user,current_user,current_database(),(SELECT oid FROM pg_database WHERE datname=current_database()),NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname=session_user`).Scan(&session, &current, &name, &oid, &safe) != nil || session != "nmcp_api" || current != "nmcp_api" || !safe || name != want.DatabaseName || oid != want.DatabaseOID {
		return errRestore
	}
	// API has no pg_control_system grant. Its socket host/port must be exactly
	// the verified operator endpoint, then DB name/OID prove the same candidate.
	a, b := api.Config().ConnConfig, operator.Config().ConnConfig
	if a.Host != b.Host || a.Port != b.Port {
		return errRestore
	}
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		return errRestore
	}
	codec, e := readapi.NewCursorCodec(key)
	if e != nil {
		return e
	}
	reads, e := readapi.NewService(api, codec, c.FileBaseURL)
	if e != nil {
		return e
	}
	handler := httpapi.NewHandler(httpapi.Dependencies{Reads: reads})
	for _, path := range []string{"/media", "/jobs", "/profiles", "/changes"} {
		r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		r.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			return errRestore
		}
	}
	var id string
	e = operator.QueryRow(ctx, `SELECT id::text FROM media WHERE deleted_at IS NULL ORDER BY id LIMIT 1`).Scan(&id)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return errRestore
	}
	if e == nil {
		r := httptest.NewRequest(http.MethodGet, "/media/"+id, nil).WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			return errRestore
		}
	}
	return nil
}
