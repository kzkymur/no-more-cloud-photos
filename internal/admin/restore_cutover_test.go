package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type fakeCutover struct {
	calls []string
	fail  string
}

func (f *fakeCutover) call(s string) error {
	f.calls = append(f.calls, s)
	if f.fail == s {
		return errors.New("injected failure with credential text never returned")
	}
	return nil
}
func (f *fakeCutover) Preflight(context.Context, RestoreOperation) error { return f.call("preflight") }
func (f *fakeCutover) Stop(context.Context) error                        { return f.call("stop") }
func (f *fakeCutover) Switch(context.Context, RestoreOperation) error    { return f.call("switch") }
func (f *fakeCutover) VerifyTargets(context.Context, RestoreOperation) error {
	return f.call("targets")
}
func (f *fakeCutover) Start(context.Context) error                        { return f.call("start") }
func (f *fakeCutover) VerifyLive(context.Context, RestoreOperation) error { return f.call("live") }
func cutoverTestOperation() RestoreOperation {
	op := restoreTestOperation()
	op.Source.DatabaseName = "source"
	op.Candidate.DatabaseName = "candidate"
	op.Receipt.Source = op.Source
	op.Receipt.Candidate = op.Candidate
	op.Phase = "validated"
	op.StartedAt = time.Now().Add(-time.Minute)
	return op
}
func TestCutoverSuccessDurableAndNeverResume(t *testing.T) {
	root := privateTestDir(t)
	op := cutoverTestOperation()
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	f := &fakeCutover{}
	out, e := runCutover(context.Background(), root, op, f, SaveRestoreOperation)
	if e != nil || out.Phase != "cutover_verified" {
		t.Fatalf("phase=%s err=%v", out.Phase, e)
	}
	want := []string{"preflight", "stop", "switch", "targets", "start", "live"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatal(f.calls)
	}
	latest, e := LoadRestoreOperation(root, op.ID)
	if e != nil || latest.Phase != "cutover_verified" || latest.Source != op.Source || latest.Receipt.Source != op.Receipt.Source || latest.Receipt.Candidate != op.Receipt.Candidate {
		t.Fatalf("ledger invalid: %v", e)
	}
	calls := len(f.calls)
	if _, e := runCutover(context.Background(), root, latest, f, SaveRestoreOperation); e == nil || len(f.calls) != calls {
		t.Fatal("cutover replayed or resumed")
	}
}
func TestCutoverEveryActionFailureStopsAndPersists(t *testing.T) {
	for _, action := range []string{"preflight", "stop", "switch", "targets", "start", "live"} {
		t.Run(action, func(t *testing.T) {
			root := privateTestDir(t)
			op := cutoverTestOperation()
			f := &fakeCutover{fail: action}
			out, e := runCutover(context.Background(), root, op, f, SaveRestoreOperation)
			if !errors.Is(e, errCutover) || out.Phase != "cutover_failed" || out.FailedPhase == "" {
				t.Fatalf("phase=%s failed=%s error=%v", out.Phase, out.FailedPhase, e)
			}
			if f.calls[len(f.calls)-1] != "stop" {
				t.Fatal("failure didn't stop services")
			}
			if strings.Contains(e.Error(), "credential") {
				t.Fatal("raw callback error leaked")
			}
			loaded, e := LoadRestoreOperation(root, op.ID)
			if e != nil || loaded.Phase != "cutover_failed" {
				t.Fatal("failure not durable", e)
			}
			calls := len(f.calls)
			if _, e := runCutover(context.Background(), root, loaded, f, SaveRestoreOperation); e == nil || len(f.calls) != calls {
				t.Fatal("failed operation retried")
			}
		})
	}
}
func TestCutoverEveryLedgerBoundaryFailure(t *testing.T) {
	phases := []string{"cutover_authorized", "cutover_preflight", "cutover_stopped", "cutover_switched", "cutover_targets_verified", "cutover_started", "cutover_verified"}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			f := &fakeCutover{}
			var recorded []string
			save := func(_ string, op RestoreOperation) error {
				recorded = append(recorded, op.Phase)
				if op.Phase == phase {
					return errors.New("fsync")
				}
				return nil
			}
			_, e := runCutover(context.Background(), "unused", cutoverTestOperation(), f, save)
			if e == nil {
				t.Fatal("ledger failure ignored")
			}
			if phase == "cutover_authorized" {
				if len(f.calls) != 0 {
					t.Fatal("action before durable authorization")
				}
			} else if f.calls[len(f.calls)-1] != "stop" {
				t.Fatal("failed durable transition left service running")
			}
			if len(recorded) == 0 {
				t.Fatal("no journal attempt")
			}
		})
	}
}
func TestCutoverRejectUnsupportedAndUnvalidated(t *testing.T) {
	for _, change := range []func(*RestoreOperation){func(op *RestoreOperation) { op.Phase = "restored" }, func(op *RestoreOperation) { op.Source.ClusterID = "other" }, func(op *RestoreOperation) { op.Candidate.DatabaseOID = op.Source.DatabaseOID }, func(op *RestoreOperation) { op.Candidate.DatabaseName = "x\nall all trust" }} {
		op := cutoverTestOperation()
		change(&op)
		f := &fakeCutover{}
		saves := 0
		if _, e := runCutover(context.Background(), "unused", op, f, func(string, RestoreOperation) error { saves++; return nil }); e == nil || saves != 0 || len(f.calls) != 0 {
			t.Fatal("unsafe operation admitted")
		}
	}
}
func TestCutoverOperatorOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		return
	}
	_, e := New(nil).RestoreCutover(context.Background(), newID(), RestoreSettings{})
	if !errors.Is(e, ErrValidation) {
		t.Fatal(e)
	}
}

func TestCutoverEnvRetainsSecretsAndChangesActualConnString(t *testing.T) {
	original := "# private\nNMCP_DATABASE_URL=postgres://nmcp_api:p%3Aa%27ss%5Cword@127.0.0.1:5432/source?sslmode=disable\nOTHER=value\n"
	out, cfg, e := rewriteCutoverEnv([]byte(original), "source", "candidate", "nmcp_api", "unique_api")
	if e != nil {
		t.Fatal(e)
	}
	reparsed, e := pgx.ParseConfig(cfg.ConnString())
	if e != nil || reparsed.Database != "candidate" || reparsed.Password != "p:a'ss\\word" || reparsed.User != "nmcp_api" || reparsed.RuntimeParams["application_name"] != "unique_api" {
		t.Fatal("actual serialized identity/credentials changed incorrectly")
	}
	if !strings.Contains(string(out), "OTHER=value") || !strings.Contains(string(out), "# private") {
		t.Fatal("unrelated configuration lost")
	}
	for _, raw := range []string{
		"host=127.0.0.1 dbname=source user=nmcp_api", "postgres://nmcp_api:p@evil:5432/source?sslmode=disable", "postgres://nmcp_worker:p@127.0.0.1:5432/source?sslmode=disable", "postgres://nmcp_api:p@127.0.0.1:5432/other?sslmode=disable", "postgres://nmcp_api:p@127.0.0.1:5432/source?sslmode=disable&options=-crole%3Dpostgres", "postgres://nmcp_api:p@127.0.0.1:5432/source?sslmode=disable&host=evil", "postgres://nmcp_api:p@127.0.0.1:5432/source?sslmode=disable&dbname=other", "postgres://nmcp_api@127.0.0.1:5432/source?sslmode=disable", "postgres://nmcp_api:p@127.0.0.1:5432/source",
	} {
		if _, _, e := rewriteCutoverEnv([]byte("NMCP_DATABASE_URL="+raw), "source", "candidate", "nmcp_api", "app"); e == nil {
			t.Fatal("unsafe URI accepted")
		}
	}
	for _, extra := range []string{"\nPGOPTIONS=-c role=postgres", "\nNMCP_DATABASE_URL=duplicate", "\n NMCP_DATABASE_URL=hidden", "\nexport NMCP_DATABASE_URL=hidden"} {
		if _, _, e := rewriteCutoverEnv([]byte(original+extra), "source", "candidate", "nmcp_api", "app"); e == nil {
			t.Fatal("environment override accepted")
		}
	}
}
func cutoverServiceFixture() string {
	return "[nmcp_backup_control]\nhost=/var/run/postgresql\nport=5432\ndbname=source\nuser=nmcp_backup_control\n[nmcp_backup_dump]\nhost=/var/run/postgresql\nport=5432\ndbname=source\nuser=nmcp_backup_dump\n"
}
func TestCutoverBackupServicesClosedSet(t *testing.T) {
	op := cutoverTestOperation()
	b, probes, e := rewriteCutoverServices([]byte(cutoverServiceFixture()), op, "/var/run/postgresql", 5432)
	if e != nil || len(probes) != 2 {
		t.Fatal(e)
	}
	if strings.Count(string(b), "dbname=candidate") != 2 {
		t.Fatal("did not switch both backup targets")
	}
	for _, p := range probes {
		if p.config.Database != "candidate" || p.config.User != p.role || p.pass != "/etc/nmcp/credentials/backup-"+strings.TrimPrefix(p.role, "nmcp_backup_")+".pgpass" {
			t.Fatal("credential identity changed")
		}
	}
	for _, s := range []string{strings.ReplaceAll(cutoverServiceFixture(), "dbname=source", "dbname=wrong"), strings.ReplaceAll(cutoverServiceFixture(), "host=/var/run/postgresql", "host=evil"), cutoverServiceFixture() + "[extra]\nuser=postgres\n", cutoverServiceFixture() + "options=-c role=postgres\n", strings.Replace(cutoverServiceFixture(), "user=nmcp_backup_dump", "user=nmcp_backup_control", 1)} {
		if _, _, e := rewriteCutoverServices([]byte(s), op, "/var/run/postgresql", 5432); e == nil {
			t.Fatal("unsafe existing services accepted")
		}
	}
}
func TestCutoverPassfileEscapesAndRejectsReuse(t *testing.T) {
	original := "# secret\nlocalhost:5432:source:nmcp_backup_dump:p\\:a\\\\ss\n"
	out, e := rewriteCutoverPass([]byte(original), "source", "candidate", "nmcp_backup_dump", "localhost", 5432)
	if e != nil || string(out) != "# secret\nlocalhost:5432:candidate:nmcp_backup_dump:p\\:a\\\\ss\n" {
		t.Fatal("credential escaping/identity not retained")
	}
	for _, s := range []string{"*:5432:source:nmcp_backup_dump:p", "localhost:*:source:nmcp_backup_dump:p", "localhost:5432:*:nmcp_backup_dump:p", "localhost:5432:candidate:nmcp_backup_dump:p", "localhost:5432:source:nmcp_backup_control:p", "localhost:5432:source:nmcp_backup_dump:", "localhost:5432:source:nmcp_backup_dump:p\\", original + original} {
		if _, e := rewriteCutoverPass([]byte(s), "source", "candidate", "nmcp_backup_dump", "localhost", 5432); e == nil {
			t.Fatal("unscoped credential entry accepted")
		}
	}
}
func TestCutoverHBAPrependsPermanentSourceFence(t *testing.T) {
	op := cutoverTestOperation()
	old := "local all all peer\nhost all all 0.0.0.0/0 trust\n"
	b := string(cutoverHBA([]byte(old), op))
	for _, line := range []string{`local "source" nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control,nmcp_migrator reject`, `host "source" nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control,nmcp_migrator 0.0.0.0/0 reject`, `host "source" nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control,nmcp_migrator ::/0 reject`, `host "candidate" nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control,nmcp_migrator 127.0.0.1/32 scram-sha-256`, `local "candidate" nmcp_api,nmcp_worker,nmcp_backup_dump,nmcp_backup_control,nmcp_migrator scram-sha-256`} {
		if !strings.Contains(b, line) {
			t.Fatal("missing gate", line)
		}
	}
	if !strings.HasSuffix(b, old) || strings.Index(b, `local "source"`) > strings.Index(b, "local all") {
		t.Fatal("old permissive rule precedes fence")
	}
	if strings.Contains(b[:strings.Index(b, old)], " trust") {
		t.Fatal("trust bypass introduced")
	}
	for _, path := range []string{"/tmp/pg_hba.conf", "/etc/postgresql/../pg_hba.conf", "/etc/postgresql/17/main/other.conf"} {
		if cutoverHBAPath.MatchString(path) {
			t.Fatal("arbitrary HBA path accepted")
		}
	}
	if !cutoverHBAPath.MatchString("/etc/postgresql/17/main/pg_hba.conf") {
		t.Fatal("standard HBA path rejected")
	}
}

func TestCutoverFilePublishAtomicAndMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "api.env")
	if e := os.WriteFile(path, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	dir, e := os.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer dir.Close()
	f := cutoverFile{path: path, before: []byte("old"), after: []byte("new"), mode: 0600, uid: os.Geteuid(), gid: os.Getegid()}
	if e := publishCutoverFile(int(dir.Fd()), f, nil); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "new" {
		t.Fatal("new config not published")
	}
	st, e := os.Stat(path)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal("private config permissions lost")
	}
	if publishCutoverFile(int(dir.Fd()), f, nil) == nil {
		t.Fatal("stale expected source reused")
	}
}
func TestCutoverFileEveryFaultBoundary(t *testing.T) {
	for _, stage := range []string{"write", "file_sync", "rename", "dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "api.env")
			if e := os.WriteFile(path, []byte("old"), 0600); e != nil {
				t.Fatal(e)
			}
			dir, e := os.Open(root)
			if e != nil {
				t.Fatal(e)
			}
			defer dir.Close()
			f := cutoverFile{path: path, before: []byte("old"), after: []byte("new"), mode: 0600, uid: os.Geteuid(), gid: os.Getegid()}
			seen := false
			e = publishCutoverFile(int(dir.Fd()), f, func(s string) error {
				if s == stage {
					seen = true
					return fmt.Errorf("injected %s", stage)
				}
				return nil
			})
			if e == nil || !seen {
				t.Fatal("fault ignored")
			}
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			want := "old"
			if stage == "dir_sync" {
				want = "new"
			}
			if string(b) != want {
				t.Fatal("partial or rolled-back publication")
			}
		})
	}
}
func TestCutoverFileRejectsSymlinkHardlinkAndConcurrentEdit(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "changed", "mode"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "api.env")
			other := filepath.Join(root, "secret")
			if e := os.WriteFile(other, []byte("old"), 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			switch kind {
			case "symlink":
				e = os.Symlink(other, path)
			case "hardlink":
				e = os.Link(other, path)
			case "changed":
				e = os.WriteFile(path, []byte("different"), 0600)
			case "mode":
				e = os.WriteFile(path, []byte("old"), 0600)
				if e == nil {
					e = os.Chmod(path, 0644)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			dir, e := os.Open(root)
			if e != nil {
				t.Fatal(e)
			}
			defer dir.Close()
			f := cutoverFile{path: path, before: []byte("old"), after: []byte("new"), mode: 0600, uid: os.Geteuid(), gid: os.Getegid()}
			if publishCutoverFile(int(dir.Fd()), f, nil) == nil {
				t.Fatal("unsafe existing config reused")
			}
			b, e := os.ReadFile(other)
			if e != nil || string(b) != "old" {
				t.Fatal("unrelated secret modified")
			}
		})
	}
}

func TestCutoverBackupUnitCredentialNamespace(t *testing.T) {
	env := "PGSERVICEFILE=%d/pg_service.conf PGPASSFILE=%d/control.pgpass NMCP_BACKUP_DUMP_PASSFILE=%d/dump.pgpass"
	credentials := "pg_service.conf:/etc/nmcp/backup.pg_service.conf dump.pgpass:/etc/nmcp/credentials/backup-dump.pgpass control.pgpass:/etc/nmcp/credentials/backup-control.pgpass"
	if validateCutoverBackupUnit(env, credentials) != nil {
		t.Fatal("canonical unit rejected")
	}
	if validateCutoverBackupUnit(strings.ReplaceAll(env, "%d/", "/run/credentials/nmcp-backup.service/"), credentials) != nil {
		t.Fatal("expanded canonical namespace rejected")
	}
	for _, bad := range []string{env + " PGHOST=evil", env + " PGSERVICEFILE=%d/other", strings.ReplaceAll(env, "%d/pg_service.conf", "/tmp/config"), ""} {
		if validateCutoverBackupUnit(bad, credentials) == nil {
			t.Fatal("untrusted environment route accepted")
		}
	}
	for _, bad := range []string{credentials + " extra:/tmp/other", strings.ReplaceAll(credentials, "/etc/nmcp/backup.pg_service.conf", "/tmp/other"), ""} {
		if validateCutoverBackupUnit(env, bad) == nil {
			t.Fatal("untrusted credential source accepted")
		}
	}
}

func TestCutoverHBAQuotesKeywordDatabaseNames(t *testing.T) {
	op := cutoverTestOperation()
	op.Source.DatabaseName = "all"
	op.Candidate.DatabaseName = "sameuser"
	b := string(cutoverHBA(nil, op))
	if !strings.Contains(b, `local "all" nmcp_api`) || !strings.Contains(b, `local "sameuser" nmcp_api`) {
		t.Fatal("HBA keyword used instead of exact database identity")
	}
}

func TestCutoverRejectsAmbientRoleAndOperatorPassword(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c role=postgres")
	raw := []byte("NMCP_DATABASE_URL=postgres://nmcp_api:p@127.0.0.1:5432/source?sslmode=disable")
	if _, _, e := rewriteCutoverEnv(raw, "source", "candidate", "nmcp_api", "app"); e == nil {
		t.Fatal("ambient role injection accepted")
	}
	if _, _, e := rewriteCutoverServices([]byte(cutoverServiceFixture()), cutoverTestOperation(), "/var/run/postgresql", 5432); e == nil {
		t.Fatal("ambient backup role injection accepted")
	}
	t.Setenv("PGOPTIONS", "")
	t.Setenv("PGPASSWORD", "private-operator-password-never-use-for-backup")
	_, probes, e := rewriteCutoverServices([]byte(cutoverServiceFixture()), cutoverTestOperation(), "/var/run/postgresql", 5432)
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range probes {
		if p.config.Password == "private-operator-password-never-use-for-backup" {
			t.Fatal("operator credential crossed login boundary")
		}
	}
}

func TestCutoverCandidateGrantsAreClosedAndQuoted(t *testing.T) {
	sql, err := cutoverTargetGrants("all")
	if err != nil || !strings.Contains(sql, `DATABASE "all"`) || !strings.Contains(sql, "FROM PUBLIC") || !strings.Contains(sql, "TO nmcp_api,nmcp_worker,nmcp_migrator,nmcp_backup_control,nmcp_backup_dump") {
		t.Fatal("candidate CONNECT grant is not closed and quoted")
	}
	for _, name := range []string{"", "x;DROP DATABASE source", "other db", strings.Repeat("x", 64)} {
		if _, err := cutoverTargetGrants(name); err == nil {
			t.Fatal("unsafe candidate name accepted")
		}
	}
}

func TestCutoverRuntimeUsesValidatedStorageNamespace(t *testing.T) {
	c := RestoreSettings{StorageRoot: "/var/lib/nmcp/media", FileBaseURL: "https://files.example.test/files/"}
	valid := "NMCP_STORAGE_ROOT=" + c.StorageRoot + "\nNMCP_FILE_BASE_URL=" + c.FileBaseURL + "\n"
	if cutoverRuntimeStorage([]byte(valid), c) != nil {
		t.Fatal("valid runtime namespace rejected")
	}
	for _, bad := range []string{strings.ReplaceAll(valid, c.StorageRoot, "/unrelated/validated-copy"), strings.ReplaceAll(valid, c.FileBaseURL, "https://different.example/files/"), valid + "NMCP_STORAGE_ROOT=" + c.StorageRoot + "\n", "NMCP_STORAGE_ROOT=" + c.StorageRoot + "\n"} {
		if cutoverRuntimeStorage([]byte(bad), c) == nil {
			t.Fatal("unproven or ambiguous runtime FS namespace accepted")
		}
	}
}

func TestCutoverPassEquivalentSocketAliasesOnly(t *testing.T) {
	good := "localhost:5432:source:nmcp_backup_dump:secret\n/run/pg-private:5432:source:nmcp_backup_dump:secret\n"
	out, err := rewriteCutoverPass([]byte(good), "source", "candidate", "nmcp_backup_dump", "localhost", 5432, "/run/pg-private")
	if err != nil || strings.Count(string(out), ":candidate:") != 2 {
		t.Fatal("equivalent exact Unix socket aliases did not preserve one credential")
	}
	for _, bad := range []string{strings.Replace(good, "/run/pg-private", "/run/other-cluster", 1), strings.Replace(good, "secret\n", "different\n", 1), good + "localhost:5432:source:nmcp_backup_dump:secret\n", strings.Replace(good, "/run/pg-private", "*", 1)} {
		if _, err := rewriteCutoverPass([]byte(bad), "source", "candidate", "nmcp_backup_dump", "localhost", 5432, "/run/pg-private"); err == nil {
			t.Fatal("unbound alias or credential accepted")
		}
	}
}

func TestCutoverProducedServicesAdmitIndependentNextOperation(t *testing.T) {
	first := cutoverTestOperation()
	produced, _, err := rewriteCutoverServices([]byte(cutoverServiceFixture()), first, "/var/run/postgresql", 5432)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = newID()
	second.Source = first.Candidate
	second.Candidate.DatabaseName = "next_candidate"
	twice, probes, err := rewriteCutoverServices(produced, second, "/var/run/postgresql", 5432)
	if err != nil || len(probes) != 2 || strings.Count(string(twice), "dbname=next_candidate") != 2 {
		t.Fatal("self-produced config rejected by independent next cutover")
	}
	for _, p := range probes {
		if p.config.RuntimeParams["application_name"] != cutoverApp(second.ID, strings.TrimPrefix(p.role, "nmcp_")) {
			t.Fatal("second operation reused old witness identity")
		}
	}
	bad := strings.ReplaceAll(string(produced), "_backup_dump", "_wrong_role")
	if _, _, err := rewriteCutoverServices([]byte(bad), second, "/var/run/postgresql", 5432); err == nil {
		t.Fatal("unbound prior application witness accepted")
	}
}

func TestOfflineCutoverRetargetsEveryConnectionCoordinate(t *testing.T) {
	op := cutoverTestOperation()
	raw := []byte("NMCP_DATABASE_URL=postgres://nmcp_api:secret@127.0.0.1:5432/source?sslmode=disable\n")
	out, cfg, err := rewriteCutoverEnv(raw, "source", "candidate", "nmcp_api", cutoverApp(op.ID, "api"), 5544)
	if err != nil || cfg.Port != 5544 || cfg.Host != "127.0.0.1" || cfg.Database != "candidate" || !strings.Contains(string(out), "127.0.0.1:5544/candidate") {
		t.Fatal("offline API retarget did not change serialized endpoint")
	}
	target := OfflineRestoreEndpoint{SocketDirectory: "/run/nmcp-target", Port: 5544, LoopbackHosts: []string{}}
	services, probes, err := rewriteCutoverServices([]byte(cutoverServiceFixture()), op, "/var/run/postgresql", 5432, target)
	if err != nil || len(probes) != 2 || strings.Count(string(services), "host=/run/nmcp-target") != 2 || strings.Count(string(services), "port=5544") != 2 {
		t.Fatal("offline control/dump endpoint retarget incomplete")
	}
	for _, p := range probes {
		if p.config.Host != target.SocketDirectory || p.config.Port != target.Port || p.config.Database != op.Candidate.DatabaseName {
			t.Fatal("probe differs from actual service endpoint")
		}
	}
	pass := []byte("localhost:5432:source:nmcp_backup_dump:secret\n/var/run/postgresql:5432:source:nmcp_backup_dump:secret\n")
	changed, err := rewriteCutoverPassTo(pass, "source", "candidate", "nmcp_backup_dump", "localhost", 5432, &target, "/var/run/postgresql")
	if err != nil || strings.Contains(string(changed), ":5432:") || !strings.Contains(string(changed), "/run/nmcp-target:5544:candidate:nmcp_backup_dump:secret") {
		t.Fatal("offline passfile aliases did not follow same target")
	}
}

func TestOfflineTargetHBADoesNotFenceUnrelatedTargetDatabase(t *testing.T) {
	op := cutoverTestOperation()
	op.SourceOffline = true
	old := "local all all peer\n"
	actual := string(cutoverHBA([]byte(old), op))
	prefix := strings.TrimSuffix(actual, old)
	if strings.Contains(prefix, `"source"`) || !strings.Contains(prefix, `"candidate"`) || !strings.HasSuffix(actual, old) {
		t.Fatal("offline target HBA changed authority outside candidate")
	}
}

func TestBackupSourceOnlyPolicyRetargetPreservesSingleBlock(t *testing.T) {
	old := []byte(backupSourceHBABlock("source") + "local all all peer\n")
	next, err := retargetBackupHBAPolicy(old, "candidate")
	if err != nil || strings.Count(string(next), "# BEGIN nmcp backup source-only") != 1 || !strings.Contains(string(next), `local "candidate" nmcp_backup_dump,nmcp_backup_control scram-sha-256`) {
		t.Fatal("source-only policy not retargeted")
	}
	for _, bad := range [][]byte{append(old, old...), []byte(strings.Replace(string(old), "local all nmcp_backup_dump,nmcp_backup_control reject", "local all all trust", 1))} {
		if _, err := retargetBackupHBAPolicy(bad, "candidate"); err == nil {
			t.Fatal("mutated source-only policy accepted")
		}
	}
}
