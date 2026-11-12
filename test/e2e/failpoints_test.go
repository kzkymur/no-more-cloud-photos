//go:build nmcp_failpoints_e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
)

// This file is compiled only into the failpoint lane's scenario binary
// (go test -c -tags nmcp_failpoints_e2e), which runs against the instrumented
// image (-tags nmcp_failpoints). The release lane never contains it.

func (h *harness) setFailpoint(service, point string) {
	h.t.Helper()
	out := h.run("", "control", "set-failpoint", service, point)
	if !strings.Contains(out, "failpoint="+point) {
		h.t.Fatalf("set-failpoint %s %s not confirmed: %q", service, point, strings.TrimSpace(out))
	}
}

func (h *harness) waitDead(service string) {
	h.t.Helper()
	h.eventually(service+" killed by failpoint", 5*time.Minute, func() bool { return h.pid(service) == 0 })
}

// uploadRaw is a multipart upload that tolerates the server dying mid-request.
func (h *harness) uploadRaw(key, filename string, data []byte) (int, http.Header, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	part, err := w.CreatePart(header)
	if err != nil {
		return 0, nil, err
	}
	part.Write(data)
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.api+"/media", &body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	resp, err := h.apiHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// residueFinding is one finding of the latest sealed reconciliation report.
type residueFinding struct {
	Kind, Actionability, SubjectType, Key, MediaID, SHA256 string
}

// latestFindings returns the latest report's findings keyed by kind, subject
// type and relative key (or subject), so residue can be diffed per point.
func (h *harness) latestFindings() map[string]residueFinding {
	h.t.Helper()
	rows := h.dbRead(`SELECT COALESCE(string_agg(concat_ws('|',f.kind,f.actionability,f.subject_type,
			COALESCE(f.relative_key,f.subject_id::text,''),COALESCE(f.media_id::text,''),COALESCE(f.observed_sha256,'')),E'\n' ORDER BY f.ordinal),'')
		FROM reconciliation_check_findings f WHERE f.report_id=(SELECT r.id FROM reconciliation_check_reports r
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1)`)
	out := map[string]residueFinding{}
	for _, line := range strings.Split(rows, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 6 {
			h.t.Fatalf("unparseable finding row %q", line)
		}
		f := residueFinding{parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]}
		out[f.Kind+"|"+f.SubjectType+"|"+f.Key] = f
	}
	return out
}

// residue is what one failpoint may leave relative to the previous report
// (review B4): before a publish only the interrupted attempt's temporary
// file (recent, never actionable); after a publish without its commit
// exactly one unreferenced final of this upload or media (repairable, so
// operator repair can quarantine it); at every later point nothing new.
type residue int

const (
	residueNone residue = iota
	residueTemp
	residueOrphan
)

// assertResidue checks the findings new since prev against the point's
// expected residue. orphanMatch identifies "this media's" orphan.
func (h *harness) assertResidue(point string, prev map[string]residueFinding, want residue, orphanMatch func(residueFinding) bool) {
	h.t.Helper()
	var added []residueFinding
	for key, f := range h.latestFindings() {
		if _, ok := prev[key]; !ok {
			added = append(added, f)
		}
	}
	h.record("failpoint_residue", map[string]any{"point": point, "new_findings": added})
	switch want {
	case residueNone:
		if len(added) != 0 {
			h.t.Fatalf("%s: new findings %+v, want none", point, added)
		}
	case residueTemp:
		if len(added) > 1 {
			h.t.Fatalf("%s: new findings %+v, want at most the interrupted attempt's temp", point, added)
		}
		for _, f := range added {
			if f.Kind != "recent_or_live_temp" || f.Actionability != "non_actionable" ||
				(f.SubjectType != "original_attempt_temp" && f.SubjectType != "rendition_attempt_temp") {
				h.t.Fatalf("%s: new finding %+v, want only a non-actionable recent attempt temp", point, f)
			}
		}
	case residueOrphan:
		if len(added) != 1 || added[0].Kind != "final_orphan" || added[0].Actionability != "repairable" || !orphanMatch(added[0]) {
			h.t.Fatalf("%s: new findings %+v, want exactly one repairable final_orphan of this media", point, added)
		}
	}
}

func TestReleaseGateFailpoints(t *testing.T) {
	h := newHarness(t)
	if os.Getenv("NMCP_E2E_LANE") != "failpoints" || os.Getenv("NMCP_E2E_FAILPOINTS") != "1" {
		t.Fatal("failpoint scenario requires the instrumented failpoint lane")
	}
	h.ensureProfilesActive()
	h.assertConsistent("failpoint lane baseline")

	// Upload points: the API SIGKILLs itself inside acceptance. The same key is
	// then retried against a clean API; the stored idempotency row (or its
	// absence) decides the result, never request-path deletion.
	for _, c := range []struct {
		point   string
		replay  bool // the acceptance committed before the crash
		residue residue
	}{
		{"upload.before_original_publish", false, residueTemp},
		{"upload.after_original_publish", false, residueOrphan},
		{"upload.after_db_commit", true, residueNone},
	} {
		t.Run(c.point, func(t *testing.T) {
			prev := h.latestFindings()
			data := fixtureImage(t, "png", 2000, 1500)
			key := "e2e-fp-" + strings.ReplaceAll(c.point, ".", "-") + "-" + sha(data)[:12]
			h.setFailpoint("api", c.point)
			status, _, err := h.uploadRaw(key, "fp.png", data)
			if err == nil && status == http.StatusCreated {
				t.Fatalf("upload succeeded although %s should have killed the API", c.point)
			}
			h.waitDead("api")
			h.setFailpoint("api", "none")
			again := h.upload(key, "fp.png", "image/png", data)
			var created struct {
				Media media `json:"media"`
				Job   *job  `json:"job"`
			}
			h.expectJSON(again, 201, &created)
			if replayed := again.Header.Get("Idempotency-Replayed") == "true"; replayed != c.replay {
				t.Fatalf("%s retry replayed=%t, want %t", c.point, replayed, c.replay)
			}
			h.waitJob(created.Job.ID, "succeeded", 10*time.Minute)
			h.assertExactlyOneCurrentPerTarget(created.Media.ID)
			h.assertReconciled(c.point, crashResidueKinds)
			// The crashed acceptance's original is this upload's bytes under
			// an ID the retry never committed.
			h.assertResidue(c.point, prev, c.residue, func(f residueFinding) bool {
				return f.SubjectType == "original" && f.SHA256 == sha(data)
			})
			h.record("failpoint", map[string]any{"point": c.point, "media_id": created.Media.ID, "replayed": c.replay})
		})
	}

	// Rendition publish points: the Worker dies at the boundary of the first
	// target; after the lease expires a clean Worker reclaims and finishes.
	for _, c := range []struct {
		point   string
		residue residue
	}{
		{"rendition.before_file_publish", residueTemp},
		{"rendition.after_file_publish", residueOrphan},
		{"rendition.after_db_commit", residueNone},
	} {
		point := c.point
		t.Run(point, func(t *testing.T) {
			prev := h.latestFindings()
			h.setFailpoint("worker", point)
			data := fixtureImage(t, "png", 2000, 1500)
			var up struct {
				Media media `json:"media"`
				Job   *job  `json:"job"`
			}
			h.expectJSON(h.upload("e2e-fp-"+strings.ReplaceAll(point, ".", "-")+"-"+sha(data)[:12], "fp.png", "image/png", data), 201, &up)
			h.waitDead("worker")
			h.setFailpoint("worker", "none")
			done := h.waitJob(up.Job.ID, "succeeded", reclaimWait)
			if done.Attempts < 2 {
				t.Fatalf("%s: job finished in %d attempt(s); the crash was not reclaimed", point, done.Attempts)
			}
			h.assertExactlyOneCurrentPerTarget(up.Media.ID)
			h.assertReconciled(point, crashResidueKinds)
			h.assertResidue(point, prev, c.residue, func(f residueFinding) bool {
				return f.SubjectType == "rendition" && f.MediaID == up.Media.ID
			})
			h.record("failpoint", map[string]any{"point": point, "job_id": up.Job.ID, "attempts": done.Attempts})
		})
	}

	// Purge points: the Worker dies inside purge; the reclaimed purge must
	// converge to no Media, no files, a retained job and one tombstone.
	for _, point := range []string{"purge.after_start", "purge.after_file_delete", "purge.before_finalize", "purge.after_finalize", "purge.after_file_step"} {
		t.Run(point, func(t *testing.T) {
			prev := h.latestFindings()
			data := fixtureImage(t, "png", 1200, 900)
			var up struct {
				Media media `json:"media"`
				Job   *job  `json:"job"`
			}
			h.expectJSON(h.upload("e2e-fp-"+strings.ReplaceAll(point, ".", "-")+"-"+sha(data)[:12], "fp.png", "image/png", data), 201, &up)
			h.waitJob(up.Job.ID, "succeeded", 10*time.Minute)
			m, _ := h.getMedia(up.Media.ID)
			orig := h.original(up.Media.ID)
			h.expectJSON(h.do(http.MethodDelete, "/media/"+up.Media.ID, nil, nil), 200, nil)
			h.setFailpoint("worker", point)
			var purge job
			h.expectJSON(h.do(http.MethodDelete, "/media/"+up.Media.ID+"/purge", nil, nil), 202, &purge)
			h.waitDead("worker")
			h.setFailpoint("worker", "none")
			h.waitJob(purge.ID, "succeeded", reclaimWait)
			if _, status := h.getMedia(up.Media.ID); status != 404 {
				t.Fatalf("%s: purged media detail status %d", point, status)
			}
			h.assertFileGone(orig.FileURL)
			for _, r := range m.CurrentRenditions {
				h.assertFileGone(r.FileURL)
			}
			h.mediaTreeLacks(orig.ID)
			tombstones := 0
			for _, c := range h.changesFor(up.Media.ID) {
				if c.Type == "media_purged" {
					tombstones++
				}
			}
			if tombstones != 1 {
				t.Fatalf("%s: %d purge tombstones, want exactly 1", point, tombstones)
			}
			h.assertReconciled(point, crashResidueKinds)
			h.assertResidue(point, prev, residueNone, nil)
			h.record("failpoint", map[string]any{"point": point, "purge_job_id": purge.ID})
		})
	}
}
