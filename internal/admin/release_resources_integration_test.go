package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
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
		t.Fatalf("upload %s: status=%d", name, resp.StatusCode)
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
	for _, fx := range []releasePrepared{rawFixture, videoFixture} {
		data, err := os.ReadFile(filepath.Join(fixtures, fx.File))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		mediaID, jobID := releasePostMedia(t, ctx, data, fx.File, "release-resources-"+fx.SHA256[:20])
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
	store, err := storage.Open("/var/lib/nmcp/media", storage.Options{})
	if err != nil {
		t.Fatal("open real media store")
	}
	defer store.Close()
	if result, err := New(migrator).Check(ctx, store, reconciliation.ScopeAll); err != nil {
		t.Fatalf("reconciliation after real-unit ENOSPC: sealed=%t findings=%d", result.Sealed, result.FindingCount)
	}
	t.Log("actual real-unit ENOSPC: upload refused without acceptance, same key accepted after recovery, DB/FS reconciliation clean")
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
