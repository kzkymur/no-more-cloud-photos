package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
)

func offlineTestAuthority() (OfflineRestoreAuthority, RestoreReceipt) {
	now := time.Now().Add(-time.Minute)
	source := RestoreIdentity{ClusterID: "1234567890123456789", DatabaseOID: 12, DatabaseName: "source_db", Owner: "nmcp_owner"}
	candidate := RestoreIdentity{ClusterID: "2234567890123456789", DatabaseOID: 15, DatabaseName: "candidate_db", Owner: "nmcp_owner"}
	r := RestoreReceipt{Source: source, Candidate: candidate, Template0: true, Empty: true, RoutineLoginDenied: true, OperatorSocketOnly: true, HBASHA256: strings.Repeat("a", 64), IssuedAt: now}
	a := OfflineRestoreAuthority{SourceBackupSucceeded: true, Source: source, Candidate: candidate, Manifest: backupartifact.Manifest{Metadata: backupartifact.Metadata{ID: "12345678-1234-4123-8123-123456789abc", SnapshotCutoff: now.Add(-time.Hour)}, DumpSHA256: strings.Repeat("b", 64), DumpSize: 128}, RoleGraphSHA256: strings.Repeat("c", 64), SourceHBAPath: "/etc/postgresql/17/source/pg_hba.conf", SourceHBASHA256: strings.Repeat("d", 64), SourceUnit: "postgresql@17-source.service", CandidateUnit: "postgresql@17-candidate.service", SourceEndpoint: OfflineRestoreEndpoint{SocketDirectory: "/run/postgresql/source", Port: 5432, LoopbackHosts: []string{"127.0.0.1", "::1"}}, CandidateEndpoint: OfflineRestoreEndpoint{SocketDirectory: "/run/postgresql/candidate", Port: 5433, LoopbackHosts: []string{}}, EndpointComplete: true, IssuedAt: now, Confirmation: offlineConfirmation}
	return a, r
}
func TestOfflineAuthorityBindingsFailBeforeEffects(t *testing.T) {
	valid, r := offlineTestAuthority()
	if e := validateOfflineAuthority(valid, r, valid.Manifest.ID, time.Now()); e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(*OfflineRestoreAuthority){
		"unattested_source_success": func(a *OfflineRestoreAuthority) { a.SourceBackupSucceeded = false },
		"wrong_backup":              func(a *OfflineRestoreAuthority) { a.Manifest.ID = "22345678-1234-4123-8123-123456789abc" },
		"wrong_candidate":           func(a *OfflineRestoreAuthority) { a.Candidate.DatabaseOID++ },
		"non_numeric_source":        func(a *OfflineRestoreAuthority) { a.Source.ClusterID = "unknown" },
		"unknown_source":            func(a *OfflineRestoreAuthority) { a.Source.ClusterID = "" },
		"wildcard_source_name": func(a *OfflineRestoreAuthority) {
			a.Source.DatabaseName = "all"
			a.Source.DatabaseName += " replication"
		},
		"source_unit_version":     func(a *OfflineRestoreAuthority) { a.SourceUnit = "postgresql@18-source.service" },
		"source_unit_arbitrary":   func(a *OfflineRestoreAuthority) { a.SourceUnit = "evil.service" },
		"source_unit_regex_dot":   func(a *OfflineRestoreAuthority) { a.SourceUnit = "postgresql@17-sourceXservice" },
		"candidate_unit_same":     func(a *OfflineRestoreAuthority) { a.CandidateUnit = a.SourceUnit },
		"path_cluster_mismatch":   func(a *OfflineRestoreAuthority) { a.SourceHBAPath = "/etc/postgresql/17/other/pg_hba.conf" },
		"path_traversal":          func(a *OfflineRestoreAuthority) { a.SourceHBAPath = "/etc/postgresql/17/source/../source/pg_hba.conf" },
		"hash":                    func(a *OfflineRestoreAuthority) { a.SourceHBASHA256 = "bad" },
		"role_graph":              func(a *OfflineRestoreAuthority) { a.RoleGraphSHA256 = "" },
		"digest":                  func(a *OfflineRestoreAuthority) { a.Manifest.DumpSHA256 = "" },
		"size":                    func(a *OfflineRestoreAuthority) { a.Manifest.DumpSize = 0 },
		"unknown_endpoints":       func(a *OfflineRestoreAuthority) { a.EndpointComplete = false },
		"source_default_port":     func(a *OfflineRestoreAuthority) { a.SourceEndpoint.Port = 0 },
		"source_default_host":     func(a *OfflineRestoreAuthority) { a.SourceEndpoint.SocketDirectory = "" },
		"source_tcp_wildcard":     func(a *OfflineRestoreAuthority) { a.SourceEndpoint.LoopbackHosts = []string{"0.0.0.0"} },
		"source_tcp_remote":       func(a *OfflineRestoreAuthority) { a.SourceEndpoint.LoopbackHosts = []string{"192.0.2.1"} },
		"missing_tcp_attestation": func(a *OfflineRestoreAuthority) { a.SourceEndpoint.LoopbackHosts = nil },
		"candidate_tcp":           func(a *OfflineRestoreAuthority) { a.CandidateEndpoint.LoopbackHosts = []string{"127.0.0.1"} },
		"candidate_endpoint_same": func(a *OfflineRestoreAuthority) {
			a.CandidateEndpoint = a.SourceEndpoint
			a.CandidateEndpoint.LoopbackHosts = []string{}
		},
		"same_cluster": func(a *OfflineRestoreAuthority) { a.Candidate.ClusterID = a.Source.ClusterID },
		"confirmation": func(a *OfflineRestoreAuthority) { a.Confirmation = "yes" },
		"expired":      func(a *OfflineRestoreAuthority) { a.IssuedAt = time.Now().Add(-2 * time.Hour) },
		"future":       func(a *OfflineRestoreAuthority) { a.IssuedAt = time.Now().Add(time.Hour) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := valid
			change(&a)
			if validateOfflineAuthority(a, r, valid.Manifest.ID, time.Now()) == nil {
				t.Fatal("invalid external authority accepted")
			}
		})
	}
}

type offlineFenceRecorder struct {
	calls []string
	fail  bool
}

func (a *offlineFenceRecorder) Fence(context.Context, RestoreOperation) error {
	a.calls = append(a.calls, "fence")
	if a.fail {
		return errors.New("stop failed")
	}
	return nil
}
func (a *offlineFenceRecorder) VerifyStopped(context.Context, RestoreOperation) error { return nil }
func TestOfflineFenceRequiresDurableAuthorizationBeforeEffects(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		a := &offlineFenceRecorder{}
		op := RestoreOperation{}
		save := func(_ string, op RestoreOperation) error {
			a.calls = append(a.calls, op.Phase)
			if failSave {
				return errors.New("fsync failed")
			}
			return nil
		}
		e := runOfflineFence(context.Background(), "unused", &op, a, save)
		want := "offline_fence_authorized,fence,offline_source_fenced"
		if failSave {
			want = "offline_fence_authorized"
			if e == nil {
				t.Fatal("accepted undurable authorization")
			}
		} else if e != nil {
			t.Fatal(e)
		}
		if strings.Join(a.calls, ",") != want {
			t.Fatalf("effects ordering %v", a.calls)
		}
	}
	a := &offlineFenceRecorder{fail: true}
	op := RestoreOperation{}
	if runOfflineFence(context.Background(), "unused", &op, a, func(_ string, op RestoreOperation) error { return nil }) == nil || op.Phase == "offline_source_fenced" {
		t.Fatal("failed source stop claimed fenced")
	}
}

type offlineFenceReasonSource struct{ err error }

func (a offlineFenceReasonSource) Fence(context.Context, RestoreOperation) error { return a.err }
func (a offlineFenceReasonSource) VerifyStopped(context.Context, RestoreOperation) error {
	return nil
}
func TestOfflineFenceFailureRecordsClosedReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{offlineFenceFailure("not inactive nmcp-api.service (failed)"), "not inactive nmcp-api.service (failed)"},
		{fmt.Errorf("wrapped: %w", offlineFenceFailure("stop command")), "stop command"},
		{errors.New("raw SECRET systemctl stderr"), "unclassified"},
	} {
		op := RestoreOperation{}
		e := runOfflineFence(context.Background(), "unused", &op, offlineFenceReasonSource{c.err}, func(string, RestoreOperation) error { return nil })
		if !errors.Is(e, errRestore) || op.FenceFailure != c.want || op.Phase != "offline_fence_authorized" || strings.Contains(e.Error(), "SECRET") {
			t.Fatalf("err=%v reason=%q phase=%s", e, op.FenceFailure, op.Phase)
		}
	}
	if !errors.Is(offlineFenceFailure("x"), errRestore) {
		t.Fatal("fence reason must remain a restore error")
	}
	for unit, want := range map[string]string{
		"nmcp-api.service": "nmcp-api.service", "postgresql@17-nmcp-offline-fixture.service": "postgresql@17-nmcp-offline-fixture.service",
		"evil.service": "<invalid unit>", "postgresql@17-x;rm.service": "<invalid unit>",
	} {
		if got := offlineFenceUnit(unit); got != want {
			t.Fatalf("unit %q -> %q", unit, got)
		}
	}
	for state, want := range map[string]string{"failed": "failed", "inactive": "inactive", "SECRET\nvalue": "unknown", "": "unknown"} {
		if got := offlineActiveStateLabel(state); got != want {
			t.Fatalf("state %q -> %q", state, got)
		}
	}
}
func TestOfflineTimerStoppedWithoutMainPID(t *testing.T) {
	saved := offlineTimerShow
	defer func() { offlineTimerShow = saved }()
	for out, ok := range map[string]bool{
		// Observed systemd 255 timer state has no MainPID; an unrequested property is rejected.
		"Result=success\nLoadState=loaded\nActiveState=inactive\nSubState=dead":     false, // extra property is not the queried set
		"ActiveState=inactive\nSubState=dead\nResult=success":                       true,
		"Result=success\nActiveState=inactive\nSubState=dead":                       true,
		"ActiveState=active\nSubState=waiting\nResult=success":                      false,
		"ActiveState=inactive\nSubState=dead\nResult=resources":                     false,
		"ActiveState=inactive\nSubState=elapsed\nResult=success":                    false,
		"ActiveState=inactive\nSubState=dead":                                       false,
		"ActiveState=inactive\nActiveState=inactive\nSubState=dead\nResult=success": false,
		"": false,
	} {
		offlineTimerShow = func(context.Context, string) (string, error) { return out, nil }
		err := offlineTimerStopped(context.Background(), "nmcp-backup.timer")
		if (err == nil) != ok {
			t.Fatalf("timer state %q accepted=%t err=%v", out, err == nil, err)
		}
		if err != nil && !errors.Is(err, errRestore) {
			t.Fatalf("timer reason must remain a restore error: %v", err)
		}
	}
	offlineTimerShow = func(context.Context, string) (string, error) { return "", errors.New("SECRET query") }
	if err := offlineTimerStopped(context.Background(), "nmcp-backup.timer"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("query failure: %v", err)
	}
	if offlineFenceUnit("nmcp-migrate.service") != "nmcp-migrate.service" || offlineFenceUnit("nmcp-migrate.timer") != "<invalid unit>" {
		t.Fatal("diagnostic unit labels")
	}
}
func TestOfflineAuthorityRejectsUnknownJSONAndRoutineUID(t *testing.T) {
	var a OfflineRestoreAuthority
	if decodeRestore([]byte(`{"unexpected":true}`), &a) == nil {
		t.Fatal("unknown field accepted")
	}
	if os.Geteuid() != 0 {
		if _, e := readOfflineAuthority("/etc/nmcp/authority.json"); e == nil {
			t.Fatal("routine UID accepted root authority")
		}
		if _, e := New(nil).RestoreOffline(context.Background(), "12345678-1234-4123-8123-123456789abc", RestoreSettings{}); e == nil {
			t.Fatal("routine UID accepted offline invocation")
		}
	}
}

// This is a real, destructive, root/systemd PG17 fixture entry point, NOT a
// component-test substitute or evidence of a completed run. The formal lane
// must preprovision two dedicated clusters, successful dump + DBFS, root-private
// receipt/authority/service/pass inputs, and stopped application fixture units.
// It must not point at production. UID1001 cannot execute this authorization.
// These root-only production-entrypoint negatives run against the real empty
// candidate BEFORE the positive fixture. A missing operation ID proves rejection
// precedes reservation, system actions and pg_restore, not just eventual failure.
func offlineRootNegativeInputs(t *testing.T, c RestoreSettings, authority OfflineRestoreAuthority) {
	t.Helper()
	variants := map[string]func(*OfflineRestoreAuthority){
		"wrong-backup":             func(a *OfflineRestoreAuthority) { a.Manifest.ID = newID() },
		"wrong-candidate":          func(a *OfflineRestoreAuthority) { a.Candidate.DatabaseOID++ },
		"wrong-unit":               func(a *OfflineRestoreAuthority) { a.SourceUnit = "postgresql@17-other.service" },
		"wrong-path":               func(a *OfflineRestoreAuthority) { a.SourceHBAPath = "/etc/postgresql/17/other/pg_hba.conf" },
		"wrong-hba-hash":           func(a *OfflineRestoreAuthority) { a.SourceHBASHA256 = strings.Repeat("0", 64) },
		"wrong-dump-hash":          func(a *OfflineRestoreAuthority) { a.Manifest.DumpSHA256 = strings.Repeat("0", 64) },
		"wrong-role-graph":         func(a *OfflineRestoreAuthority) { a.RoleGraphSHA256 = strings.Repeat("0", 64) },
		"wrong-candidate-endpoint": func(a *OfflineRestoreAuthority) { a.CandidateEndpoint.Port++ },
		"wrong-size":               func(a *OfflineRestoreAuthority) { a.Manifest.DumpSize++ },
		"wrong-fullmanifest":       func(a *OfflineRestoreAuthority) { a.Manifest.Config = json.RawMessage(`{}`) },
		"default-port":             func(a *OfflineRestoreAuthority) { a.SourceEndpoint.Port = 0 },
		"default-host":             func(a *OfflineRestoreAuthority) { a.SourceEndpoint.SocketDirectory = "" },
		"public-input-acl":         func(a *OfflineRestoreAuthority) {},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			a := authority
			mutate(&a)
			path := filepath.Join(filepath.Dir(c.OfflineAuthorityFile), "negative-"+newID()+".json")
			b, e := json.Marshal(a)
			if e != nil {
				t.Fatal(e)
			}
			mode := os.FileMode(0600)
			if name == "public-input-acl" {
				mode = 0644
			}
			file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				t.Fatal(e)
			}
			if e = file.Chmod(mode); e != nil {
				file.Close()
				t.Fatal(e)
			}
			if _, e = file.Write(b); e != nil {
				file.Close()
				t.Fatal(e)
			}
			if file.Sync() != nil || file.Close() != nil {
				t.Fatal("negative authority durability")
			}
			// Keep each private fixture input as failure evidence, never alter the
			// selected positive authority and never remove unknown fixture files.
			c.OfflineAuthorityFile = path
			op, e := New(nil).Restore(context.Background(), authority.Manifest.ID, c)
			if e == nil || op.ID != "" {
				t.Fatalf("invalid authority passed pre-effect boundary: %s", op.Phase)
			}
		})
	}
}
func TestOfflineLedgerPreservesAuthorityAndRejectsDrift(t *testing.T) {
	a, r := offlineTestAuthority()
	op := RestoreOperation{ID: newID(), BackupID: a.Manifest.ID, DumpSHA256: a.Manifest.DumpSHA256, Source: a.Source, Candidate: a.Candidate, Receipt: r, RoleGraphSHA256: a.RoleGraphSHA256, SourceOffline: true, OfflineAuthority: &a, StartedAt: time.Now(), UpdatedAt: time.Now(), Phase: "prepared", SnapshotCutoff: a.Manifest.SnapshotCutoff}
	root := privateTestDir(t)
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	op.Phase = "offline_source_fenced"
	op.UpdatedAt = time.Now()
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	loaded, e := LoadRestoreOperation(root, op.ID)
	if e != nil || !loaded.SourceOffline || !sameOfflineAuthority(loaded.OfflineAuthority, &a) {
		t.Fatal("lost offline source pins")
	}
	// Each phase is individually well-formed, but changing a pinned endpoint
	// across append-only snapshots must prevent any later cutover authorization.
	a.SourceEndpoint.Port++
	op.Phase = "offline_validated"
	op.UpdatedAt = time.Now()
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadRestoreOperation(root, op.ID); e == nil {
		t.Fatal("endpoint drift accepted")
	}
}

func TestOfflineEndpointErrorClassUsesRealDialErrors(t *testing.T) {
	// Short directory: a Unix socket path must fit sun_path (108 bytes), else
	// the dial fails with EINVAL, which is correctly a fail-closed "other".
	dir, err := os.MkdirTemp("/tmp", "nmcp-sock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// A cleanly stopped postmaster removes its socket: the real dial error is a
	// *net.OpError wrapping ENOENT, which os.IsNotExist does not recognise.
	_, missing := net.Dial("unix", filepath.Join(dir, ".s.PGSQL.5432"))
	if missing == nil || os.IsNotExist(missing) || offlineEndpointErrorClass(missing) != "absent" {
		t.Fatalf("missing socket: %v class=%s", missing, offlineEndpointErrorClass(missing))
	}
	// A stale socket file with no listener is refused, which also proves absence.
	stale := filepath.Join(dir, ".s.PGSQL.5433")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(stale); err != nil {
		t.Fatal("stale socket file should remain")
	}
	_, refused := net.Dial("unix", stale)
	if refused == nil || offlineEndpointErrorClass(refused) != "refused" {
		t.Fatalf("stale socket: %v class=%s", refused, offlineEndpointErrorClass(refused))
	}
	// A live listener is never classified; it fails the probe before classification.
	live := filepath.Join(dir, ".s.PGSQL.5434")
	ll, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()
	if c, err := net.Dial("unix", live); err != nil {
		t.Fatal("live listener must accept")
	} else {
		c.Close()
	}
	// Every other class fails closed.
	for e, want := range map[error]string{
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EACCES)}:       "permission",
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EPERM)}:        "permission",
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EINVAL)}:       "other",
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENAMETOOLONG)}: "other",
		context.DeadlineExceeded: "timeout",
		errors.New("unknown"):    "other",
	} {
		if got := offlineEndpointErrorClass(e); got != want {
			t.Fatalf("%v -> %s, want %s", e, got, want)
		}
	}
}
func TestOfflineFenceRejectsEveryRoleIncludingMigratorAndQuotesDatabase(t *testing.T) {
	fence := string(offlineSourceFence("all"))
	for _, rule := range []string{`local "all" all reject`, `host "all" all 0.0.0.0/0 reject`, `host "all" all ::/0 reject`} {
		if !strings.Contains(fence, rule+"\n") {
			t.Fatalf("missing persistent source fence %q", rule)
		}
	}
	if !isOfflineRefused(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}) {
		t.Fatal("actual syscall refusal not recognized")
	}
	if isOfflineRefused(context.DeadlineExceeded) || isOfflineRefused(os.ErrPermission) {
		t.Fatal("unproven endpoint absence accepted")
	}
}

func TestOfflineRestoreCLIExplicitMode(t *testing.T) {
	id := newID()
	o, err := parse([]string{"restore", "--backup-id", id, "--offline-source", "--offline-authority", "/root/offline.json", "--yes", "--json"})
	if err != nil || !o.offlineSource || o.offlineAuthority != "/root/offline.json" || o.id != id {
		t.Fatal("explicit offline canonical backup ID parse")
	}
	if _, err = parse([]string{"restore", "--backup-id", id, "--offline-authority", "/root/offline.json", "--yes"}); err == nil {
		t.Fatal("offline authority silently selected mode")
	}
	if _, err = parse([]string{"restore", "--backup-id", id, "--offline-source"}); err == nil {
		t.Fatal("offline mutation did not require yes")
	}
}
