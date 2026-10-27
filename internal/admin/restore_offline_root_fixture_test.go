package admin

// Self-provisioned destructive acceptance, restricted to a fresh hosted root VM.
// No local UID1001 execution constitutes root or systemd acceptance evidence.
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
	"net/http"
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

	"github.com/jackc/pgservicefile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/backupartifact"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestRestoreOfflineRootTwoClusters(t *testing.T) {
	if os.Getenv("TEST_BACKUP_OFFLINE_SYSTEMD") != "1" {
		t.Skip("TEST_BACKUP_OFFLINE_SYSTEMD=1 absent: real fixed-path systemd cutover NOT executed")
	}
	if os.Geteuid() != 0 {
		t.Skip("offline two-cluster root positive SKIP: UID1001/same-UID simulation is not production evidence")
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
	if bin != "/opt/nmcp-pg17-ci/bin" {
		t.Fatal("offline fixture requires canonical actual PG17 artifact prefix")
	}
	offlineFixedPreflight(t)
	f := newLiveCutoverFixture(t, bin, "nmcp-offline-fixture")
	f.manageOfflinePG("postgresql@17-nmcp-offline-fixture.service")
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

	g, peer, candidateHBA := f.newOfflineCandidate(source)
	candidate, e := pgxpool.New(ctx, peer)
	if e != nil {
		t.Fatal("candidate pool")
	}
	defer candidate.Close()
	graph, e := restoreRoleGraph(ctx, source)
	if e != nil {
		t.Fatal("capture source role graph")
	}
	if actual, err := restoreRoleGraph(ctx, candidate); err != nil || actual != graph {
		t.Fatal("preprovisioned candidate global role graph mismatch")
	}
	offlineCandidateRoleProof(t, g, peer)
	f.installLiveUnits(artifacts, prefix, video, icc)
	// Real API and worker must be running on source *before* Restore; actual
	// unit files retain production sandboxing and fixed executable paths.
	f.run("/usr/bin/systemctl", "daemon-reload")
	f.run("/usr/bin/systemctl", "start", "nmcp-migrate.service", "nmcp-api.service", "nmcp-worker.service")
	oldPIDs := map[string]int{}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		pid, e := unitPID(ctx, unit)
		oldPIDs[unit] = pid
		if e != nil || pid <= 0 {
			t.Fatal("real core unit did not start")
		}
	}
	liveWaitHTTP(t, ctx)
	livePostMedia(t, ctx, mediaBytes, "orientation-6.jpg", "live-source-before-backup")
	deadline := time.Now().Add(90 * time.Second)
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
	ci, e := restoreIdentity(ctx, candidate)
	if e != nil || ci.ClusterID == si.ClusterID {
		t.Fatal("candidate must be a second actual cluster")
	}
	hash := sha256.Sum256([]byte(candidateHBA))
	receipt := RestoreReceipt{Source: si, Candidate: ci, Template0: true, Empty: true, RoutineLoginDenied: true, OperatorSocketOnly: true, HBASHA256: hex.EncodeToString(hash[:]), IssuedAt: time.Now()}
	raw, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal(e)
	}
	f.write("/var/lib/nmcp-restore/receipt.json", string(raw), 0600)
	services := fmt.Sprintf("[restore_operator]\nhost=%s\nport=5544\ndbname=candidate\nuser=operator\nsslmode=disable\n[restore_api]\nhost=%s\nport=5544\ndbname=candidate\nuser=nmcp_api\nsslmode=disable\n", g.socket, g.socket)
	f.write("/var/lib/nmcp-restore/services.conf", services, 0600)
	f.write("/var/lib/nmcp-restore/peer.pgpass", "# root-only peer, no reusable candidate credential\n", 0600)
	settings := RestoreSettings{Root: "/var/lib/nmcp-backup", LedgerRoot: "/var/lib/nmcp-restore", ReceiptFile: "/var/lib/nmcp-restore/receipt.json", Service: "restore_operator", APIService: "restore_api", ServiceFile: "/var/lib/nmcp-restore/services.conf", PassFile: "/var/lib/nmcp-restore/peer.pgpass", StorageRoot: "/var/lib/nmcp/media", FileBaseURL: "https://files.example.test/files/", RestoreTool: filepath.Join(bin, "pg_restore")}

	// Capture committed FULL manifest and all source proofs before product stops it.
	artifactOwner, _ := strconv.ParseUint(backupUser.Uid, 10, 32)
	owner := uint32(artifactOwner)
	archives, e := backupartifact.Open(settings.Root, backupartifact.Options{ReadOnly: true, Lock: true, OwnerUID: &owner})
	if e != nil {
		t.Fatal("open committed archive")
	}
	manifest, e := archives.Verify(backupID)
	archives.Close()
	if e != nil {
		t.Fatal("verify complete committed manifest")
	}
	sourceHBA, e := os.ReadFile(f.hbaPath)
	if e != nil {
		t.Fatal(e)
	}
	sourceHash := sha256.Sum256(sourceHBA)
	authority := OfflineRestoreAuthority{SourceBackupSucceeded: true, Source: si, Candidate: ci, Manifest: manifest, RoleGraphSHA256: graph,
		SourceHBAPath: f.hbaPath, SourceHBASHA256: hex.EncodeToString(sourceHash[:]), SourceUnit: "postgresql@17-nmcp-offline-fixture.service", CandidateUnit: "postgresql@17-nmcp-offline-candidate.service",
		SourceEndpoint:    OfflineRestoreEndpoint{SocketDirectory: f.socket, Port: 5432, LoopbackHosts: []string{"127.0.0.1"}},
		CandidateEndpoint: OfflineRestoreEndpoint{SocketDirectory: g.socket, Port: 5544, LoopbackHosts: []string{}}, EndpointComplete: true, IssuedAt: time.Now(), Confirmation: offlineConfirmation}
	settings.OfflineSource = true
	settings.OfflineAuthorityFile = "/var/lib/nmcp-restore/offline-authority.json"
	raw, e = json.Marshal(authority)
	if e != nil {
		t.Fatal(e)
	}
	f.write(settings.OfflineAuthorityFile, string(raw), 0600)
	// Capture exact catalog expectations without retaining live-source dependence.
	catalog := map[string]string{}
	for _, query := range pg17CatalogProofQueries {
		catalog[query] = pg17Scalar(t, ctx, source, query)
	}
	candidate.Close() // no idle bootstrap session may bypass candidate empty preflight
	initialPID := f.postmaster
	offlineRootNegativeInputs(t, settings, authority)
	if pid, err := unitPID(ctx, authority.SourceUnit); err != nil || pid != initialPID {
		t.Fatal("preflight negatives touched actual source unit")
	}
	source.Close()
	admin.Close()
	migrator.Close()
	op, e := New(nil).Restore(ctx, backupID, settings)
	if e != nil || op.Phase != "offline_validated" {
		t.Fatalf("actual offline Restore failed at sanitized phase %s", op.FailedPhase)
	}
	if syscall.Kill(initialPID, 0) != syscall.ESRCH || VerifyOfflineSourceFence(ctx, op) != nil {
		t.Fatal("actual source postmaster/fence/endpoint absence not proven")
	}
	candidate, e = pgxpool.New(ctx, peer)
	if e != nil {
		t.Fatal("reopen restored candidate")
	}
	defer candidate.Close()
	validated, e := LoadRestoreOperation(settings.LedgerRoot, op.ID)
	if e != nil || validated.Phase != "offline_validated" || !validated.SourceOffline || !sameOfflineAuthority(validated.OfflineAuthority, &authority) {
		t.Fatal("offline validated authority not durable")
	}
	for query, expected := range catalog {
		if pg17Scalar(t, ctx, candidate, query) != expected {
			t.Fatal("actual two-cluster restore catalog/data proof mismatch")
		}
	}
	op, e = New(nil).RestoreCutover(ctx, op.ID, settings)
	if e != nil || op.Phase != "cutover_verified" {
		t.Fatalf("real system cutover failed at sanitized phase %s", op.FailedPhase)
	}
	pg17AssertBackupOtherDBHBA(t, ctx, f.passwords, g.socket, 5544, op.Candidate.DatabaseName)
	durable, e := LoadRestoreOperation(settings.LedgerRoot, op.ID)
	if e != nil || durable.Phase != "cutover_verified" || durable.Receipt != receipt || durable.BackupID != backupID || len(durable.DumpSHA256) != 64 {
		t.Fatal("external authorization ledger lost pinned identity/receipt/digest")
	}

	if VerifyOfflineSourceFence(ctx, op) != nil {
		t.Fatal("offline cutover modified source fence or restarted old server")
	}
	offlineRetargetProof(t, ctx, f, candidate, op, oldPIDs)
	state, e := New(candidate).MaintenanceStatus(ctx)
	if e != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
		t.Fatal("candidate escaped cutover maintenance")
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
	if VerifyOfflineSourceFence(ctx, op) != nil {
		t.Fatal("source fence lost before separate resume")
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
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM media`) != "2" || VerifyOfflineSourceFence(ctx, op) != nil {
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
	if status, err := systemctl(ctx, "show", "--property=ExecMainStatus", "--value", "nmcp-backup.service"); err != nil || status != "4" {
		t.Fatal("real not-due timer wake did not return structured exit 4")
	}
	if pg17Scalar(t, ctx, candidate, `SELECT count(*)::text FROM backup_runs`) != before {
		t.Fatal("timer wake/if-due created a false backup")
	}
	t.Logf("actual two-cluster PG17 offline restore+cutover+separate resume+real API write passed; backup=%s operation=%s rpo_ms=%d rto_ms=%d", backupID, op.ID, op.RPO.Milliseconds(), op.RTO.Milliseconds())
	offlineActualTimeoutCleanup(t, f, g)
}

const offlineSourcePGUnit = "postgresql@17-nmcp-offline-fixture.service"
const offlineCandidatePGUnit = "postgresql@17-nmcp-offline-candidate.service"

// All additional fixed namespaces are checked BEFORE the shared constructor
// creates its root owner receipt or any directory. Existing instances are never
// stopped, overwritten, or adopted.
func offlineFixedPreflight(t *testing.T) {
	t.Helper()
	for _, path := range []string{"/etc/postgresql/17/nmcp-offline-fixture", "/etc/postgresql/17/nmcp-offline-candidate", "/run/nmcp-offline-fixture", "/run/nmcp-offline-candidate"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("refuse existing/uninspectable offline fixture path %s", path)
		}
	}
	for _, unit := range []string{offlineSourcePGUnit, offlineCandidatePGUnit} {
		out, err := exec.Command("/usr/bin/systemctl", "show", "--property=LoadState", "--value", unit).Output()
		if err != nil || strings.TrimSpace(string(out)) != "not-found" {
			t.Fatalf("refuse existing/uninspectable offline PG unit %s", unit)
		}
		for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
			for _, suffix := range []string{"", ".d"} {
				if _, err := os.Lstat(filepath.Join(dir, unit+suffix)); !os.IsNotExist(err) {
					t.Fatal("preexisting offline PG unit or drop-in")
				}
			}
		}
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:5544", time.Second); err == nil {
		c.Close()
		t.Fatal("candidate TCP port occupied")
	}
}

// Adopt ONLY this fixture's already exclusively created PG data into a real
// Type=forking unit. systemctl stop therefore stops the real tracked postmaster,
// rather than a no-op unit whose MainPID never represented PostgreSQL.
func (f *liveCutoverFixture) manageOfflinePG(unit string) {
	f.t.Helper()
	if unit != offlineSourcePGUnit && unit != offlineCandidatePGUnit {
		f.t.Fatal("unsupported fixture unit")
	}
	if f.postmaster > 0 {
		old := f.postmaster
		f.command("pg_ctl", nil, "-D", f.data, "-m", "fast", "-w", "stop")
		if syscall.Kill(old, 0) != syscall.ESRCH {
			f.t.Fatal("initial private postmaster did not stop before unit adoption")
		}
	}
	body := fmt.Sprintf("[Unit]\nDescription=Owned offline PG17 acceptance cluster\n[Service]\nType=forking\nUser=postgres\nGroup=postgres\nPIDFile=%s/postmaster.pid\nExecStart=%s/pg_ctl -D %s -l %s/postgres.log -w start\nExecStop=%s/pg_ctl -D %s -m fast -w stop\nTimeoutStartSec=60\nTimeoutStopSec=60\nKillMode=mixed\n", f.data, f.bin, f.data, f.root, f.bin, f.data)
	path := "/etc/systemd/system/" + unit
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		f.t.Fatal("exclusive real PG unit creation")
	}
	// Record identity immediately, even if the write/start later fails.
	st, err := file.Stat()
	if err != nil {
		file.Close()
		f.t.Fatal(err)
	}
	f.owned[path] = st
	if _, err = file.WriteString(body); err != nil {
		file.Close()
		f.t.Fatal(err)
	}
	if file.Sync() != nil || file.Close() != nil {
		f.t.Fatal("durable PG unit")
	}
	f.managedUnits = append(f.managedUnits, unit)
	// Persist exact unit+data identities BEFORE startup: Go's timeout does not
	// execute t.Cleanup, so the workflow must independently own both PG stops.
	dataInfo, dataErr := os.Lstat(f.data)
	if dataErr != nil {
		f.t.Fatal("owned PG data receipt")
	}
	unitStat, unitOK := st.Sys().(*syscall.Stat_t)
	dataStat, dataOK := dataInfo.Sys().(*syscall.Stat_t)
	if !unitOK || !dataOK {
		f.t.Fatal("PG receipt inode identity")
	}
	port := 5432
	if unit == offlineCandidatePGUnit {
		port = 5544
	}
	receipt := map[string]any{"head": os.Getenv("TEST_BACKUP_CANDIDATE_SHA"), "unit": unit, "device": unitStat.Dev, "inode": unitStat.Ino, "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(body))), "data": f.data, "data_device": dataStat.Dev, "data_inode": dataStat.Ino, "socket": f.socket, "port": port, "postgres": filepath.Join(f.bin, "postgres")}
	encoded, encodeErr := json.Marshal(receipt)
	if encodeErr != nil {
		f.t.Fatal("PG ownership receipt")
	}
	receiptPath := "/run/nmcp-backup-cutover." + unit + ".owner"
	receiptFile, receiptErr := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if receiptErr != nil {
		f.t.Fatal("exclusive PG owner receipt")
	}
	if _, receiptErr = receiptFile.Write(encoded); receiptErr == nil {
		receiptErr = receiptFile.Sync()
	}
	closeErr := receiptFile.Close()
	if receiptErr != nil || closeErr != nil {
		f.t.Fatal("durable PG ownership receipt")
	}
	receiptInfo, receiptErr := os.Lstat(receiptPath)
	if receiptErr != nil {
		f.t.Fatal("PG receipt identity")
	}
	f.owned[receiptPath] = receiptInfo
	f.run("/usr/bin/systemctl", "daemon-reload")
	f.run("/usr/bin/systemctl", "start", unit)
	raw, err := os.ReadFile(filepath.Join(f.data, "postmaster.pid"))
	if err != nil {
		f.t.Fatal("real unit postmaster receipt")
	}
	f.postmaster, err = strconv.Atoi(strings.SplitN(string(raw), "\n", 2)[0])
	pid, pidErr := unitPID(f.ctx, unit)
	if err != nil || pidErr != nil || pid <= 0 || pid != f.postmaster {
		f.t.Fatal("systemd MainPID is not actual PostgreSQL postmaster")
	}
}

func (f *liveCutoverFixture) newOfflineCandidate(source *pgxpool.Pool) (*liveCutoverFixture, string, string) {
	t := f.t
	g := &liveCutoverFixture{pg17Fixture: &pg17Fixture{t: t, ctx: f.ctx, bin: f.bin, passwords: f.passwords, passfiles: map[string]string{}}, owned: f.owned, hbaPath: "/etc/postgresql/17/nmcp-offline-candidate/pg_hba.conf"}
	g.ownDir(filepath.Dir(g.hbaPath), 0755, "")
	g.socket = "/run/nmcp-offline-candidate"
	g.ownDir(g.socket, 0755, "postgres")
	root, err := os.MkdirTemp("/tmp", "nmcp-offline-candidate-pg17-")
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	f.owned[root] = st
	g.root = root
	g.data = filepath.Join(root, "data")
	// Register before initdb/start. Shared cleanup stops both actual PG units,
	// then proves both postmaster processes absent before ANY owned removal.
	f.managedServers = append(f.managedServers, g)
	f.managedUnits = append(f.managedUnits, offlineCandidatePGUnit)
	pg, err := user.Lookup("postgres")
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(pg.Uid)
	gid, _ := strconv.Atoi(pg.Gid)
	if os.Chown(root, uid, gid) != nil || os.Chmod(root, 0755) != nil {
		t.Fatal("candidate PG root ownership")
	}
	pw := filepath.Join(root, "init.password")
	g.write(pw, g.passwords["operator"]+"\n", 0600)
	if os.Chown(pw, uid, gid) != nil {
		t.Fatal("candidate init password owner")
	}
	g.command("initdb", nil, "-D", g.data, "--username=operator", "--pwfile="+pw, "--auth-local=scram-sha-256", "--auth-host=reject", "--encoding=UTF8", "--locale=C")
	// Bootstrap global roles via a root peer operator; no target DB migrations,
	// schemas, or application objects are provisioned before the actual restore.
	g.write(filepath.Join(g.data, "pg_ident.conf"), "offline_operator root operator\noffline_operator root nmcp_api\n", 0600)
	if os.Chown(filepath.Join(g.data, "pg_ident.conf"), uid, gid) != nil {
		t.Fatal("candidate ident owner")
	}
	hba := "local all operator peer map=offline_operator\nlocal candidate nmcp_api peer map=offline_operator\nlocal all all reject\nhost all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n"
	g.write(g.hbaPath, hba, 0644)
	conf := fmt.Sprintf("listen_addresses='127.0.0.1'\nport=5544\nunix_socket_directories='%s'\nhba_file='%s'\n", g.socket, g.hbaPath)
	g.write(filepath.Join(g.data, "postgresql.auto.conf"), conf, 0600)
	if os.Chown(filepath.Join(g.data, "postgresql.auto.conf"), uid, gid) != nil {
		t.Fatal("candidate config owner")
	}
	g.manageOfflinePG(offlineCandidatePGUnit)
	// Shared cleanup owns both unit files; g's standalone cleanup is not used.
	peer := fmt.Sprintf("host=%s port=5544 dbname=postgres user=operator sslmode=disable", quoteConnValue(g.socket))
	admin, err := pgxpool.New(g.ctx, peer)
	if err != nil {
		t.Fatal("candidate bootstrap pool")
	}
	defer admin.Close()
	rows, err := source.Query(g.ctx, `SELECT format('CREATE ROLE %I %s %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT %s',rolname,CASE WHEN rolcanlogin THEN 'LOGIN' ELSE 'NOLOGIN' END,CASE WHEN rolinherit THEN 'INHERIT' ELSE 'NOINHERIT' END,rolconnlimit) FROM pg_roles WHERE starts_with(rolname,'nmcp_') ORDER BY rolname`)
	if err != nil {
		t.Fatal("source global role inventory")
	}
	var ddl []string
	for rows.Next() {
		var q string
		if rows.Scan(&q) != nil {
			t.Fatal("role inventory scan")
		}
		ddl = append(ddl, q)
	}
	if rows.Err() != nil {
		t.Fatal("role inventory read")
	}
	rows.Close()
	for _, q := range ddl {
		g.sql(admin, q)
	}
	for role, password := range f.passwords {
		if role != "operator" {
			g.sql(admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+password+"'")
		}
	}
	rows, err = source.Query(g.ctx, `SELECT format('GRANT %I TO %I WITH ADMIN %s, INHERIT %s, SET %s GRANTED BY %I',r.rolname,m.rolname,a.admin_option,a.inherit_option,a.set_option,g.rolname) FROM pg_auth_members a JOIN pg_roles r ON r.oid=a.roleid JOIN pg_roles m ON m.oid=a.member JOIN pg_roles g ON g.oid=a.grantor WHERE starts_with(r.rolname,'nmcp_') OR starts_with(m.rolname,'nmcp_') ORDER BY r.rolname,m.rolname,g.rolname`)
	if err != nil {
		t.Fatal("source membership inventory")
	}
	ddl = nil
	for rows.Next() {
		var q string
		if rows.Scan(&q) != nil {
			t.Fatal("membership scan")
		}
		ddl = append(ddl, q)
	}
	if rows.Err() != nil {
		t.Fatal("membership inventory read")
	}
	rows.Close()
	for _, q := range ddl {
		g.sql(admin, q)
	}
	rows, err = source.Query(g.ctx, `SELECT format('ALTER ROLE %I SET %I TO %L',rolname,split_part(setting,'=',1),substring(setting from position('=' in setting)+1)) FROM pg_roles,LATERAL unnest(rolconfig) setting WHERE starts_with(rolname,'nmcp_')`)
	if err != nil {
		t.Fatal("source role settings")
	}
	ddl = nil
	for rows.Next() {
		var q string
		if rows.Scan(&q) != nil {
			t.Fatal("role settings scan")
		}
		ddl = append(ddl, q)
	}
	if rows.Err() != nil {
		t.Fatal("role settings read")
	}
	rows.Close()
	for _, q := range ddl {
		g.sql(admin, q)
	}
	g.sql(admin, `CREATE DATABASE candidate TEMPLATE template0 OWNER nmcp_migrator`)
	g.sql(admin, `REVOKE ALL ON DATABASE candidate FROM PUBLIC; GRANT CONNECT ON DATABASE candidate TO nmcp_api`)
	return g, strings.Replace(peer, "dbname=postgres", "dbname=candidate", 1), hba
}

// Proof levels: real root peer success (operator + API); real PG causal denial
// for every routine OS UID/managed SQL login over BOTH socket and TCP; root TCP
// denied; exact source/candidate closed global role graph equality above.
func offlineCandidateRoleProof(t *testing.T, g *liveCutoverFixture, peer string) {
	t.Helper()
	for _, osRole := range []string{"nmcp", "nmcp-backup", "postgres"} {
		for _, role := range []string{"operator", "nmcp_api", "nmcp_worker", "nmcp_migrator", "nmcp_backup_control", "nmcp_backup_dump"} {
			for _, host := range []string{g.socket, "127.0.0.1"} {
				c := exec.CommandContext(g.ctx, "/usr/bin/setpriv", "--reuid="+osRole, "--regid="+osRole, "--init-groups", filepath.Join(g.bin, "psql"), "-X", "--no-password", "--host="+host, "--port=5544", "--dbname=candidate", "--username="+role, "--command=SELECT 1")
				c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
				var diagnostic bytes.Buffer
				c.Stderr = &diagnostic
				c.Stdout = io.Discard
				if c.Run() == nil || (!strings.Contains(diagnostic.String(), "pg_hba.conf rejects connection") && !strings.Contains(diagnostic.String(), "Peer authentication failed")) {
					t.Fatal("candidate routine UID denial lacks PostgreSQL HBA/peer causality")
				}
			}
		}
	}
	for _, role := range []string{"operator", "nmcp_api"} {
		cfg, err := pgx.ParseConfig(strings.Replace(peer, "user=operator", "user="+role, 1))
		if err != nil {
			t.Fatal(err)
		}
		c, err := pgx.ConnectConfig(g.ctx, cfg)
		if err != nil {
			t.Fatal("root peer positive proof failed")
		}
		c.Close(g.ctx)
		cfg.Host = "127.0.0.1"
		cfg.ConnectTimeout = time.Second
		c, err = pgx.ConnectConfig(g.ctx, cfg)
		if err == nil {
			c.Close(g.ctx)
			t.Fatal("root candidate TCP route allowed")
		}
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "28000" || !strings.Contains(denied.Message, "pg_hba.conf rejects connection") {
			t.Fatal("root TCP denial lacks real HBA cause")
		}
	}
	t.Log("candidate authority: real root peer operator/API positive; nmcp,nmcp-backup,postgres UID x six logins x socket/TCP PG-causal negative; root TCP negative; closed global graph exact-match")
}

func offlineRetargetProof(t *testing.T, ctx context.Context, f *liveCutoverFixture, candidate *pgxpool.Pool, op RestoreOperation, oldPIDs map[string]int) {
	target := op.Candidate
	t.Helper()
	// Product VerifyLive supplies MainPID -> /proc kernel TCP inode -> target
	// backend identity correlations. Independently prove all five config entries.
	for _, item := range []struct{ file, role string }{{"api.env", "nmcp_api"}, {"worker.env", "nmcp_worker"}, {"admin.env", "nmcp_migrator"}} {
		file, err := safeCutoverFile("/etc/nmcp/"+item.file, false)
		if err != nil {
			t.Fatal("read retargeted runtime config")
		}
		var dsn string
		for _, line := range strings.Split(string(file.before), "\n") {
			if strings.HasPrefix(line, "NMCP_DATABASE_URL=") {
				dsn = strings.TrimPrefix(line, "NMCP_DATABASE_URL=")
			}
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil || cfg.Host != "127.0.0.1" || cfg.Port != 5544 || cfg.Database != target.DatabaseName || cfg.User != item.role || cfg.Password != f.passwords[item.role] {
			t.Fatal("runtime pool not retargeted with retained credential")
		}
	}
	services, err := os.ReadFile("/etc/nmcp/backup.pg_service.conf")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgservicefile.ParseServicefile(strings.NewReader(string(services)))
	if err != nil {
		t.Fatal("parse actual retargeted backup services")
	}
	for _, role := range []string{"nmcp_backup_control", "nmcp_backup_dump"} {
		service, err := parsed.GetService(role)
		if err != nil {
			t.Fatal("missing backup service")
		}
		settings := service.Settings
		if settings["port"] != "5544" || settings["dbname"] != target.DatabaseName || settings["user"] != role || (settings["host"] != f.managedServers[0].socket && settings["host"] != "127.0.0.1") {
			t.Fatal("backup pool service not retargeted to actual target")
		}
	}
	// Repeat the actual kernel/backend proof independently, explicitly anchored
	// to target's port and live backend view, never the stopped source pool.
	verifier := &systemCutover{source: candidate, target: candidate}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		pid, err := unitPID(ctx, unit)
		if err != nil || pid <= 0 || pid == oldPIDs[unit] {
			t.Fatal("fresh actual product MainPID absent")
		}
		role, suffix := "nmcp_api", "api"
		if unit == "nmcp-worker.service" {
			role, suffix = "nmcp_worker", "worker"
		}
		if verifier.verifyProcessSockets(ctx, pid, role, cutoverApp(op.ID, suffix), target.DatabaseOID) != nil {
			t.Fatal("fresh MainPID kernel TCP socket lacks actual target backend identity")
		}
	}
}

// A real Go test timeout skips t.Cleanup. Inject that failure only AFTER the
// positive recovery proof; the old source HBA remains deny-all while its unit
// is deliberately restarted to prove the fallback stops BOTH postmasters.
func offlineActualTimeoutCleanup(t *testing.T, f, g *liveCutoverFixture) {
	t.Helper()
	unrelated := "nmcp-offline-unrelated.service"
	path := "/etc/systemd/system/" + unrelated
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		t.Fatal("exclusive unrelated cleanup negative unit")
	}
	_, err = file.WriteString("[Service]\nType=simple\nUser=nobody\nExecStart=/usr/bin/sleep 120\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal("unrelated cleanup negative unit")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f.owned[path] = info
	f.managedUnits = append(f.managedUnits, unrelated)
	f.run("/usr/bin/systemctl", "daemon-reload")
	f.run("/usr/bin/systemctl", "start", unrelated, offlineSourcePGUnit)
	otherPID, err := unitPID(f.ctx, unrelated)
	if err != nil || otherPID <= 0 {
		t.Fatal("unrelated negative PID")
	}
	pids := []int{}
	for _, unit := range []string{offlineSourcePGUnit, offlineCandidatePGUnit} {
		pid, err := unitPID(f.ctx, unit)
		if err != nil || pid <= 0 {
			t.Fatal("two real PG units before timeout")
		}
		pids = append(pids, pid)
	}
	marker := "/run/nmcp-offline-timeout-child-cleanup"
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("timeout marker preexists")
	}
	child := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestOfflineTimeoutChild$", "-test.timeout=200ms")
	child.Env = append(os.Environ(), "NMCP_OFFLINE_TIMEOUT_CHILD=1", "NMCP_OFFLINE_TIMEOUT_CLEANUP_MARKER="+marker)
	output, err := child.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("test timed out")) {
		t.Fatal("real Go timeout not observed")
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("timeout unexpectedly ran child Cleanup")
	}
	script := filepath.Join(pg17Repository(t), "scripts/cleanup-backup-root-fixture")
	f.run("/usr/bin/env", "NMCP_EXPECTED_HEAD="+os.Getenv("TEST_BACKUP_CANDIDATE_SHA"), "/usr/bin/python3", script)
	for _, pid := range pids {
		if syscall.Kill(pid, 0) != syscall.ESRCH {
			t.Fatal("owned PG PID survived timeout fallback")
		}
	}
	for _, server := range []*liveCutoverFixture{f, g} {
		if _, err := os.Lstat(filepath.Join(server.data, "postmaster.pid")); !os.IsNotExist(err) {
			t.Fatal("PG PID file survived fallback")
		}
		port := "5432"
		if server == g {
			port = "5544"
		}
		if _, err := os.Lstat(filepath.Join(server.socket, ".s.PGSQL."+port)); !os.IsNotExist(err) {
			t.Fatal("PG socket survived fallback")
		}
	}
	if current, err := unitPID(f.ctx, unrelated); err != nil || current != otherPID {
		t.Fatal("timeout cleanup touched unrelated unit")
	}
	t.Log("actual child Go timeout skipped Cleanup; exact owned fallback stopped both real PG postmasters, sockets absent, unrelated unit PID unchanged")
}
func TestOfflineTimeoutChild(t *testing.T) {
	if os.Getenv("NMCP_OFFLINE_TIMEOUT_CHILD") != "1" {
		t.Skip("controlled root timeout child not requested")
	}
	if os.Geteuid() != 0 {
		t.Fatal("timeout child requires root fixture")
	}
	marker := os.Getenv("NMCP_OFFLINE_TIMEOUT_CLEANUP_MARKER")
	if marker != "/run/nmcp-offline-timeout-child-cleanup" {
		t.Fatal("wrong timeout marker")
	}
	t.Cleanup(func() { _ = os.WriteFile(marker, []byte("unexpected cleanup"), 0600) })
	select {}
}
