package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Worker lease (internal/job defaultLeaseDuration) is 2 minutes and the
// heartbeat 30 s; reclaim can only happen after expiry. No test-only knob is
// added to production configuration.
const reclaimWait = 4 * time.Minute

func (h *harness) ensureProfilesActive() {
	h.t.Helper()
	var profiles struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	h.expectJSON(h.do(http.MethodGet, "/profiles?status=active", nil, nil), 200, &profiles)
	active := map[string]bool{}
	for _, p := range profiles.Items {
		active[p.ID] = true
	}
	if !active[standardProfileID] || !active[thumbnailProfileID] {
		h.activateBundledProfiles()
	}
}

// assertConsistent runs the read-only reconciliation check over DB and media
// files: it must seal with zero findings (exit 0). Findings are a failure.
func (h *harness) assertConsistent(when string) {
	h.t.Helper()
	stdout, stderr, err := h.runResult("", "admin", "--", "check", "--scope", "all", "--json")
	h.record("check", map[string]any{"when": when, "exit_error": errText(err), "result": json.RawMessage(nonEmptyJSON(stdout))})
	if err != nil {
		h.t.Fatalf("reconciliation after %s not clean: err=%s result=%s stderr=%s", when, errText(err), truncate([]byte(stdout)), lastLine(stderr))
	}
}

func (h *harness) pid(service string) int {
	h.t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(h.run("", "control", "pid", service)))
	if err != nil {
		h.t.Fatalf("control pid %s: %v", service, err)
	}
	return n
}

// uploadAndRunning uploads a prepared 1440p video (its AV1 transform runs long
// enough to interrupt) and returns once the job is observed running. A job
// that finishes before the fault is a failed precondition, not a pass.
func (h *harness) uploadAndRunning(t *testing.T, fixture string) (media, job) {
	t.Helper()
	var chosen preparedFixture
	for _, f := range h.preparedFixtures() {
		if f.Name == fixture {
			chosen = f
		}
	}
	if chosen.File == "" {
		t.Fatalf("prepared fixture %s missing", fixture)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("NMCP_E2E_FIXTURES_DIR"), "prepared", chosen.File))
	if err != nil || sha(data) != chosen.SHA256 {
		t.Fatalf("prepared fixture %s unreadable or changed", fixture)
	}
	var up struct {
		Media media `json:"media"`
		Job   *job  `json:"job"`
	}
	h.expectJSON(h.upload("e2e-fault-"+chosen.SHA256[:20], chosen.File, chosen.MIMEType, data), 201, &up)
	if up.Job == nil {
		t.Fatal("fault upload created no job")
	}
	h.eventually("job running", 3*time.Minute, func() bool {
		j := h.getJob(up.Job.ID)
		if j.Status == "succeeded" {
			t.Fatal("job finished before the fault could be injected")
		}
		return j.Status == "running"
	})
	return up.Media, *up.Job
}

func (h *harness) assertExactlyOneCurrentPerTarget(mediaID string) {
	h.t.Helper()
	m, status := h.getMedia(mediaID)
	if status != 200 {
		h.t.Fatalf("media %s status %d", mediaID, status)
	}
	byKey := map[string]int{}
	for _, r := range m.CurrentRenditions {
		byKey[r.Profile.Key]++
		h.assertFileBytes(r.FileURL, r.SHA256, r.MIMEType, r.SizeBytes)
	}
	if byKey["standard"] != 1 || byKey["thumbnail"] != 1 {
		h.t.Fatalf("current renditions after fault: %v", byKey)
	}
}

// TestReleaseGateFaults injects process, heartbeat, database and storage
// faults into the real stack and proves recovery plus DB/FS consistency.
func TestReleaseGateFaults(t *testing.T) {
	h := newHarness(t)
	h.ensureProfilesActive()
	h.assertConsistent("baseline")

	t.Run("worker SIGKILL mid-job is reclaimed after lease expiry", func(t *testing.T) {
		m, j := h.uploadAndRunning(t, "video-mp4-generated")
		before := h.pid("worker")
		killed := h.run("", "control", "kill", "worker", "KILL")
		h.run("", "control", "start", "worker")
		after := h.pid("worker")
		if after == 0 || after == before {
			t.Fatalf("worker not replaced: before=%d after=%d (%s)", before, after, strings.TrimSpace(killed))
		}
		done := h.waitJob(j.ID, "succeeded", reclaimWait)
		h.record("sigkill_reclaim", map[string]any{"job_id": j.ID, "attempts": done.Attempts, "killed": strings.TrimSpace(killed)})
		h.assertExactlyOneCurrentPerTarget(m.ID)
		h.assertConsistent("worker SIGKILL")
	})

	t.Run("worker SIGSTOP heartbeat loss never double-publishes", func(t *testing.T) {
		m, j := h.uploadAndRunning(t, "video-quicktime-generated")
		h.run("", "control", "kill", "worker", "STOP")
		time.Sleep(2*time.Minute + 20*time.Second) // past lease expiry without heartbeats
		h.run("", "control", "kill", "worker", "CONT")
		done := h.waitJob(j.ID, "succeeded", reclaimWait)
		h.record("sigstop_heartbeat", map[string]any{"job_id": j.ID, "attempts": done.Attempts})
		h.assertExactlyOneCurrentPerTarget(m.ID)
		h.assertConsistent("worker SIGSTOP")
	})

	t.Run("database loss and recovery", func(t *testing.T) {
		h.run("", "control", "stop", "postgres")
		h.eventually("readyz unavailable", time.Minute, func() bool {
			return h.do(http.MethodGet, "/readyz", nil, nil).Status == 503
		})
		data := fixtureImage(t, "png", 800, 600)
		key := "e2e-dbdown-" + sha(data)[:20]
		if r := h.upload(key, "db.png", "image/png", data); r.Status != 503 {
			t.Fatalf("upload while database down: %d", r.Status)
		}
		h.run("", "control", "start", "postgres")
		h.eventually("readyz ready", 2*time.Minute, func() bool {
			return h.do(http.MethodGet, "/readyz", nil, nil).Status == 200
		})
		var up struct {
			Media media `json:"media"`
			Job   *job  `json:"job"`
		}
		// The transient 503 was never stored: the same key is a first acceptance.
		h.expectJSON(h.upload(key, "db.png", "image/png", data), 201, &up)
		h.waitJob(up.Job.ID, "succeeded", 5*time.Minute)
		h.assertExactlyOneCurrentPerTarget(up.Media.ID)
		h.assertConsistent("database loss")
	})

	for _, fault := range []struct{ name, on, off string }{
		{"read-only media filesystem", "media-readonly", "media-readonly"},
		{"full media filesystem", "media-full", "media-full"},
	} {
		t.Run(fault.name, func(t *testing.T) {
			h.run("", "fault", fault.on, "on")
			data := fixtureImage(t, "png", 1200, 900)
			key := "e2e-" + fault.on + "-" + sha(data)[:16]
			r := h.upload(key, "f.png", "image/png", data)
			h.run("", "fault", fault.off, "off")
			if r.Status != 500 && r.Status != 503 {
				t.Fatalf("upload during %s: status %d, want 500/503", fault.name, r.Status)
			}
			var none struct {
				Items []media `json:"items"`
			}
			h.expectJSON(h.do(http.MethodGet, "/media?deleted=include&limit=200", nil, nil), 200, &none)
			for _, m := range none.Items {
				if m.SizeBytes == int64(len(data)) && m.OriginalFilename != nil && *m.OriginalFilename == "f.png" {
					t.Fatal("failed upload left an accepted media row")
				}
			}
			h.assertConsistent(fault.name)
			var up struct {
				Media media `json:"media"`
				Job   *job  `json:"job"`
			}
			h.expectJSON(h.upload(key, "f.png", "image/png", data), 201, &up)
			h.waitJob(up.Job.ID, "succeeded", 5*time.Minute)
			h.assertExactlyOneCurrentPerTarget(up.Media.ID)
			h.assertConsistent(fault.name + " recovered")
		})
	}
}
