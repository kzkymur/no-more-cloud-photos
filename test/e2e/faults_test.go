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

// Finding kinds a crash or kill may legitimately leave behind: a temporary
// file of the interrupted attempt, and a published file whose DB commit never
// happened. The storage contract keeps both (they are reconcilable only after
// a 48 h cutoff), and neither may be referenced by DB state. Every other kind
// (missing/mismatched/invalid references, unexpected paths/types, residue)
// is a consistency failure.
var crashResidueKinds = map[string]bool{"recent_or_live_temp": true, "final_orphan": true}

// assertReconciled runs the read-only DB/FS reconciliation check and requires
// a sealed report whose findings are all of the allowed kinds (none for
// assertConsistent) and none of them repairable-by-mistake references.
func (h *harness) assertReconciled(when string, allowed map[string]bool) {
	h.t.Helper()
	stdout, stderr, err := h.runResult("", "admin", "--", "check", "--scope", "all", "--json")
	report := h.dbRead(`SELECT id::text || '|' || (SELECT count(*) FROM reconciliation_check_report_seals s WHERE s.report_id=r.id)::text
		FROM reconciliation_check_reports r ORDER BY r.started_at DESC LIMIT 1`)
	id, sealed, _ := strings.Cut(report, "|")
	kinds := h.dbRead(`SELECT COALESCE(string_agg(kind || ':' || actionability, ',' ORDER BY kind), '') FROM reconciliation_check_findings WHERE report_id='` + id + `'`)
	h.record("check", map[string]any{"when": when, "exit_error": errText(err), "report_id": id, "findings": kinds, "result": json.RawMessage(nonEmptyJSON(stdout))})
	if sealed != "1" {
		h.t.Fatalf("reconciliation after %s did not seal: err=%s stderr=%s", when, errText(err), lastLine(stderr))
	}
	for _, f := range strings.Split(kinds, ",") {
		if f == "" {
			continue
		}
		kind, _, _ := strings.Cut(f, ":")
		if !allowed[kind] {
			h.t.Fatalf("reconciliation after %s has a disallowed finding %s (all: %s)", when, f, kinds)
		}
	}
	if len(allowed) == 0 && err != nil {
		h.t.Fatalf("reconciliation after %s not clean: %s", when, errText(err))
	}
}

func (h *harness) assertConsistent(when string) { h.assertReconciled(when, nil) }

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
