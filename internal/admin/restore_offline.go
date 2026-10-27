package admin

// Offline preparation is an explicit root/operator action. This external
// authority is NOT a dump-supplied history row. The operator must independently
// identify the stopped source, attest the complete local endpoints and role
// graph, and copy the full previously committed successful manifest. No online
// source query, normal imported state, or imported running row grants authority.
//
// Required JSON schema is OfflineRestoreAuthority (unknown fields rejected).
// All fields are mandatory, including endpoint_complete=true and the literal
// confirmation. Input and its parent must be root-private (0600/0700); ancestors
// must not be symlinks. Candidate HBA/template0/empty provenance is separately
// bound by RestoreReceipt. PostgreSQL 17 units and /etc HBA paths must agree.
// Prepare stops fixed application units and the exact source unit, persists an
// HBA reject fence, restores and validates, and leaves everything in maintenance.
// It NEVER implements promotion, restart, resume, drop, or removal of that fence.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"golang.org/x/sys/unix"
)

const offlineConfirmation = "I authorize offline source fencing and candidate preparation; no cutover or resume"

var offlinePGUnit = regexp.MustCompile(`^postgresql@17-([A-Za-z0-9_][A-Za-z0-9_-]*)\.service$`)

type OfflineRestoreEndpoint struct {
	SocketDirectory string `json:"socket_directory"`
	Port            uint16 `json:"port"`
	// Every configured TCP listener must be enumerated; remote/wildcard listeners
	// are unsupported. An empty list explicitly attests Unix-socket-only source.
	LoopbackHosts []string `json:"loopback_hosts"`
}

type OfflineRestoreAuthority struct {
	// Independently attested committed source success, never imported history.
	SourceBackupSucceeded bool                    `json:"source_backup_succeeded"`
	Source                RestoreIdentity         `json:"source"`
	Candidate             RestoreIdentity         `json:"candidate"`
	Manifest              backupartifact.Manifest `json:"manifest"`
	RoleGraphSHA256       string                  `json:"role_graph_sha256"`
	SourceHBAPath         string                  `json:"source_hba_path"`
	SourceHBASHA256       string                  `json:"source_hba_sha256"`
	SourceUnit            string                  `json:"source_unit"`
	CandidateUnit         string                  `json:"candidate_unit"`
	SourceEndpoint        OfflineRestoreEndpoint  `json:"source_endpoint"`
	CandidateEndpoint     OfflineRestoreEndpoint  `json:"candidate_endpoint"`
	EndpointComplete      bool                    `json:"endpoint_complete"`
	IssuedAt              time.Time               `json:"issued_at"`
	Confirmation          string                  `json:"confirmation"`
}

func offlineDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func offlineEndpointValid(e OfflineRestoreEndpoint, candidate bool) bool {
	if !filepath.IsAbs(e.SocketDirectory) || filepath.Clean(e.SocketDirectory) != e.SocketDirectory || strings.ContainsAny(e.SocketDirectory, "\x00\n\r,") || e.Port == 0 || e.LoopbackHosts == nil {
		return false
	}
	seen := map[string]bool{}
	for _, h := range e.LoopbackHosts {
		if (h != "127.0.0.1" && h != "::1") || seen[h] || candidate {
			return false
		}
		seen[h] = true
	}
	return true
}
func offlineIdentityValid(i RestoreIdentity) bool {
	n, e := strconv.ParseUint(i.ClusterID, 10, 64)
	return e == nil && n != 0 && strconv.FormatUint(n, 10) == i.ClusterID && validIdentity(i) && cutoverName.MatchString(i.DatabaseName) && cutoverName.MatchString(i.Owner)
}
func validateOfflineAuthority(a OfflineRestoreAuthority, r RestoreReceipt, id string, now time.Time) error {
	sourceUnit, candidateUnit := offlinePGUnit.FindStringSubmatch(a.SourceUnit), offlinePGUnit.FindStringSubmatch(a.CandidateUnit)
	if !a.SourceBackupSucceeded || !validReceipt(r, now) || !offlineIdentityValid(a.Source) || !offlineIdentityValid(a.Candidate) || a.Source != r.Source || a.Candidate != r.Candidate || a.Source.ClusterID == a.Candidate.ClusterID || !cutoverName.MatchString(a.Source.DatabaseName) || !cutoverName.MatchString(a.Candidate.DatabaseName) || !cutoverName.MatchString(a.Source.Owner) || !cutoverName.MatchString(a.Candidate.Owner) || len(sourceUnit) != 2 || len(candidateUnit) != 2 || a.SourceUnit == a.CandidateUnit || a.SourceHBAPath != "/etc/postgresql/17/"+sourceUnit[1]+"/pg_hba.conf" || !offlineDigest(a.SourceHBASHA256) || !offlineDigest(a.RoleGraphSHA256) || !offlineEndpointValid(a.SourceEndpoint, false) || !offlineEndpointValid(a.CandidateEndpoint, true) || (a.SourceEndpoint.SocketDirectory == a.CandidateEndpoint.SocketDirectory && a.SourceEndpoint.Port == a.CandidateEndpoint.Port) || !a.EndpointComplete || a.Confirmation != offlineConfirmation || a.IssuedAt.IsZero() || a.IssuedAt.After(now) || now.Sub(a.IssuedAt) > time.Hour || a.Manifest.ID != id || !uuidPattern.MatchString(id) || !offlineDigest(a.Manifest.DumpSHA256) || a.Manifest.DumpSize <= 0 || a.Manifest.SnapshotCutoff.IsZero() || a.Manifest.SnapshotCutoff.After(a.IssuedAt) {
		return errRestore
	}
	return nil
}

// offlineSourceActions is the causal root-test seam. Production never accepts
// replacement executables, arbitrary commands, units, paths, or a UID bypass.
type offlineSourceActions interface {
	Fence(context.Context, RestoreOperation) error
	VerifyStopped(context.Context, RestoreOperation) error
}
type offlineSystemSource struct{ hba cutoverFile }

func (a *offlineSystemSource) Fence(ctx context.Context, op RestoreOperation) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	// Install durable reject rules BEFORE stopping: even a failed/partial stop or
	// subsequent operator restart cannot expose the old source to routine roles.
	if replaceCutoverFile(a.hba) != nil {
		return errRestore
	}
	if _, e := systemctl(ctx, "stop", "nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service", op.OfflineAuthority.SourceUnit); e != nil {
		return errRestore
	}
	return a.VerifyStopped(ctx, op)
}
func offlineInactive(ctx context.Context, unit string) error {
	state, e := systemctl(ctx, "show", "--property=ActiveState", "--value", unit)
	if e != nil || state != "inactive" {
		return errRestore
	}
	pid, e := unitPID(ctx, unit)
	if e != nil || pid != 0 {
		return errRestore
	}
	return nil
}
func (a *offlineSystemSource) VerifyStopped(ctx context.Context, op RestoreOperation) error {
	for _, u := range []string{"nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service", op.OfflineAuthority.SourceUnit} {
		if offlineInactive(ctx, u) != nil {
			return errRestore
		}
	}
	return a.verifySourceStopped(ctx, op)
}
func (a *offlineSystemSource) verifySourceStopped(ctx context.Context, op RestoreOperation) error {
	if offlineInactive(ctx, op.OfflineAuthority.SourceUnit) != nil {
		return errRestore
	}
	current, e := safeCutoverFile(op.OfflineAuthority.SourceHBAPath, true)
	if e != nil || !bytes.Equal(current.before, a.hba.after) {
		return errRestore
	}
	ep := op.OfflineAuthority.SourceEndpoint
	endpoints := []struct{ network, address string }{{"unix", filepath.Join(ep.SocketDirectory, fmt.Sprintf(".s.PGSQL.%d", ep.Port))}}
	for _, h := range ep.LoopbackHosts {
		endpoints = append(endpoints, struct{ network, address string }{"tcp", net.JoinHostPort(h, strconv.Itoa(int(ep.Port)))})
	}
	for _, ep := range endpoints {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, e := (&net.Dialer{}).DialContext(probeCtx, ep.network, ep.address)
		cancel()
		if e == nil {
			conn.Close()
			return errRestore
		}
		// A timeout, canceled context, or permission denial does not prove absence.
		if ctx.Err() != nil || !(os.IsNotExist(e) || isOfflineRefused(e)) {
			return errRestore
		}
	}
	return nil
}
func isOfflineRefused(e error) bool { return errors.Is(e, unix.ECONNREFUSED) }

func readOfflineAuthority(path string) (OfflineRestoreAuthority, error) {
	var a OfflineRestoreAuthority
	if os.Geteuid() != 0 {
		return a, errRestore
	}
	pinned, e := safeCutoverFile(path, false)
	if e != nil || pinned.mode != 0600 {
		return a, errRestore
	}
	d, e := privateDirectory(filepath.Dir(path))
	if e != nil {
		return a, e
	}
	defer d.Close()
	var st unix.Stat_t
	if unix.Fstat(int(d.Fd()), &st) != nil || st.Mode&0777 != 0700 {
		return a, errRestore
	}
	if unix.Fstatat(int(d.Fd()), filepath.Base(path), &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Uid != 0 || st.Mode&0777 != 0600 {
		return a, errRestore
	}
	b, e := privateRead(d, filepath.Base(path))
	if e != nil || !bytes.Equal(b, pinned.before) || decodeRestore(b, &a) != nil {
		return a, errRestore
	}
	return a, nil
}

// runOfflineFence journals authorization durably before the first system effect.
// It is also the formal root fixture's fault-injection boundary: no test need
// substitute systemctl or weaken root authorization in RestoreOffline itself.
func runOfflineFence(ctx context.Context, root string, op *RestoreOperation, a offlineSourceActions, save cutoverSave) error {
	op.Phase = "offline_fence_authorized"
	op.UpdatedAt = time.Now()
	if save(root, *op) != nil {
		return errRestore
	}
	if a.Fence(ctx, *op) != nil {
		return errRestore
	}
	op.Phase = "offline_source_fenced"
	op.UpdatedAt = time.Now()
	return save(root, *op)
}

// RestoreOffline prepares a separately provisioned PG17 candidate without a
// live source, SQL maintenance entry, source credentials, or application
// provisioning rights. Its external journal remains authoritative. The only
// successful result is a validated MAINTENANCE candidate; it is NOT cutover.
func (s *Service) RestoreOffline(ctx context.Context, backupID string, c RestoreSettings) (op RestoreOperation, retErr error) {
	started := time.Now()
	if os.Geteuid() != 0 || !c.OfflineSource || c.validate() != nil || !uuidPattern.MatchString(backupID) {
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
	if decodePrivate(c.ReceiptFile, &receipt) != nil {
		return op, errRestore
	}
	authority, e := readOfflineAuthority(c.OfflineAuthorityFile)
	if e != nil || validateOfflineAuthority(authority, receipt, backupID, started) != nil {
		return op, errRestore
	}
	cc := connections.operator.ConnConfig
	if cc.Host != authority.CandidateEndpoint.SocketDirectory || cc.Port != authority.CandidateEndpoint.Port || cc.Database != authority.Candidate.DatabaseName || cc.User == "nmcp_api" || cc.User == "nmcp_worker" || cc.User == "nmcp_backup_dump" || cc.User == "nmcp_backup_control" || cc.User == "nmcp_migrator" {
		return op, errRestore
	}
	hba, e := safeCutoverFile(authority.SourceHBAPath, true)
	if e != nil {
		return op, errRestore
	}
	hash := sha256.Sum256(hba.before)
	if hex.EncodeToString(hash[:]) != authority.SourceHBASHA256 {
		return op, errRestore
	}
	// Reject ALL source DB roles, including all present/future managed logins and
	// migrator, rather than guessing a historical source role inventory.
	hba.after = append(offlineSourceFence(authority.Source.DatabaseName), hba.before...)
	artifactOptions := backupartifact.Options{ReadOnly: true, Lock: true}
	var artifactStat unix.Stat_t
	if unix.Lstat(c.Root, &artifactStat) != nil {
		return op, errRestore
	}
	if artifactStat.Uid != 0 {
		u, e := user.Lookup("nmcp-backup")
		if e != nil {
			return op, errRestore
		}
		uid, e := strconv.ParseUint(u.Uid, 10, 32)
		if e != nil {
			return op, errRestore
		}
		owner := uint32(uid)
		artifactOptions.OwnerUID = &owner
	}
	store, e := backupartifact.Open(c.Root, artifactOptions)
	if e != nil {
		return op, e
	}
	defer store.Close()
	manifest, e := store.Verify(backupID)
	manifestJSON, _ := json.Marshal(manifest)
	authorityJSON, _ := json.Marshal(authority.Manifest)
	if e != nil || !backupJSONEqual(manifestJSON, authorityJSON) || manifest.SnapshotCutoff.After(started) {
		return op, errRestore
	}
	candidate, e := pgxpool.NewWithConfig(ctx, connections.operator)
	if e != nil {
		return op, e
	}
	defer candidate.Close()
	var version int
	if candidate.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version) != nil || version < 170000 || version >= 180000 {
		return op, errRestore
	}
	state, e := systemctl(ctx, "show", "--property=ActiveState", "--value", authority.CandidateUnit)
	pid, pidErr := unitPID(ctx, authority.CandidateUnit)
	if e != nil || state != "active" || pidErr != nil || pid <= 0 {
		return op, errRestore
	}
	identity, e := restoreIdentity(ctx, candidate)
	if e != nil || identity != receipt.Candidate || verifyGate(ctx, candidate, receipt) != nil {
		return op, errRestore
	}
	var empty bool
	if candidate.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) acl WHERE d.datname=current_database() AND acl.grantee=0 AND acl.privilege_type='CONNECT') AND NOT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname NOT IN ('public','pg_catalog','information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp%') AND NOT EXISTS(SELECT 1 FROM pg_extension WHERE extname<>'plpgsql') AND NOT EXISTS(SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public') AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%') AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema')) AND NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND pid<>pg_backend_pid())`).Scan(&empty) != nil || !empty {
		return op, errRestore
	}

	graph, e := restoreRoleGraph(ctx, candidate)
	if e != nil || graph != authority.RoleGraphSHA256 {
		return op, errRestore
	}
	var closed bool
	if candidate.QueryRow(ctx, `SELECT count(*)=5 AND bool_and(NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls) FROM pg_roles WHERE rolname IN ('nmcp_api','nmcp_worker','nmcp_migrator','nmcp_backup_dump','nmcp_backup_control')`).Scan(&closed) != nil || !closed {
		return op, errRestore
	}
	op = RestoreOperation{SourceOffline: true, OfflineAuthority: &authority, RoleGraphSHA256: graph, ID: newID(), BackupID: backupID, DumpSHA256: manifest.DumpSHA256, Source: authority.Source, Candidate: identity, Receipt: receipt, Phase: "prepared", StartedAt: started, UpdatedAt: time.Now(), SnapshotCutoff: manifest.SnapshotCutoff, RPO: started.Sub(manifest.SnapshotCutoff)}
	if reserveCandidate(c.LedgerRoot, identity, op.ID) != nil || SaveRestoreOperation(c.LedgerRoot, op) != nil {
		return op, errRestore
	}
	actions := &offlineSystemSource{hba: hba}
	if runOfflineFence(ctx, c.LedgerRoot, &op, actions, SaveRestoreOperation) != nil {
		return op, errRestore
	}
	phase := func(name string) error {
		op.Phase = name
		op.UpdatedAt = time.Now()
		op.RTO = time.Since(started)
		return SaveRestoreOperation(c.LedgerRoot, op)
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
	if tx.Commit(ctx) != nil || phase("offline_restored") != nil {
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

	maintenance, e := target.MaintenanceStatus(ctx)
	if e != nil || maintenance.Mode != "maintenance" || maintenance.Owner == nil || *maintenance.Owner != op.ID || actions.VerifyStopped(ctx, op) != nil {
		return op, errRestore
	}
	if phase("offline_validated") != nil {
		return op, errRestore
	}
	return op, nil
}

func sameOfflineAuthority(a, b *OfflineRestoreAuthority) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}
func validOfflineOperation(op RestoreOperation) bool {
	if !op.SourceOffline {
		return op.OfflineAuthority == nil
	}
	a := op.OfflineAuthority
	return a != nil && validateOfflineAuthority(*a, op.Receipt, op.BackupID, op.StartedAt) == nil && a.Source == op.Source && a.Candidate == op.Candidate && a.Manifest.DumpSHA256 == op.DumpSHA256 && a.RoleGraphSHA256 == op.RoleGraphSHA256 && a.Manifest.SnapshotCutoff.Equal(op.SnapshotCutoff)
}

func offlineSourceFence(database string) []byte {
	return []byte(fmt.Sprintf("# NMCP offline restore: old source remains fenced\nlocal \"%s\" all reject\nhost \"%s\" all 0.0.0.0/0 reject\nhost \"%s\" all ::/0 reject\n", database, database, database))
}

// VerifyOfflineSourceFence is the cutover boundary: re-check the durable old
// source fence and complete pinned stopped endpoint without source SQL. It does
// not authorize cutover or change any service/configuration. An offline cutover
// backend must preserve this condition, not start/unfence the old source.
func VerifyOfflineSourceFence(ctx context.Context, op RestoreOperation) error {
	if os.Geteuid() != 0 || !op.SourceOffline || !validOfflineOperation(op) {
		return errRestore
	}
	a := op.OfflineAuthority
	current, e := safeCutoverFile(a.SourceHBAPath, true)
	if e != nil {
		return errRestore
	}
	prefix := offlineSourceFence(op.Source.DatabaseName)
	if !bytes.HasPrefix(current.before, prefix) {
		return errRestore
	}
	oldHash := sha256.Sum256(current.before[len(prefix):])
	if hex.EncodeToString(oldHash[:]) != a.SourceHBASHA256 {
		return errRestore
	}
	current.after = current.before
	return (&offlineSystemSource{hba: current}).verifySourceStopped(ctx, op)
}
