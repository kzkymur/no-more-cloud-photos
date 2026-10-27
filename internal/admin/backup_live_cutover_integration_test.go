package admin

// This gate is intentionally destructive ONLY within a newly created fixture on
// an isolated GitHub-hosted Linux runner. It must never run on a customer host.
// No fake cutoverActions, substitute core executables, external PostgreSQL, or
// manually restored archive is accepted as evidence. Ordinary developer/UID1001
// runs skip honestly; opt-in root runs fail closed on every missing prerequisite.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

var liveUnits = []string{"nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service", "nmcp-migrate.service", "nmcp.slice"}

type liveCutoverFixture struct {
	*pg17Fixture
	owned          map[string]os.FileInfo
	postmaster     int
	hbaPath        string
	managedUnits   []string
	managedServers []*liveCutoverFixture
}

func liveRequired(t *testing.T, key string, executable bool) string {
	t.Helper()
	p := os.Getenv(key)
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		t.Fatalf("%s must name an absolute clean real artifact path", key)
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || executable && st.Mode()&0111 == 0 {
		t.Fatalf("%s artifact absent or not executable", key)
	}
	// Text/script helper stand-ins are forbidden. The formal job supplies the
	// exact-head native helper closure and actual Go product binaries as artifacts.
	if executable {
		b, err := os.ReadFile(p)
		if err != nil || len(b) < 4 || string(b[:4]) != "\x7fELF" {
			t.Fatalf("%s must be a real ELF product/helper, not a script stub", key)
		}
	}
	return p
}

func (f *liveCutoverFixture) run(path string, args ...string) string {
	f.t.Helper()
	c := exec.CommandContext(f.ctx, path, args...)
	c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	var out, diagnostic bytes.Buffer
	c.Stdout = &out
	c.Stderr = &diagnostic
	if err := c.Run(); err != nil {
		f.t.Fatalf("live fixture command %s %s failed (output suppressed)", filepath.Base(path), strings.Join(args, " "))
	}
	return strings.TrimSpace(out.String())
}
func (f *liveCutoverFixture) ownDir(path string, mode os.FileMode, owner string) {
	f.t.Helper()
	// Every fixed root was checked absent before *any* mutation. Mkdir rather
	// than MkdirAll detects races; parent directories are never removed by us.
	if err := os.Mkdir(path, mode); err != nil {
		f.t.Fatalf("create exclusively owned fixture directory %s", path)
	}
	st, err := os.Lstat(path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.owned[path] = st
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			f.t.Fatal(err)
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if os.Chown(path, uid, gid) != nil {
			f.t.Fatal("fixture chown")
		}
	}
}
func (f *liveCutoverFixture) dsnLive(role, db string) string {
	return "host=" + quoteConnValue(f.socket) + " port=5432 dbname=" + quoteConnValue(db) + " user=" + role + " sslmode=disable password=" + quoteConnValue(f.passwords[role])
}
func (f *liveCutoverFixture) poolLive(role, db string) *pgxpool.Pool {
	f.t.Helper()
	p, err := pgxpool.New(f.ctx, f.dsnLive(role, db))
	if err != nil {
		f.t.Fatal("parse live pool")
	}
	f.t.Cleanup(p.Close)
	if p.Ping(f.ctx) != nil {
		f.t.Fatal("connect live fixture pool")
	}
	return p
}
func (f *liveCutoverFixture) bootstrapLive(after bool) {
	// libpq bootstrap receives only a root-private passfile path, never a
	// password-bearing DSN in argv. Routine credentials cannot read this file.
	pass := "/etc/nmcp/credentials/bootstrap-operator.pgpass"
	f.write(pass, fmt.Sprintf("localhost:5432:source:operator:%s\n%s:5432:source:operator:%s\n", f.passwords["operator"], f.socket, f.passwords["operator"]), 0600)
	dsn := "host=" + quoteConnValue(f.socket) + " port=5432 dbname=source user=operator sslmode=disable passfile=" + quoteConnValue(pass)
	args := []string{"-X", "--no-password", "--set=ON_ERROR_STOP=1", "--dbname=" + dsn}
	file := "--file=" + filepath.Join(pg17Repository(f.t), "infra/ubuntu/postgresql-roles.sql")
	if after {
		f.command("psql", nil, append(args, "--set=nmcp_backup_grants_after_migration=1", "--set=nmcp_migration_compatibility=0", file)...)
		return
	}
	f.command("psql", nil, append(args, "--set=nmcp_migration_compatibility=0", file)...)
	f.command("psql", nil, append(args, "--set=nmcp_migration_compatibility=1", file)...)
}

func newLiveCutoverFixture(t *testing.T, bin string, clusters ...string) *liveCutoverFixture {
	cluster := "backup-ci"
	if len(clusters) > 0 {
		if len(clusters) != 1 || clusters[0] != "nmcp-offline-fixture" {
			t.Fatal("unsupported fixture cluster")
		}
		cluster = clusters[0]
	}
	hbaPath := "/etc/postgresql/17/" + cluster + "/pg_hba.conf"
	socketPath := "/run/nmcp-" + cluster
	if cluster == "nmcp-offline-fixture" {
		socketPath = "/run/nmcp-offline-fixture"
	}
	t.Helper()
	// No host process may be reused, even if its SQL happens to look suitable.
	processes, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range processes {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		exe, _ := os.Readlink("/proc/" + p.Name() + "/exe")
		if filepath.Base(exe) == "postgres" || filepath.Base(exe) == "postmaster" {
			t.Fatal("existing PostgreSQL process: isolated runner required")
		}
	}
	for _, addr := range []string{"127.0.0.1:5432", "127.0.0.1:8080"} {
		c, e := net.DialTimeout("tcp", addr, time.Second)
		if e == nil {
			c.Close()
			t.Fatal("fixture TCP port already occupied")
		}
	}
	// Refuse symlinked or writable host ancestors before creating anything.
	for _, leaf := range []string{"/etc/systemd/system", "/var/lib/systemd/timers", "/etc/postgresql/17", "/var/lib", "/run", "/opt"} {
		for p := leaf; p != "/"; p = filepath.Dir(p) {
			info, e := os.Lstat(p)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
				t.Fatalf("unsafe fixture ancestor %s", p)
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || st.Uid != 0 {
				t.Fatalf("non-root fixture ancestor %s", p)
			}
		}
	}
	fixed := []string{"/run/nmcp-backup-cutover.owner", "/var/lib/systemd/timers/stamp-nmcp-backup.timer", "/etc/systemd/system/timers.target.wants/nmcp-backup.timer", "/etc/nmcp", "/var/lib/nmcp", "/var/lib/nmcp-backup", "/var/lib/nmcp-restore", filepath.Dir(hbaPath), "/opt/nmcp", socketPath}
	for _, p := range fixed {
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			t.Fatalf("refuse preexisting/uninspectable fixed path %s", p)
		}
	}
	for _, u := range append(append([]string{}, liveUnits...), "nmcp.target") {
		c := exec.Command("/usr/bin/systemctl", "show", "--property=LoadState", "--value", u)
		b, e := c.Output()
		if e != nil || strings.TrimSpace(string(b)) != "not-found" {
			t.Fatalf("refuse existing/uninspectable runtime unit %s", u)
		}
		for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
			if _, e := os.Lstat(filepath.Join(dir, u)); !os.IsNotExist(e) {
				t.Fatalf("preexisting unit path %s", u)
			}
			if _, e := os.Lstat(filepath.Join(dir, u+".d")); !os.IsNotExist(e) {
				t.Fatalf("preexisting unit drop-in %s", u)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	f := &liveCutoverFixture{pg17Fixture: &pg17Fixture{t: t, ctx: ctx, bin: bin, passwords: map[string]string{}, passfiles: map[string]string{}}, owned: map[string]os.FileInfo{}}
	t.Cleanup(cancel)
	// Cleanup is registered before the first mutation, and only removes exact
	// roots whose directory identity still matches the fixture's exclusive mkdir.
	f.hbaPath = hbaPath
	t.Cleanup(f.cleanupLive)
	// A timeout cleanup may touch fixed units only after this exclusive root
	// receipt proves that this exact-head fixture, not another host owner, began.
	head := os.Getenv("TEST_BACKUP_CANDIDATE_SHA")
	decoded, decodeErr := hex.DecodeString(head)
	if decodeErr != nil || len(decoded) != 20 || strings.ToLower(head) != head {
		t.Fatal("live fixture requires exact candidate SHA")
	}
	marker, markerErr := os.OpenFile("/run/nmcp-backup-cutover.owner", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if markerErr != nil {
		t.Fatal("exclusive live fixture owner receipt")
	}
	if _, markerErr = marker.WriteString(head + "\n"); markerErr == nil {
		markerErr = marker.Sync()
	}
	closeErr := marker.Close()
	if markerErr != nil || closeErr != nil {
		t.Fatal("durable live fixture owner receipt")
	}
	markerInfo, markerErr := os.Lstat("/run/nmcp-backup-cutover.owner")
	if markerErr != nil {
		t.Fatal("owner receipt identity")
	}
	f.owned["/run/nmcp-backup-cutover.owner"] = markerInfo
	for _, p := range []string{"/etc/postgresql", "/etc/postgresql/17"} {
		if _, e := os.Lstat(p); os.IsNotExist(e) {
			f.ownDir(p, 0755, "")
		} else if e != nil {
			t.Fatal("inspect HBA parent")
		}
	}
	for _, p := range []string{"/etc/nmcp", "/var/lib/nmcp", "/var/lib/nmcp-restore", filepath.Dir(hbaPath), "/opt/nmcp", socketPath} {
		f.ownDir(p, 0755, "")
	}
	if os.Chmod("/etc/nmcp", 0700) != nil {
		t.Fatal("exact private operator configuration root")
	}
	if os.Chmod("/var/lib/nmcp-restore", 0700) != nil {
		t.Fatal("ledger privacy")
	}
	f.ownDir("/var/lib/nmcp-backup", 0700, "nmcp-backup")
	pgSocketOwner, _ := user.Lookup("postgres")
	socketUID, _ := strconv.Atoi(pgSocketOwner.Uid)
	socketGID, _ := strconv.Atoi(pgSocketOwner.Gid)
	if os.Chown(socketPath, socketUID, socketGID) != nil {
		t.Fatal("socket directory owner")
	}
	runtimeUser, _ := user.Lookup("nmcp")
	runtimeUID, _ := strconv.Atoi(runtimeUser.Uid)
	runtimeGID, _ := strconv.Atoi(runtimeUser.Gid)
	if os.Chown("/var/lib/nmcp", runtimeUID, runtimeGID) != nil || os.Chmod("/var/lib/nmcp", 0700) != nil {
		t.Fatal("exact runtime parent ownership/privacy")
	}
	f.ownDir("/var/lib/nmcp/media", 0700, "nmcp")
	f.ownDir("/etc/nmcp/credentials", 0700, "")
	root, err := os.MkdirTemp("/tmp", "nmcp-live-pg17-")
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	f.owned[root] = st
	f.root = root
	f.socket = socketPath
	f.data = filepath.Join(root, "data")
	pg, _ := user.Lookup("postgres")
	uid, _ := strconv.Atoi(pg.Uid)
	gid, _ := strconv.Atoi(pg.Gid)
	if os.Chown(root, uid, gid) != nil || os.Chmod(root, 0755) != nil {
		t.Fatal("PG fixture ownership")
	}
	for _, role := range []string{"operator", "nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		var secret [32]byte
		if _, e := rand.Read(secret[:]); e != nil {
			t.Fatal(e)
		}
		f.passwords[role] = hex.EncodeToString(secret[:])
	}
	pw := filepath.Join(root, "init.password")
	f.write(pw, f.passwords["operator"]+"\n", 0600)
	if os.Chown(pw, uid, gid) != nil {
		t.Fatal("init password owner")
	}
	f.command("initdb", nil, "-D", f.data, "--username=operator", "--pwfile="+pw, "--auth-local=scram-sha-256", "--auth-host=reject", "--encoding=UTF8", "--locale=C")
	hba := "local all all scram-sha-256\nhost source all 127.0.0.1/32 scram-sha-256\nhost all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n"
	f.write(hbaPath, hba, 0644)
	conf := fmt.Sprintf("listen_addresses='127.0.0.1'\nport=5432\nunix_socket_directories='%s'\nhba_file='%s'\n", f.socket, hbaPath)
	f.write(filepath.Join(f.data, "postgresql.auto.conf"), conf, 0600)
	if os.Chown(filepath.Join(f.data, "postgresql.auto.conf"), uid, gid) != nil {
		t.Fatal("PG config owner")
	}
	f.command("pg_ctl", nil, "-D", f.data, "-l", filepath.Join(root, "postgres.log"), "-w", "start")
	raw, err := os.ReadFile(filepath.Join(f.data, "postmaster.pid"))
	if err != nil {
		t.Fatal("capture postmaster PID")
	}
	f.postmaster, err = strconv.Atoi(strings.SplitN(string(raw), "\n", 2)[0])
	if err != nil || f.postmaster <= 0 {
		t.Fatal("invalid postmaster PID")
	}
	return f
}

func (f *liveCutoverFixture) cleanupLive() {
	t := f.t
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	safe := true
	// Stop only units whose files this fixture created (never another host's).
	for _, u := range append(append([]string{}, liveUnits...), f.managedUnits...) {
		path := filepath.Join("/etc/systemd/system", u)
		if _, ok := f.owned[path]; !ok {
			continue
		}
		if _, e := systemctl(ctx, "stop", u); e != nil {
			t.Error("cleanup unit stop failed")
			safe = false
		}
		pid, e := unitPID(ctx, u)
		if u == "nmcp.slice" || u == "nmcp-backup.timer" {
			pid = 0
			e = nil
		}
		if e != nil || pid != 0 {
			t.Error("cleanup cannot prove unit MainPID absence")
			safe = false
		}
	}
	if _, ok := f.owned["/etc/systemd/system/nmcp-backup.timer"]; ok {
		if active, _ := systemctl(ctx, "is-active", "nmcp-backup.timer"); active == "active" {
			t.Error("cleanup timer still active")
			safe = false
		}
	}
	for _, server := range append([]*liveCutoverFixture{f}, f.managedServers...) {
		if _, statErr := os.Stat(filepath.Join(server.data, "postmaster.pid")); server.data != "" && !os.IsNotExist(statErr) {
			c := server.process(ctx, "pg_ctl", "-D", server.data, "-m", "immediate", "-w", "stop")
			c.Stdout = io.Discard
			c.Stderr = io.Discard
			if e := c.Run(); e != nil && server.postmaster != 0 {
				t.Error("cleanup own PG stop failed")
				safe = false
			}
		}
		if server.data != "" {
			raw, e := os.ReadFile(filepath.Join(server.data, "postmaster.pid"))
			if e == nil {
				pid, e := strconv.Atoi(strings.SplitN(string(raw), "\n", 2)[0])
				if e != nil || pid <= 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
					t.Error("cleanup cannot prove postmaster.pid process absence")
					safe = false
				}
			} else if !os.IsNotExist(e) {
				t.Error("cleanup cannot inspect owned postmaster.pid")
				safe = false
			}
		}
		if server.postmaster > 0 && syscall.Kill(server.postmaster, 0) != syscall.ESRCH {
			t.Error("cleanup owned postmaster still exists")
			safe = false
		}
	}
	if !safe {
		t.Error("preserving fixture paths because process absence is unproven")
		return
	}
	// Deepest first; descendants recorded separately are removed before parents.
	var paths []string
	for p := range f.owned {
		paths = append(paths, p)
	}
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if len(paths[j]) > len(paths[i]) {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	for _, p := range paths {
		before := f.owned[p]
		now, e := os.Lstat(p)
		if e != nil || !os.SameFile(before, now) {
			t.Errorf("refuse cleanup identity-changed path %s", p)
			continue
		}
		// Parents created merely to support /etc/postgresql are removed only empty.
		if p == "/etc/postgresql" || p == "/etc/postgresql/17" {
			e = os.Remove(p)
		} else {
			e = os.RemoveAll(p)
		}
		if e != nil {
			t.Errorf("remove owned fixture path %s: %v", p, e)
		}
	}
	if _, e := systemctl(ctx, "daemon-reload"); e != nil {
		t.Error("cleanup daemon-reload")
	}
	t.Log("live cleanup: all owned unit MainPIDs and owned postmaster absent; only exclusively created, identity-checked paths removed")
}

func TestBackupPG17LiveCutover(t *testing.T) {
	if os.Getenv("TEST_BACKUP_LIVE_SYSTEMD") != "1" {
		t.Skip("TEST_BACKUP_LIVE_SYSTEMD=1 absent: real fixed-path systemd cutover NOT executed")
	}
	if os.Geteuid() != 0 {
		t.Skip("live cutover requires root; UID1001/same-UID simulation is not production evidence")
	}
	if runtime.GOOS != "linux" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("live gate is restricted to isolated GitHub-hosted Linux runners")
	}
	if b, e := os.ReadFile("/proc/1/comm"); e != nil || strings.TrimSpace(string(b)) != "systemd" {
		t.Fatal("real systemd PID1 required")
	}
	ids := map[string]bool{}
	for _, name := range []string{"postgres", "nmcp", "nmcp-backup"} {
		u, e := user.Lookup(name)
		if e != nil || u.Uid == "0" || ids[u.Uid] {
			t.Fatal("distinct postgres/nmcp/nmcp-backup accounts required")
		}
		ids[u.Uid] = true
	}
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if !filepath.IsAbs(bin) || filepath.Clean(bin) != bin {
		t.Fatal("TEST_BACKUP_PG17_BINDIR must be a real absolute PG17 binary prefix")
	}
	if filepath.Base(bin) != "bin" {
		t.Fatal("PG17 bindir must be actual prefix/bin for narrow sandbox bind")
	}
	for _, tool := range []string{"postgres", "pg_dump", "pg_restore", "initdb", "pg_ctl", "psql"} {
		out, e := exec.Command(filepath.Join(bin, tool), "--version").Output()
		if e != nil || !strings.Contains(string(out), "(PostgreSQL) 17.") {
			t.Fatalf("real PG17 %s required", tool)
		}
	}
	prefix := os.Getenv("TEST_BACKUP_NATIVE_PREFIX")
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix {
		t.Fatal("TEST_BACKUP_NATIVE_PREFIX absolute closure required")
	}
	st, e := os.Stat(prefix)
	if e != nil || !st.IsDir() {
		t.Fatal("native closure prefix absent")
	}
	artifacts := map[string]string{}
	for _, key := range []string{"TEST_BACKUP_STILL_HELPER", "TEST_BACKUP_ANIMATION_HELPER", "TEST_BACKUP_CORE_API", "TEST_BACKUP_CORE_WORKER", "TEST_BACKUP_ADMIN_BINARY"} {
		artifacts[key] = liveRequired(t, key, true)
	}
	mediaFixture := liveRequired(t, "TEST_BACKUP_MEDIA_FIXTURE", false)
	mediaBytes, e := os.ReadFile(mediaFixture)
	if e != nil {
		t.Fatal("read known native media fixture")
	}
	mediaHash := sha256.Sum256(mediaBytes)
	if hex.EncodeToString(mediaHash[:]) != "9b344e9f0c869d8637ea22e672df9451d8d3cc1d2d0b291af3b284e538e5f124" {
		t.Fatal("known real orientation JPEG digest mismatch")
	}
	icc := liveRequired(t, "TEST_BACKUP_ICC_PATH", false)
	iccBytes, e := os.ReadFile(icc)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(iccBytes)
	if hex.EncodeToString(digest[:]) != config.RequiredSRGBICCSHA256 {
		t.Fatal("actual pinned ICC digest mismatch")
	}
	video := filepath.Join(prefix, "bin/nmcp-video-helper")
	// Validate the video ELF with the same non-script rule without inventing an
	// extra public environment variable in the formal-job API.
	t.Setenv("NMCP_LIVE_VIDEO_ARTIFACT", video)
	liveRequired(t, "NMCP_LIVE_VIDEO_ARTIFACT", true)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			if os.Unsetenv(key) != nil {
				t.Fatal("clear ambient libpq override")
			}
		}
	}
	f := newLiveCutoverFixture(t, bin)
	ctx := f.ctx
	admin := f.poolLive("operator", "postgres")
	f.sql(admin, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrapLive(false)
	for _, role := range []string{"nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		f.sql(admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+f.passwords[role]+"'")
	}
	migrator := f.poolLive("nmcp_migrator", "source")
	migration, e := database.NewMigrator(migrator)
	if e != nil || migration.Up(ctx) != nil {
		t.Fatal("real source production migrations failed")
	}
	f.sql(migrator, `CREATE SCHEMA proof AUTHORIZATION nmcp_migrator; CREATE SEQUENCE proof.serial; CREATE TABLE proof.payload(id bigint PRIMARY KEY DEFAULT nextval('proof.serial'),body text NOT NULL); INSERT INTO proof.payload(body) VALUES ('live PG17 source')`)
	f.bootstrapLive(true)
	source := f.poolLive("operator", "source")
	f.sql(admin, `CREATE DATABASE candidate TEMPLATE template0 OWNER nmcp_migrator`)
	f.sql(admin, `REVOKE ALL ON DATABASE candidate FROM PUBLIC; GRANT CONNECT ON DATABASE candidate TO nmcp_api`)
	// Candidate root-only peer gate. Source remains usable by actual core units.
	f.write(filepath.Join(f.data, "pg_ident.conf"), "live_operator root operator\nlive_operator root nmcp_api\n", 0600)
	pg, _ := user.Lookup("postgres")
	uid, _ := strconv.Atoi(pg.Uid)
	gid, _ := strconv.Atoi(pg.Gid)
	if os.Chown(filepath.Join(f.data, "pg_ident.conf"), uid, gid) != nil {
		t.Fatal("peer map owner")
	}
	hba := "local candidate operator peer map=live_operator\nlocal candidate nmcp_api peer map=live_operator\nlocal candidate all reject\nhost candidate all 0.0.0.0/0 reject\nhost candidate all ::/0 reject\nlocal all all scram-sha-256\nhost source all 127.0.0.1/32 scram-sha-256\nhost all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n"
	f.write("/etc/postgresql/17/backup-ci/pg_hba.conf", hba, 0644)
	f.command("pg_ctl", nil, "-D", f.data, "reload")
	peer := fmt.Sprintf("host=%s port=5432 dbname=candidate user=operator sslmode=disable", quoteConnValue(f.socket))
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, e := pgx.Connect(ctx, peer)
		if e == nil {
			c.Close(ctx)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root peer gate did not load")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Every routine OS identity is denied every login over local and TCP routes.
	// Require PostgreSQL causal diagnostics, not exec/permission/network failures.
	for _, osRole := range []string{"nmcp", "nmcp-backup"} {
		for _, role := range []string{"operator", "nmcp_api", "nmcp_worker", "nmcp_migrator", "nmcp_backup_control", "nmcp_backup_dump"} {
			for _, host := range []string{f.socket, "127.0.0.1"} {
				args := []string{"--reuid=" + osRole, "--regid=" + osRole, "--init-groups", filepath.Join(bin, "psql"), "-X", "--no-password", "--host=" + host, "--port=5432", "--dbname=candidate", "--username=" + role, "--command=SELECT 1"}
				c := exec.CommandContext(ctx, "/usr/bin/setpriv", args...)
				c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
				var diagnostic bytes.Buffer
				c.Stderr = &diagnostic
				c.Stdout = io.Discard
				if c.Run() == nil || (!strings.Contains(diagnostic.String(), "pg_hba.conf rejects connection") && !strings.Contains(diagnostic.String(), "Peer authentication failed")) {
					t.Fatal("routine UID denial lacks real HBA/peer causality")
				}
			}
		}
	}
	// Root may use operator/api only via the private peer socket, never TCP.
	for _, role := range []string{"operator", "nmcp_api"} {
		c, e := pgx.Connect(ctx, strings.Replace(peer, "user=operator", "user="+role, 1))
		if e != nil {
			t.Fatal("root peer login unavailable")
		}
		c.Close(ctx)
		cfg, e := pgx.ParseConfig(peer)
		if e != nil {
			t.Fatal(e)
		}
		cfg.Host = "127.0.0.1"
		cfg.User = role
		cfg.ConnectTimeout = time.Second
		c, e = pgx.ConnectConfig(ctx, cfg)
		if e == nil {
			c.Close(ctx)
			t.Fatal("candidate root TCP unexpectedly allowed")
		}
	}
	f.installLiveUnits(artifacts, prefix, video, icc)
	// Real API and worker must be running on source *before* Restore; actual
	// unit files retain production sandboxing and fixed executable paths.
	f.run("/usr/bin/systemctl", "daemon-reload")
	f.run("/usr/bin/systemctl", "start", "nmcp-migrate.service", "nmcp-api.service", "nmcp-worker.service")
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		pid, e := unitPID(ctx, unit)
		if e != nil || pid <= 0 {
			t.Fatal("real core unit did not start")
		}
	}
	liveWaitHTTP(t, ctx)
	livePostMedia(t, ctx, mediaBytes, "orientation-6.jpg", "live-source-before-backup")
	deadline = time.Now().Add(90 * time.Second)
	for {
		var complete bool
		if source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE status='succeeded') AND EXISTS(SELECT 1 FROM renditions) AND NOT EXISTS(SELECT 1 FROM jobs WHERE status IN ('queued','running','failed'))`).Scan(&complete) != nil {
			t.Fatal("actual source native job query")
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual pinned native helper job did not finish before backup")
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.run("/usr/bin/systemctl", "start", "nmcp-backup.service")
	runs, e := New(source).BackupList(ctx)
	if e != nil || len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatal("actual backup UID/LoadCredential CLI did not produce successful archive")
	}
	backupID := runs[0].ID
	if pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM admin_audit WHERE command='backup_run' AND outcome='succeeded' AND $1::uuid=ANY(affected_ids)`, backupID) != "1" {
		t.Fatal("backup success audit absent")
	}
	backupUser, _ := user.Lookup("nmcp-backup")
	backupUID, _ := strconv.Atoi(backupUser.Uid)
	tree, e := os.ReadDir("/var/lib/nmcp-backup")
	if e != nil || len(tree) == 0 {
		t.Fatal("backup publication absent")
	}
	if e = filepath.Walk("/var/lib/nmcp-backup", func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		s, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(s.Uid) != backupUID {
			return fmt.Errorf("archive not produced under backup UID")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	si, e := restoreIdentity(ctx, source)
	if e != nil {
		t.Fatal("source identity")
	}
	temporary, e := pgxpool.New(ctx, peer)
	if e != nil {
		t.Fatal("candidate pool")
	}
	ci, e := restoreIdentity(ctx, temporary)
	temporary.Close()
	if e != nil {
		t.Fatal("candidate identity")
	}
	hash := sha256.Sum256([]byte(hba))
	receipt := RestoreReceipt{Source: si, Candidate: ci, Template0: true, Empty: true, RoutineLoginDenied: true, OperatorSocketOnly: true, HBASHA256: hex.EncodeToString(hash[:]), IssuedAt: time.Now()}
	raw, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal(e)
	}
	f.write("/var/lib/nmcp-restore/receipt.json", string(raw), 0600)
	services := fmt.Sprintf("[restore_operator]\nhost=%s\nport=5432\ndbname=candidate\nuser=operator\nsslmode=disable\n[restore_api]\nhost=%s\nport=5432\ndbname=candidate\nuser=nmcp_api\nsslmode=disable\n", f.socket, f.socket)
	f.write("/var/lib/nmcp-restore/services.conf", services, 0600)
	f.write("/var/lib/nmcp-restore/peer.pgpass", "# root-only peer, no reusable candidate credential\n", 0600)
	settings := RestoreSettings{Root: "/var/lib/nmcp-backup", LedgerRoot: "/var/lib/nmcp-restore", ReceiptFile: "/var/lib/nmcp-restore/receipt.json", Service: "restore_operator", APIService: "restore_api", ServiceFile: "/var/lib/nmcp-restore/services.conf", PassFile: "/var/lib/nmcp-restore/peer.pgpass", StorageRoot: "/var/lib/nmcp/media", FileBaseURL: "https://files.example.test/files/", RestoreTool: filepath.Join(bin, "pg_restore")}
	op, e := New(source).Restore(ctx, backupID, settings)
	if e != nil || op.Phase != "validated" {
		t.Fatalf("actual Service.Restore failed at sanitized phase %s", op.FailedPhase)
	}
	candidate, e := pgxpool.New(ctx, peer)
	if e != nil {
		t.Fatal("restored candidate pool")
	}
	defer candidate.Close()
	for _, query := range pg17CatalogProofQueries {
		if pg17Scalar(t, ctx, source, query) != pg17Scalar(t, ctx, candidate, query) {
			t.Fatal("actual Restore multiowner/ACL/data equality failed")
		}
	}
	op, e = New(source).RestoreCutover(ctx, op.ID, settings)
	if e != nil || op.Phase != "cutover_verified" {
		t.Fatalf("real system cutover failed at sanitized phase %s", op.FailedPhase)
	}
	durable, e := LoadRestoreOperation(settings.LedgerRoot, op.ID)
	if e != nil || durable.Phase != "cutover_verified" || durable.Receipt != receipt || durable.BackupID != backupID || len(durable.DumpSHA256) != 64 {
		t.Fatal("external authorization ledger lost pinned identity/receipt/digest")
	}
	pinned, e := os.ReadFile(filepath.Join(settings.LedgerRoot, op.ID+".cutover-hba-sha256"))
	hbaNow, e2 := os.ReadFile("/etc/postgresql/17/backup-ci/pg_hba.conf")
	h := sha256.Sum256(hbaNow)
	if e != nil || e2 != nil || strings.TrimSpace(string(pinned)) != hex.EncodeToString(h[:]) {
		t.Fatal("cutover HBA digest not durably pinned")
	}
	for _, role := range []string{"nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		for _, host := range []string{f.socket, "127.0.0.1"} {
			cfg, e := pgx.ParseConfig(f.dsnLive(role, "source"))
			if e != nil {
				t.Fatal(e)
			}
			cfg.Host = host
			cfg.ConnectTimeout = time.Second
			c, e := pgx.ConnectConfig(ctx, cfg)
			if e == nil {
				c.Close(ctx)
				t.Fatal("old source routine login escaped permanent HBA fence")
			}
			var denied *pgconn.PgError
			if !errors.As(e, &denied) || denied.Code != "28000" || !strings.Contains(denied.Message, "pg_hba.conf rejects connection") {
				t.Fatal("source denial was not actual PostgreSQL HBA rejection")
			}
		}
	}
	for _, p := range []*pgxpool.Pool{source, candidate} {
		state, e := New(p).MaintenanceStatus(ctx)
		if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
			t.Fatal("cutover escaped maintenance fence")
		}
	}
	if active, _ := systemctl(ctx, "is-active", "nmcp-backup.timer"); active == "active" {
		t.Fatal("cutover started backup timer before explicit resume")
	}
	// Production VerifyLive already performs actual routes + MainPID/kernel TCP
	// socket to pg_stat_activity correlation for *every* core process pool.
	var mediaID string
	if candidate.QueryRow(ctx, `SELECT id::text FROM media ORDER BY id LIMIT 1`).Scan(&mediaID) != nil {
		t.Fatal("restored positive media row missing")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8080/media/"+mediaID, nil)
	if e != nil {
		t.Fatal(e)
	}
	resp, e := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if e != nil {
		t.Fatal("live restored media detail failed")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("live restored media detail non-200")
	}
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM renditions`) == "0" {
		t.Fatal("restored positive rendition rows missing")
	}
	t.Setenv("NMCP_RESTORE_LEDGER_ROOT", settings.LedgerRoot)
	store, e := storage.Open(settings.StorageRoot, storage.Options{})
	if e != nil {
		t.Fatal("open real media store")
	}
	defer store.Close()
	if _, e := New(source).MaintenanceResume(ctx, store); e == nil {
		t.Fatal("source permanent maintenance fence could be resumed")
	}
	if _, e := New(candidate).MaintenanceResume(ctx, store); e != nil {
		t.Fatal("separate actual candidate MaintenanceResume failed")
	}
	liveUploadAfterResume(t, ctx)
	// Stabilize the positive fixture before its next DB-only snapshot. This does
	// not waive production reconciliation when files change after a backup.
	for {
		var pending int
		if candidate.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status IN ('queued','running')`).Scan(&pending) != nil {
			t.Fatal("postresume job completion observation")
		}
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("actual resumed native job did not finish")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM media`) != "2" || pg17Scalar(t, ctx, source, `SELECT count(*)::text FROM media`) != "1" {
		t.Fatal("resumed real API write did not exclusively mutate candidate")
	}
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM admin_audit WHERE command='maintenance_resume' AND outcome='succeeded'`) != "1" {
		t.Fatal("resume audit missing")
	}
	// Snapshot contains the originating run as running, correctly converted to
	// failed by Restore. A real first candidate backup is therefore due, not a
	// false timer wake. Establish it with the actual production unit first.
	f.run("/usr/bin/systemctl", "start", "nmcp-backup.service")
	postRuns, e := New(candidate).BackupList(ctx)
	if e != nil || len(postRuns) < 2 || postRuns[0].Status != "succeeded" {
		t.Fatal("first real resumed candidate backup did not succeed")
	}
	before := pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM backup_runs`)
	// Enabling happens ONLY after explicit resume plus successful real write.
	if _, e := os.Lstat("/etc/systemd/system/timers.target.wants"); os.IsNotExist(e) {
		f.ownDir("/etc/systemd/system/timers.target.wants", 0755, "")
	} else if e != nil {
		t.Fatal("inspect timer enable directory")
	}
	f.run("/usr/bin/systemctl", "enable", "--now", "nmcp-backup.timer")
	for _, path := range []string{"/etc/systemd/system/timers.target.wants/nmcp-backup.timer", "/var/lib/systemd/timers/stamp-nmcp-backup.timer"} {
		st, e := os.Lstat(path)
		if e != nil {
			t.Fatal("record newly created timer state")
		}
		f.owned[path] = st
	}
	if enabled, e := systemctl(ctx, "is-enabled", "nmcp-backup.timer"); e != nil || enabled != "enabled" {
		t.Fatal("timer enable not persistent")
	}
	f.run("/usr/bin/systemctl", "start", "nmcp-backup.service")
	if active, e := systemctl(ctx, "is-active", "nmcp-backup.timer"); e != nil || active != "active" {
		t.Fatal("timer not enabled after successful resume")
	}
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM backup_runs`) != before {
		t.Fatal("timer wake/if-due created a false backup")
	}
	t.Logf("actual PG17 live cutover+separate resume+real API write passed; backup=%s operation=%s rpo_ms=%d rto_ms=%d", backupID, op.ID, op.RPO.Milliseconds(), op.RTO.Milliseconds())
}

func (f *liveCutoverFixture) installLiveUnits(artifacts map[string]string, prefix, video, icc string) {
	f.t.Helper()
	for _, p := range []string{"/opt/nmcp/current", "/opt/nmcp/current/bin"} {
		if os.Mkdir(p, 0755) != nil {
			f.t.Fatal("create fixed product prefix")
		}
	}
	copies := map[string]string{"core-api": artifacts["TEST_BACKUP_CORE_API"], "core-worker": artifacts["TEST_BACKUP_CORE_WORKER"], "nmcp-admin": artifacts["TEST_BACKUP_ADMIN_BINARY"], "nmcp-still-helper": artifacts["TEST_BACKUP_STILL_HELPER"], "nmcp-animation-helper": artifacts["TEST_BACKUP_ANIMATION_HELPER"]}
	for name, src := range copies {
		f.run("/usr/bin/install", "-m", "0755", src, "/opt/nmcp/current/bin/"+name)
	}
	f.run("/usr/bin/install", "-m", "0644", icc, "/opt/nmcp/current/sRGB.icc")
	common := "NMCP_STORAGE_ROOT=/var/lib/nmcp/media\nNMCP_FILE_BASE_URL=https://files.example.test/files/\n"
	for _, item := range []struct{ file, role string }{{"api.env", "nmcp_api"}, {"worker.env", "nmcp_worker"}, {"admin.env", "nmcp_migrator"}} {
		u := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/source", User: url.UserPassword(item.role, f.passwords[item.role]), RawQuery: "sslmode=disable"}
		env := "NMCP_DATABASE_URL=" + u.String() + "\n" + common
		if item.file == "api.env" {
			env += "NMCP_API_ADDR=127.0.0.1:8080\nNMCP_CURSOR_HMAC_KEY=live-cutover-isolated-fixture-key-32bytes\n"
		}
		if item.file == "worker.env" {
			env += "NMCP_STILL_HELPER_PATH=/opt/nmcp/current/bin/nmcp-still-helper\nNMCP_ANIMATION_HELPER_PATH=/opt/nmcp/current/bin/nmcp-animation-helper\nNMCP_VIDEO_HELPER_PATH=" + video + "\nNMCP_PRLIMIT_PATH=/usr/bin/prlimit\nNMCP_SRGB_ICC_PATH=/opt/nmcp/current/sRGB.icc\nNMCP_SRGB_ICC_SHA256=" + config.RequiredSRGBICCSHA256 + "\nLD_LIBRARY_PATH=" + prefix + "/lib:" + prefix + "/lib64\n"
		}
		f.write("/etc/nmcp/"+item.file, env, 0600)
	}
	f.write("/etc/nmcp/backup.env", "NMCP_DATABASE_URL=service=nmcp_backup_control\nNMCP_BACKUP_DUMP_SERVICE=nmcp_backup_dump\nNMCP_BACKUP_ROOT=/var/lib/nmcp-backup\nNMCP_BACKUP_PG_DUMP="+filepath.Join(f.bin, "pg_dump")+"\nNMCP_BACKUP_PG_RESTORE="+filepath.Join(f.bin, "pg_restore")+"\n", 0600)
	var services strings.Builder
	for _, role := range []string{"nmcp_backup_control", "nmcp_backup_dump"} {
		fmt.Fprintf(&services, "[%s]\nhost=%s\nport=5432\ndbname=source\nuser=%s\nsslmode=disable\n", role, f.socket, role)
		leaf := strings.TrimPrefix(role, "nmcp_backup_")
		f.write("/etc/nmcp/credentials/backup-"+leaf+".pgpass", fmt.Sprintf("localhost:5432:source:%s:%s\n%s:5432:source:%s:%s\n", role, f.passwords[role], f.socket, role, f.passwords[role]), 0600)
	}
	f.write("/etc/nmcp/backup.pg_service.conf", services.String(), 0600)
	repo := pg17Repository(f.t)
	for _, unit := range liveUnits {
		var body []byte
		if unit == "nmcp-migrate.service" {
			// The database above was really migrated by nmcp_migrator. This explicit
			// dependency fixture prevents restarting production migrations against the
			// old admin.env; it is not a fake product/migration acceptance claim.
			body = []byte("[Unit]\nDescription=Live test dependency fixture: database already really migrated\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/true\n")
		} else {
			var e error
			body, e = os.ReadFile(filepath.Join(repo, "infra/ubuntu/systemd", unit))
			if e != nil {
				f.t.Fatal("read exact production unit")
			}
		}
		path := filepath.Join("/etc/systemd/system", unit)
		file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if e != nil {
			f.t.Fatal("exclusive unit install")
		}
		if _, e = file.Write(body); e != nil {
			file.Close()
			f.t.Fatal(e)
		}
		if e = file.Close(); e != nil {
			f.t.Fatal(e)
		}
		st, e := os.Lstat(path)
		if e != nil {
			f.t.Fatal(e)
		}
		f.owned[path] = st
	}
	// Preserve all production hardening. Narrow read-only mounts expose only
	// trusted artifact prefixes needed by embedded native rpaths under /home.
	// No sandbox directive is reset or weakened. Unit source bytes remain exact.
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service", "nmcp-backup.service"} {
		dir := "/etc/systemd/system/" + unit + ".d"
		f.ownDir(dir, 0755, "")
		for _, p := range []string{prefix, f.bin} {
			if strings.ContainsAny(p, " \n\t:%") {
				f.t.Fatal("native/PG prefix cannot contain systemd metacharacters")
			}
		}
		f.write(dir+"/artifact-paths.conf", "[Service]\nBindReadOnlyPaths="+prefix+" "+filepath.Dir(f.bin)+"\n", 0644)
	}
}

func liveWaitHTTP(t *testing.T, ctx context.Context) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8080/readyz", nil)
		r, e := client.Do(req)
		if e == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("actual source API never ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func livePostMedia(t *testing.T, ctx context.Context, data []byte, name, key string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, e := w.CreateFormFile("file", name)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = part.Write(data); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	req, e := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8080/media", &b)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Accept", "application/json")
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, e := c.Do(req)
	if e != nil {
		t.Fatal("actual media API write failed")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("actual media API write status=%d", resp.StatusCode)
	}
}
func liveUploadAfterResume(t *testing.T, ctx context.Context) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 91, A: 255})
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatal(e)
	}
	livePostMedia(t, ctx, b.Bytes(), "live-resumed.png", "live-cutover-explicit-resume")
}
