package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

// #21 L3: the aggregate production budget is ONE nmcp.slice (CPUQuota=200%,
// MemoryMax=4G) shared by every service. Real RAW and AV1 transforms run
// through the real API and Worker units inside the isolated guest; pass means
// memory.peak stays below the 4 GiB limit and no OOM event occurred. Elapsed
// times are recorded, not asserted (real-hardware performance is #22).
const releaseSliceMemoryMax = int64(4) << 30

type releasePrepared struct {
	Name     string `json:"name"`
	MIMEType string `json:"mime_type"`
	File     string `json:"file"`
	SHA256   string `json:"sha256"`
}

func releaseFileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s", filepath.Base(path))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func releaseCgroupValue(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cgroup %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func releaseCgroupEvents(t *testing.T, path string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, line := range strings.Split(releaseCgroupValue(t, path), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("cgroup events %s line %q", path, line)
		}
		out[key] = n
	}
	return out
}

func releasePostMedia(t *testing.T, ctx context.Context, data []byte, name, key string) (mediaID, jobID string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8080/media", &b)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	defer resp.Body.Close()
	var out struct {
		Media struct {
			ID string `json:"id"`
		} `json:"media"`
		Job *struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(body, &out) != nil || out.Job == nil {
		// The API's error envelope carries only a code and message, never paths.
		t.Fatalf("upload %s: status=%d body=%.512s", name, resp.StatusCode, body)
	}
	return out.Media.ID, out.Job.ID
}

func TestReleaseGateResources(t *testing.T) {
	if os.Getenv("TEST_RELEASE_RESOURCES") != "1" {
		t.Skip("TEST_RELEASE_RESOURCES=1 absent: #21 nmcp.slice measurement runs only in its isolated root lane")
	}
	if os.Geteuid() != 0 || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("resource gate is restricted to the isolated root guest on GitHub-hosted runners")
	}
	if b, err := os.ReadFile("/proc/1/comm"); err != nil || strings.TrimSpace(string(b)) != "systemd" {
		t.Fatal("real systemd PID1 required")
	}
	fixtures := os.Getenv("TEST_RELEASE_FIXTURES_DIR")
	if !filepath.IsAbs(fixtures) {
		t.Fatal("TEST_RELEASE_FIXTURES_DIR must be the absolute prepared fixture directory")
	}
	raw, err := os.ReadFile(filepath.Join(fixtures, "prepared.json"))
	if err != nil {
		t.Fatal("prepared fixture manifest missing")
	}
	var prepared []releasePrepared
	if json.Unmarshal(raw, &prepared) != nil {
		t.Fatal("prepared fixture manifest invalid")
	}
	byName := map[string]releasePrepared{}
	for _, p := range prepared {
		byName[p.Name] = p
	}
	rawFixture, videoFixture := byName["raw-canon-cr3"], byName["video-mp4-generated"]
	if rawFixture.File == "" || videoFixture.File == "" {
		t.Fatal("prepared RAW (CR3) and AV1-source (MP4) fixtures required")
	}

	bin, prefix, video, icc, artifacts, _ := liveStackInputs(t)
	f := newLiveCutoverFixture(t, bin)
	ctx := f.ctx
	admin := f.poolLive("operator", "postgres")
	f.sql(admin, `CREATE DATABASE source TEMPLATE template0 OWNER operator`)
	f.bootstrapLive(false)
	for _, role := range []string{"nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_backup_control", "nmcp_backup_dump"} {
		f.sql(admin, "ALTER ROLE \""+role+"\" PASSWORD '"+f.passwords[role]+"'")
	}
	migrator := f.poolLive("nmcp_migrator", "source")
	migration, err := database.NewMigrator(migrator)
	if err != nil || migration.Up(ctx) != nil {
		t.Fatal("real production migrations failed")
	}
	f.bootstrapLive(true)
	// The media root is a sized tmpfs owned by the runtime user so the real
	// units can later be driven into ENOSPC without touching any host disk.
	runtimeUser, _ := user.Lookup("nmcp")
	f.run("/usr/bin/mount", "-t", "tmpfs", "-o", "size=2g,mode=0700,uid="+runtimeUser.Uid+",gid="+runtimeUser.Gid+",nosuid,nodev", "nmcp-release-media", "/var/lib/nmcp/media")
	t.Cleanup(func() {
		_, _ = systemctl(context.Background(), "stop", "nmcp-api.service", "nmcp-worker.service")
		_ = exec.Command("/usr/bin/umount", "/var/lib/nmcp/media").Run()
	})
	f.installLiveUnits(artifacts, prefix, video, icc)
	// The API's upload metadata probe runs /usr/bin/ffprobe for video (the
	// production host and the Compose image install Ubuntu's ffmpeg). The
	// isolated guest omits that package to bound its bootstrap, so provide the
	// pinned, closure-checked ffprobe at the fixed path; without it every video
	// upload is 422 invalid_media.
	if _, err := os.Lstat("/usr/bin/ffprobe"); errors.Is(err, os.ErrNotExist) {
		f.run("/usr/bin/install", "-o", "root", "-g", "root", "-m", "0755", filepath.Join(prefix, "bin/ffprobe"), "/usr/bin/ffprobe")
	} else if err != nil {
		t.Fatal("inspect /usr/bin/ffprobe")
	}

	// A narrow standard-shaped profile for exactly the measured inputs,
	// certified on this guest's exact helpers through the production command.
	definition := profile.StandardV1()
	definition.ID, definition.Key = newID(), "resource-standard"
	definition.InputMIMETypes = []string{rawFixture.MIMEType, videoFixture.MIMEType}
	params := profile.StandardV1Parameters()
	params.Recipes = map[string]profile.Recipe{rawFixture.MIMEType: params.Recipes[rawFixture.MIMEType], videoFixture.MIMEType: params.Recipes[videoFixture.MIMEType]}
	definition.Parameters, _ = json.Marshal(params)
	service := New(migrator)
	if err := service.ProfileCreate(ctx, definition); err != nil {
		t.Fatalf("create resource profile: sqlstate=%s", fixtureSQLState(err))
	}
	ref := func(path string) CertifyFileRef { return CertifyFileRef{Path: path, SHA256: releaseFileSHA(t, path)} }
	spec, _ := json.Marshal(CertifySpec{ProfileID: definition.ID, Helpers: CertifyHelpers{
		StillHelper: ref("/opt/nmcp/current/bin/nmcp-still-helper"), AnimationHelper: ref("/opt/nmcp/current/bin/nmcp-animation-helper"),
		VideoHelper: ref(video), Prlimit: ref("/usr/bin/prlimit"), FFProbe: ref(filepath.Join(prefix, "bin/ffprobe")), SRGBICC: ref("/opt/nmcp/current/sRGB.icc"),
	}, Fixtures: []CertifyFixture{
		{MIMEType: rawFixture.MIMEType, Path: filepath.Join(fixtures, rawFixture.File), SHA256: rawFixture.SHA256},
		{MIMEType: videoFixture.MIMEType, Path: filepath.Join(fixtures, videoFixture.File), SHA256: videoFixture.SHA256},
	}})
	decoded, err := DecodeCertifySpec(spec)
	if err != nil {
		t.Fatal("resource certify spec rejected")
	}
	// Root certify drops every helper and probe process to the nmcp user
	// (review D2): prove the identity a real tool process gets on this host.
	runAs, err := certifyRunAs()
	if err != nil || runAs == nil || strconv.FormatUint(uint64(runAs.UID), 10) != runtimeUser.Uid {
		t.Fatalf("root certify helper identity = %+v (%v), want nmcp uid %s", runAs, err, runtimeUser.Uid)
	}
	idRunner, err := processrunner.New("/usr/bin/prlimit", processrunner.Limits{Timeout: 10 * time.Second, AddressSpaceBytes: 1 << 30, OutputBytesPerStream: 1024, RunAs: runAs})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := idRunner.Run(ctx, processrunner.Command{Executable: "/usr/bin/id", Arguments: []string{"-u"}}); err != nil || strings.TrimSpace(string(output.Stdout)) != runtimeUser.Uid {
		t.Fatalf("certify-identity tool process ran as uid %q (%v), want nmcp %s", output.Stdout, err, runtimeUser.Uid)
	}
	// The helpers reopen /proc/self/fd/3 (input) and /fd/4 (output), which
	// re-checks the inode mode against the dropped uid: root-owned private
	// copies refuse it; the shared root:nmcp 0440/0660 copies admit it.
	reopenDir, err := os.MkdirTemp("", "nmcp-certify-reopen-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(reopenDir)
	reopenInput, reopenOutput := filepath.Join(reopenDir, "input-00"), filepath.Join(reopenDir, "output-00")
	if err := os.WriteFile(reopenInput, []byte("certify fixture copy"), 0o400); err != nil {
		t.Fatal(err)
	}
	reopenRunner, err := processrunner.New("/usr/bin/prlimit", processrunner.Limits{Timeout: 10 * time.Second, AddressSpaceBytes: 1 << 30, FileSizeBytes: 1 << 20, OutputBytesPerStream: 1024, RunAs: runAs})
	if err != nil {
		t.Fatal(err)
	}
	reopen := func() error {
		in, err := os.Open(reopenInput)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(reopenOutput, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = reopenRunner.Run(ctx, processrunner.Command{Executable: "/usr/bin/cp", Arguments: []string{"/proc/self/fd/3", "/proc/self/fd/4"}, Files: []*os.File{in, out}})
		return err
	}
	if err := os.WriteFile(reopenOutput, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopen() == nil {
		t.Fatal("dropped helper reopened root-private certify copies; the negative control is broken")
	}
	ws := &certifyWorkspace{dir: reopenDir, fixtures: []certifyVerifiedFixture{{path: reopenInput}}}
	if err := ws.shareWithGroup(int(runAs.GID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(reopenOutput, -1, *ws.outputGID); err != nil || os.Chmod(reopenOutput, 0o660) != nil {
		t.Fatal("share output copy")
	}
	if err := reopen(); err != nil {
		t.Fatalf("dropped helper cannot reopen shared certify copies through /proc/self/fd: %v", err)
	}
	if copied, err := os.ReadFile(reopenOutput); err != nil || string(copied) != "certify fixture copy" {
		t.Fatal("dropped helper output through /proc/self/fd/4 differs")
	}
	if _, err := service.ProfileCertify(ctx, decoded); err != nil {
		// Same sanitized result the CLI prints: reason, input, check, detail.
		failure, _ := json.Marshal(certifyFailureResult(definition.ID, err))
		t.Fatalf("certify resource profile on real helpers: %v result=%s", err, failure)
	}
	if err := service.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatalf("activate resource profile: sqlstate=%s", fixtureSQLState(err))
	}

	f.run("/usr/bin/systemctl", "daemon-reload")
	f.run("/usr/bin/systemctl", "start", "nmcp-migrate.service", "nmcp-api.service", "nmcp-worker.service")
	liveWaitHTTP(t, ctx)
	slice := "/sys/fs/cgroup/nmcp.slice"
	if got := releaseCgroupValue(t, filepath.Join(slice, "memory.max")); got != strconv.FormatInt(releaseSliceMemoryMax, 10) {
		t.Fatalf("effective nmcp.slice memory.max=%s, want 4G", got)
	}
	if got := releaseCgroupValue(t, filepath.Join(slice, "cpu.max")); got != "200000 100000" {
		t.Fatalf("effective nmcp.slice cpu.max=%q, want 200%%", got)
	}
	eventsBefore := releaseCgroupEvents(t, filepath.Join(slice, "memory.events"))

	type measured struct {
		Name                string  `json:"name"`
		MIMEType            string  `json:"mime_type"`
		Bytes               int     `json:"bytes"`
		JobElapsedSeconds   float64 `json:"job_elapsed_seconds"`
		UploadToDoneSeconds float64 `json:"upload_to_done_seconds"`
	}
	var results []measured
	var rawMediaID string
	for _, fx := range []releasePrepared{rawFixture, videoFixture} {
		data, err := os.ReadFile(filepath.Join(fixtures, fx.File))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		mediaID, jobID := releasePostMedia(t, ctx, data, fx.File, "release-resources-"+fx.SHA256[:20])
		if fx.Name == rawFixture.Name {
			rawMediaID = mediaID
		}
		deadline := time.Now().Add(15 * time.Minute)
		var status string
		var elapsed float64
		for {
			err := migrator.QueryRow(ctx, `SELECT status, COALESCE(EXTRACT(EPOCH FROM finished_at-started_at),0)::float8 FROM jobs WHERE id=$1`, jobID).Scan(&status, &elapsed)
			if err != nil {
				t.Fatal("job state query")
			}
			if status == "succeeded" || status == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s job did not finish within 15m (status %s)", fx.Name, status)
			}
			time.Sleep(time.Second)
		}
		if status != "succeeded" {
			t.Fatalf("%s transform failed under nmcp.slice", fx.Name)
		}
		if pg17Scalar(t, ctx, migrator, `SELECT count(*)::text FROM renditions WHERE media_id=$1`, mediaID) != "1" {
			t.Fatalf("%s produced no current rendition", fx.Name)
		}
		results = append(results, measured{fx.Name, fx.MIMEType, len(data), elapsed, time.Since(started).Seconds()})
	}

	peak, err := strconv.ParseInt(releaseCgroupValue(t, filepath.Join(slice, "memory.peak")), 10, 64)
	if err != nil {
		t.Fatal("nmcp.slice memory.peak unreadable")
	}
	eventsAfter := releaseCgroupEvents(t, filepath.Join(slice, "memory.events"))
	units := map[string]string{}
	for _, unit := range []string{"nmcp-api.service", "nmcp-worker.service"} {
		units[unit] = releaseCgroupValue(t, filepath.Join(slice, unit, "memory.peak"))
	}
	evidence, _ := json.Marshal(map[string]any{"slice_memory_peak_bytes": peak, "slice_memory_max_bytes": releaseSliceMemoryMax,
		"memory_events_before": eventsBefore, "memory_events_after": eventsAfter, "unit_memory_peak_bytes": units,
		"cpu_stat": releaseCgroupValue(t, filepath.Join(slice, "cpu.stat")), "transforms": results})
	t.Logf("release-gate resource evidence: %s", evidence)
	if peak <= 0 || peak >= releaseSliceMemoryMax {
		t.Fatalf("nmcp.slice memory.peak=%d is not below the 4 GiB budget", peak)
	}
	for _, key := range []string{"oom", "oom_kill", "oom_group_kill"} {
		if eventsAfter[key] != eventsBefore[key] {
			t.Fatalf("nmcp.slice memory.events %s changed %d -> %d", key, eventsBefore[key], eventsAfter[key])
		}
	}
	t.Log(fmt.Sprintf("actual RAW and AV1 transforms under one nmcp.slice: peak %.2f GiB < 4 GiB, no OOM", float64(peak)/(1<<30)))

	// One real-unit ENOSPC case: shrink the media filesystem to its current
	// use plus 64 KiB; a 12 MB upload through the sandboxed API unit must fail
	// without accepting anything, and the same key must succeed after recovery.
	var fs syscall.Statfs_t
	if syscall.Statfs("/var/lib/nmcp/media", &fs) != nil {
		t.Fatal("statfs media")
	}
	used := (int64(fs.Blocks) - int64(fs.Bfree)) * int64(fs.Bsize)
	f.run("/usr/bin/mount", "-o", "remount,size="+strconv.FormatInt(used+64<<10, 10), "/var/lib/nmcp/media")
	big := liveLargeBMP()
	status := releaseUploadStatus(t, ctx, big, "enospc.bmp", "release-enospc")
	f.run("/usr/bin/mount", "-o", "remount,size=2g", "/var/lib/nmcp/media")
	if status != http.StatusInternalServerError && status != http.StatusServiceUnavailable {
		t.Fatalf("real API upload on a full media filesystem: status %d, want 500/503", status)
	}
	if pg17Scalar(t, ctx, migrator, `SELECT count(*)::text FROM originals WHERE size_bytes=$1`, len(big)) != "0" {
		t.Fatal("ENOSPC upload accepted an original")
	}
	if got := releaseUploadStatus(t, ctx, big, "enospc.bmp", "release-enospc"); got != http.StatusCreated {
		t.Fatalf("same key after ENOSPC recovery: status %d, want 201 (transient failures are never stored)", got)
	}
	operator := releaseOperatorCLI(t, f, admin)
	releaseCheckResult(t, ctx, migrator, operator([]int{0}, "check", "--scope", "all", "--json"), false)
	t.Log("actual real-unit ENOSPC: upload refused without acceptance, same key accepted after recovery, DB/FS reconciliation clean")

	// Manual rendition cleanup through the documented operator invocation.
	// A superseded rendition's purge_after is fixed at supersession from the
	// retention then in force, so zero-day retention is set first; a second
	// version of the measured profile (same recipes, existing certification
	// rows) is activated, the Worker restarted to claim it, the RAW media
	// regenerated, and the shipped CLI (owner mode, media read-only to it)
	// awaits the Worker's sweep of the due superseded rendition (review D1).
	operator([]int{0}, "config", "set", "--name", "superseded_rendition_retention_days", "--value", "0", "--json")
	var oldRendition, oldPath string
	if err := migrator.QueryRow(ctx, `SELECT id::text, relative_path FROM renditions WHERE media_id=$1 AND is_current`, rawMediaID).Scan(&oldRendition, &oldPath); err != nil {
		t.Fatal("current RAW rendition query")
	}
	v2 := definition
	v2.ID, v2.Version = newID(), definition.Version+1
	if err := service.ProfileCreate(ctx, v2); err != nil {
		t.Fatalf("create resource profile v2: sqlstate=%s", fixtureSQLState(err))
	}
	if err := service.ProfileTransition(ctx, v2.ID, true); err != nil {
		t.Fatalf("activate resource profile v2 on the existing certification registry: sqlstate=%s", fixtureSQLState(err))
	}
	f.run("/usr/bin/systemctl", "restart", "nmcp-worker.service")
	operator([]int{0}, "regenerate", "--profile", definition.Key, "--media-id", rawMediaID, "--yes", "--json")
	for deadline := time.Now().Add(15 * time.Minute); ; {
		if pg17Scalar(t, ctx, migrator, `SELECT count(*)::text FROM renditions WHERE media_id=$1 AND is_current AND id::text<>$2`, rawMediaID, oldRendition) == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("regenerated RAW rendition did not become current within 15m")
		}
		time.Sleep(time.Second)
	}
	operator([]int{0}, "cleanup", "--kind", "renditions", "--yes", "--json")
	if pg17Scalar(t, ctx, migrator, `SELECT count(*)::text FROM renditions WHERE id::text=$1`, oldRendition) != "0" {
		t.Fatal("manual cleanup left the due superseded rendition row")
	}
	if _, err := os.Lstat(filepath.Join("/var/lib/nmcp/media", oldPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manual cleanup left the due superseded rendition file")
	}
	operator([]int{0}, "config", "set", "--name", "superseded_rendition_retention_days", "--value", "null", "--json")

	// Manual repair through the same invocation: a planted unreferenced
	// canonical original (nmcp-owned, as a lost publish would leave it) is a
	// repairable final_orphan; repair quarantines it under maintenance, a
	// fresh check is clean, and maintenance resume returns to normal mode.
	orphanID := newID()
	orphanDir := filepath.Join("/var/lib/nmcp/media/originals", orphanID[:2], orphanID)
	for _, dir := range []string{filepath.Dir(orphanDir), orphanDir} {
		f.run("/usr/bin/install", "-d", "-o", runtimeUser.Username, "-g", runtimeUser.Username, "-m", "0700", dir)
	}
	orphanSource := filepath.Join(t.TempDir(), "orphan")
	if err := os.WriteFile(orphanSource, liveLargeBMP()[:4096], 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(orphanDir, "original.bmp")
	f.run("/usr/bin/install", "-o", runtimeUser.Username, "-g", runtimeUser.Username, "-m", "0600", orphanSource, orphan)
	report := releaseCheckResult(t, ctx, migrator, operator([]int{4}, "check", "--scope", "all", "--json"), true)
	operator([]int{0}, "repair", "--report", report, "--yes", "--json")
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repair left the planted orphan in place")
	}
	releaseCheckResult(t, ctx, migrator, operator([]int{0}, "check", "--scope", "all", "--json"), false)
	operator([]int{0}, "maintenance", "resume", "--yes", "--json")

	// Nothing the operator commands created or renamed may be root-owned:
	// every entry under media stays the nmcp runtime user's.
	uid, _ := strconv.ParseUint(runtimeUser.Uid, 10, 32)
	gid, _ := strconv.ParseUint(runtimeUser.Gid, 10, 32)
	quarantined := 0
	if err := filepath.WalkDir("/var/lib/nmcp/media", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if uint64(stat.Uid) != uid || uint64(stat.Gid) != gid {
			return fmt.Errorf("%s owned by %d:%d, want nmcp %d:%d", strings.TrimPrefix(path, "/var/lib/nmcp/media"), stat.Uid, stat.Gid, uid, gid)
		}
		if strings.HasPrefix(path, "/var/lib/nmcp/media/.quarantine/") && info.Mode().IsRegular() {
			quarantined++
		}
		return nil
	}); err != nil {
		t.Fatalf("media ownership after operator cleanup/repair: %v", err)
	}
	if quarantined == 0 {
		t.Fatal("repair quarantined nothing")
	}
	t.Log("actual operator CLI via both documented systemd-run modes: owner cleanup awaits the Worker sweep; capability check -> repair -> check -> resume as nmcp; every media entry nmcp-owned")
}

// releaseOperatorCLI runs the shipped nmcp-admin exactly as the Ubuntu
// deployment documents ("Running nmcp-admin", review B2): capability
// commands (check, repair, maintenance resume) as a transient unit of the
// nmcp user with only check.dsn / repair.dsn; every other command as root
// with only admin.dsn. capability.env is non-secret and read by PID 1. It
// returns stdout and fails unless the exit status is one of the allowed codes.
func releaseOperatorCLI(t *testing.T, f *liveCutoverFixture, admin *pgxpool.Pool) func(allowed []int, args ...string) []byte {
	t.Helper()
	for _, role := range []string{"nmcp_check", "nmcp_repair"} {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			t.Fatal(err)
		}
		f.passwords[role] = hex.EncodeToString(secret[:])
		f.sql(admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+f.passwords[role]+"'")
		leaf := strings.TrimPrefix(role, "nmcp_")
		dsn := (&url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/source", User: url.UserPassword(role, f.passwords[role]), RawQuery: "sslmode=disable"}).String()
		f.write("/etc/nmcp/credentials/"+leaf+".dsn", dsn+"\n", 0600)
	}
	owner := (&url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/source", User: url.UserPassword("nmcp_migrator", f.passwords["nmcp_migrator"]), RawQuery: "sslmode=disable"}).String()
	f.write("/etc/nmcp/credentials/admin.dsn", owner+"\n", 0600)
	f.write("/etc/nmcp/capability.env", "NMCP_STORAGE_ROOT=/var/lib/nmcp/media\nNMCP_BACKUP_ROOT=/var/lib/nmcp-backup\n"+
		"NMCP_FILE_BASE_URL=https://files.example.test/files/\nNMCP_LOG_LEVEL=info\nNMCP_SHUTDOWN_TIMEOUT=30s\n", 0644)
	return func(allowed []int, args ...string) []byte {
		t.Helper()
		mode := []string{"-p", "User=root",
			"-p", "EnvironmentFile=/etc/nmcp/capability.env",
			"-p", "LoadCredential=admin.dsn:/etc/nmcp/credentials/admin.dsn"}
		name := args[0]
		if len(args) > 1 && args[0] == "maintenance" {
			name += " " + args[1]
		}
		if capabilityCommand(name) {
			mode = []string{"-p", "User=nmcp", "-p", "Group=nmcp",
				"-p", "EnvironmentFile=/etc/nmcp/capability.env",
				"-p", "LoadCredential=check.dsn:/etc/nmcp/credentials/check.dsn",
				"-p", "LoadCredential=repair.dsn:/etc/nmcp/credentials/repair.dsn"}
		}
		command := append(append([]string{"--quiet", "--pipe", "--wait", "--collect"}, mode...), "/opt/nmcp/current/bin/nmcp-admin")
		command = append(command, args...)
		c := exec.CommandContext(f.ctx, "/usr/bin/systemd-run", command...)
		c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		var stdout, stderr bytes.Buffer
		c.Stdout, c.Stderr = &stdout, &stderr
		err := c.Run()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("operator nmcp-admin %s did not run: %v", args[0], err)
		}
		for _, ok := range allowed {
			if code == ok {
				return stdout.Bytes()
			}
		}
		t.Fatalf("operator nmcp-admin %s exit %d, want %v; stdout=%.512s stderr=%.512s", strings.Join(args, " "), code, allowed, stdout.String(), stderr.String())
		return nil
	}
}

// releaseCheckResult decodes check's stdout (bare result on exit 0, the
// {"result":...,"exit_code":4} envelope when findings exist) and proves the
// reported ReportID, seal and finding count against the stored report.
func releaseCheckResult(t *testing.T, ctx context.Context, migrator *pgxpool.Pool, out []byte, wantFindings bool) string {
	t.Helper()
	type result struct {
		ReportID     string
		Sealed       bool
		FindingCount int64
	}
	var got result
	if wantFindings {
		var envelope struct {
			Result   result `json:"result"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal(out, &envelope) != nil || envelope.ExitCode != 4 {
			t.Fatalf("check with findings: unexpected output %s", out)
		}
		got = envelope.Result
	} else if json.Unmarshal(out, &got) != nil {
		t.Fatalf("clean check: unexpected output %s", out)
	}
	if !uuidPattern.MatchString(got.ReportID) || !got.Sealed || (got.FindingCount > 0) != wantFindings {
		t.Fatalf("check result %+v, want sealed with findings=%t", got, wantFindings)
	}
	var stored int64
	var sealed bool
	if err := migrator.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_check_findings WHERE report_id=r.id),
		EXISTS(SELECT 1 FROM reconciliation_check_report_seals s WHERE s.report_id=r.id)
		FROM reconciliation_check_reports r WHERE r.id=$1`, got.ReportID).Scan(&stored, &sealed); err != nil || !sealed || stored != got.FindingCount {
		t.Fatalf("stored report %s: sealed=%t findings=%d, CLI said %d (%v)", got.ReportID, sealed, stored, got.FindingCount, err)
	}
	return got.ReportID
}

func releaseUploadStatus(t *testing.T, ctx context.Context, data []byte, name, key string) int {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, _ := w.CreateFormFile("file", name)
	part.Write(data)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8080/media", &b)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// liveLargeBMP is a distinct 2000x2000 24-bit BMP (about 12 MB).
func liveLargeBMP() []byte {
	const w, h = 2000, 2000
	row := (w*3 + 3) &^ 3
	size := 54 + row*h
	b := make([]byte, size)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(size))
	binary.LittleEndian.PutUint32(b[10:], 54)
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], w)
	binary.LittleEndian.PutUint32(b[22:], h)
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], 24)
	binary.LittleEndian.PutUint32(b[34:], uint32(row*h))
	for i := 54; i < size; i++ {
		b[i] = byte(i * 2654435761 >> 24)
	}
	return b
}
