//go:build !nmcp_failpoints_e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

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
		h.assertReconciled("worker SIGKILL", crashResidueKinds)
	})

	t.Run("worker SIGSTOP heartbeat loss never double-publishes", func(t *testing.T) {
		m, j := h.uploadAndRunning(t, "video-quicktime-generated")
		h.run("", "control", "kill", "worker", "STOP")
		time.Sleep(2*time.Minute + 20*time.Second) // past lease expiry without heartbeats
		h.run("", "control", "kill", "worker", "CONT")
		done := h.waitJob(j.ID, "succeeded", reclaimWait)
		h.record("sigstop_heartbeat", map[string]any{"job_id": j.ID, "attempts": done.Attempts})
		h.assertExactlyOneCurrentPerTarget(m.ID)
		h.assertReconciled("worker SIGSTOP", crashResidueKinds)
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
		h.assertReconciled("database loss", crashResidueKinds)
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
			h.assertReconciled(fault.name, crashResidueKinds)
			var up struct {
				Media media `json:"media"`
				Job   *job  `json:"job"`
			}
			h.expectJSON(h.upload(key, "f.png", "image/png", data), 201, &up)
			h.waitJob(up.Job.ID, "succeeded", 5*time.Minute)
			h.assertExactlyOneCurrentPerTarget(up.Media.ID)
			h.assertReconciled(fault.name+" recovered", crashResidueKinds)
		})
	}
}
