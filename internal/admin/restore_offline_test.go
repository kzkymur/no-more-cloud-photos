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
func TestRestoreOfflineRootSystemdFixture(t *testing.T) {
	path := os.Getenv("NMCP_OFFLINE_ROOT_FIXTURE_SETTINGS")
	if path == "" {
		t.Skip("formal root/systemd two-cluster fixture required; not executed locally")
	}
	if os.Geteuid() != 0 {
		t.Fatal("formal offline fixture requires genuine root")
	}
	var c RestoreSettings
	if decodePrivate(path, &c) != nil {
		t.Fatal("invalid root-private fixture settings")
	}
	a, e := readOfflineAuthority(c.OfflineAuthorityFile)
	if e != nil {
		t.Fatal(e)
	}
	if a.SourceUnit != "postgresql@17-nmcp-offline-fixture.service" || a.CandidateUnit != "postgresql@17-nmcp-offline-candidate.service" {
		t.Fatal("dedicated fixture cluster units required")
	}
	if !c.OfflineSource {
		t.Fatal("fixture must explicitly request offline preparation")
	}
	// A formal causal run begins with a live dedicated source, then proves that
	// the production action actually stopped it (not merely an already-dead DB).
	initialPID, e := unitPID(context.Background(), a.SourceUnit)
	active, activeErr := systemctl(context.Background(), "show", "--property=ActiveState", "--value", a.SourceUnit)
	if e != nil || initialPID <= 0 || activeErr != nil || active != "active" {
		t.Fatal("formal causal fixture source must initially run")
	}
	connection, e := net.DialTimeout("unix", filepath.Join(a.SourceEndpoint.SocketDirectory, fmt.Sprintf(".s.PGSQL.%d", a.SourceEndpoint.Port)), time.Second)
	if e != nil {
		t.Fatal("formal causal fixture source must initially be reachable")
	}
	connection.Close()
	offlineRootNegativeInputs(t, c, a)
	// Service source pool is deliberately nil: any accidental live-source access
	// fails this fixture instead of being hidden by a connected source service.
	op, e := New(nil).Restore(context.Background(), a.Manifest.ID, c)
	if e != nil {
		t.Fatalf("offline prepare: %v; operation %s phase %s", e, op.ID, op.Phase)
	}
	if op.Phase != "offline_validated" || !op.SourceOffline || op.OfflineAuthority == nil {
		t.Fatalf("not offline preparation: %+v", op)
	}
	loaded, e := LoadRestoreOperation(c.LedgerRoot, op.ID)
	if e != nil {
		t.Fatal(e)
	}
	before, _ := json.Marshal(op.OfflineAuthority)
	after, _ := json.Marshal(loaded.OfflineAuthority)
	if !strings.EqualFold(string(before), string(after)) {
		t.Fatal("external source pins not durable")
	}
	if _, e := os.Stat(fmt.Sprintf("/proc/%d", initialPID)); !os.IsNotExist(e) {
		t.Fatal("original source postmaster still exists")
	}
	if offlineInactive(context.Background(), a.SourceUnit) != nil {
		t.Fatal("source restarted during preparation")
	}
	target, e := restorePool(context.Background(), c, c.Service)
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	state, e := New(target).MaintenanceStatus(context.Background())
	if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
		t.Fatal("candidate not maintenance owned by operation")
	}
}

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
