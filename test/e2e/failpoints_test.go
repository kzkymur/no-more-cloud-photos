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
		point  string
		replay bool // the acceptance committed before the crash
	}{
		{"upload.before_original_publish", false},
		{"upload.after_original_publish", false},
		{"upload.after_db_commit", true},
	} {
		t.Run(c.point, func(t *testing.T) {
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
			h.record("failpoint", map[string]any{"point": c.point, "media_id": created.Media.ID, "replayed": c.replay})
		})
	}

	// Rendition publish points: the Worker dies at the boundary of the first
	// target; after the lease expires a clean Worker reclaims and finishes.
	for _, point := range []string{"rendition.before_file_publish", "rendition.after_file_publish", "rendition.after_db_commit"} {
		t.Run(point, func(t *testing.T) {
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
			h.record("failpoint", map[string]any{"point": point, "job_id": up.Job.ID, "attempts": done.Attempts})
		})
	}

	// Purge points: the Worker dies inside purge; the reclaimed purge must
	// converge to no Media, no files, a retained job and one tombstone.
	for _, point := range []string{"purge.after_start", "purge.after_file_delete", "purge.before_finalize", "purge.after_finalize", "purge.after_file_step"} {
		t.Run(point, func(t *testing.T) {
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
			h.record("failpoint", map[string]any{"point": point, "purge_job_id": purge.ID})
		})
	}
}
