package e2e

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Any active version of each bundled key will do: the chain may already
	// have moved standard to v2 on this shared stack.
	var profiles struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
	}
	h.expectJSON(h.do(http.MethodGet, "/profiles?status=active", nil, nil), 200, &profiles)
	active := map[string]bool{}
	for _, p := range profiles.Items {
		active[p.Key] = true
	}
	if !active["standard"] || !active["thumbnail"] {
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

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// checkOutput is what `nmcp-admin check --json` prints: the bare result on
// exit 0, or {"result":{...},"exit_code":N,...} on a non-zero exit.
type checkOutput struct {
	ReportID string `json:"ReportID"`
	Result   *struct {
		ReportID string `json:"ReportID"`
	} `json:"result"`
	ExitCode *int `json:"exit_code"`
}

// parseCheckOutput returns the ReportID this check run printed and the exit
// code it declared (0 for a bare result).
func parseCheckOutput(stdout string) (string, int, error) {
	var out checkOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
		return "", 0, err
	}
	if out.ExitCode == nil {
		if out.ReportID == "" {
			return "", 0, errors.New("bare check result without ReportID")
		}
		return out.ReportID, 0, nil
	}
	if out.Result == nil || out.Result.ReportID == "" {
		return "", *out.ExitCode, errors.New("check envelope without result.ReportID")
	}
	return out.Result.ReportID, *out.ExitCode, nil
}

func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// assertReconciled runs the read-only DB/FS reconciliation check and asserts
// on exactly the report THIS run printed (review B3/K3), never "the newest
// row": it must be sealed with every source complete, and the exit status
// must be 0 with no findings or 4 with findings, all of the allowed kinds
// (none for assertConsistent). The asserted report becomes h.lastReport,
// the baseline of per-point residue diffs.
func (h *harness) assertReconciled(when string, allowed map[string]bool) {
	h.t.Helper()
	var stdout, stderr, id, sealed string
	var count, code int
	var err error
	// The checker's storage scan fails closed (storage_scan_unstable) when the
	// media tree changes while it runs, for example while a just-restarted
	// Worker starts up. Only that recorded cause of this run's own report is
	// retried, at most three times; any other outcome is asserted at once.
	for attempt := 1; ; attempt++ {
		stdout, stderr, err = h.runResult("", "admin", "--", "check", "--scope", "all", "--json")
		code = exitCode(err)
		var declared int
		var parseErr error
		id, declared, parseErr = parseCheckOutput(stdout)
		if parseErr != nil || !uuidPattern.MatchString(id) || declared != code {
			h.t.Fatalf("check after %s printed no usable report (exit %d, declared %d): %v stdout=%.300s stderr=%s", when, code, declared, parseErr, stdout, lastLine(stderr))
		}
		row := h.dbRead(`SELECT (SELECT count(*) FROM reconciliation_check_report_seals s WHERE s.report_id=r.id)::text
			|| '|' || COALESCE((SELECT s.finding_count::text FROM reconciliation_check_report_seals s WHERE s.report_id=r.id),'-1')
			|| '|' || (SELECT count(*) FROM reconciliation_check_source_results x WHERE x.report_id=r.id AND x.result='complete')::text
			|| '|' || COALESCE((SELECT error_code FROM reconciliation_check_source_results x WHERE x.report_id=r.id AND x.source='storage_scan'),'')
			FROM reconciliation_check_reports r WHERE r.id='` + id + `'`)
		parts := strings.Split(row, "|")
		if len(parts) != 4 {
			h.t.Fatalf("check after %s: report %s not stored (%q)", when, id, row)
		}
		sealed = parts[0]
		count, _ = strconv.Atoi(parts[1])
		if parts[0] == "1" && parts[2] != "3" {
			h.t.Fatalf("check after %s: sealed report %s has %s complete sources", when, id, parts[2])
		}
		if sealed == "1" || code != 1 || parts[3] != "storage_scan_unstable" || attempt == 3 {
			break
		}
		h.record("check_retry", map[string]any{"when": when, "attempt": attempt, "report_id": id, "storage_scan": parts[3]})
		time.Sleep(5 * time.Second)
	}
	kinds := h.dbRead(`SELECT COALESCE(string_agg(kind || ':' || actionability, ',' ORDER BY kind), '') FROM reconciliation_check_findings WHERE report_id='` + id + `'`)
	h.record("check", map[string]any{"when": when, "exit_code": code, "report_id": id, "findings": kinds, "result": json.RawMessage(nonEmptyJSON(stdout))})
	if sealed != "1" {
		h.t.Fatalf("reconciliation after %s did not seal: report=%s exit=%d stderr=%s", when, id, code, lastLine(stderr))
	}
	stored := 0
	for _, f := range strings.Split(kinds, ",") {
		if f == "" {
			continue
		}
		stored++
		kind, _, _ := strings.Cut(f, ":")
		if !allowed[kind] {
			h.t.Fatalf("reconciliation after %s has a disallowed finding %s (all: %s)", when, f, kinds)
		}
	}
	switch {
	case stored != count:
		h.t.Fatalf("reconciliation after %s: seal counts %d findings, report %s stores %d", when, count, id, stored)
	case code == 0 && count == 0, code == 4 && count > 0:
	default:
		h.t.Fatalf("reconciliation after %s: exit %d with %d findings (want 0 with none, or 4 with findings)", when, code, count)
	}
	if len(allowed) == 0 && count != 0 {
		h.t.Fatalf("reconciliation after %s not clean: %s", when, kinds)
	}
	h.lastReport = id
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
