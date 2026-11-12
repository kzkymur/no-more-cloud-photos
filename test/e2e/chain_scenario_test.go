//go:build !nmcp_failpoints_e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestReleaseGateChain is the single ordered #21 L1 scenario.
func TestReleaseGateChain(t *testing.T) {
	h := newHarness(t)
	var ready struct{ Status string }
	h.expectJSON(h.do(http.MethodGet, "/readyz", nil, nil), 200, &ready)
	h.activateBundledProfiles()

	data := fixtureImage(t, "jpeg", 2400, 1600)
	key := "e2e-jpeg-" + sha(data)[:16]
	var created struct {
		Media media `json:"media"`
		Job   *job  `json:"job"`
	}
	first := h.upload(key, "C:\\fakepath\\e2e.jpg", "application/octet-stream", data)
	h.expectJSON(first, 201, &created)
	if created.Job == nil || created.Media.MIMEType != "image/jpeg" || created.Media.OriginalFilename == nil || *created.Media.OriginalFilename != "e2e.jpg" {
		t.Fatalf("upload result: mime=%s job=%t", created.Media.MIMEType, created.Job != nil)
	}
	keys := map[string]bool{}
	for _, target := range created.Job.Targets {
		keys[target.Profile.Key] = true
	}
	if !keys["standard"] || !keys["thumbnail"] || created.Job.MaxAttempts != 3 {
		t.Fatalf("upload job targets %v max_attempts=%d", keys, created.Job.MaxAttempts)
	}
	mediaID := created.Media.ID
	h.record("upload", map[string]any{"media_id": mediaID, "job_id": created.Job.ID, "sha256": sha(data)})

	t.Run("idempotent replay and duplicate", func(t *testing.T) {
		replay := h.upload(key, "C:\\fakepath\\e2e.jpg", "image/jpeg", data)
		var again struct {
			Media media `json:"media"`
		}
		h.expectJSON(replay, 201, &again)
		if again.Media.ID != mediaID || replay.Header.Get("Idempotency-Replayed") != "true" {
			t.Fatalf("replay media=%s replayed=%q", again.Media.ID, replay.Header.Get("Idempotency-Replayed"))
		}
		var dup apiError
		h.expectJSON(h.upload(key+"-other", "x.jpg", "image/jpeg", data), 409, &dup)
		if dup.Error.Code != "duplicate_media" || dup.Error.Details["existing_media_id"] != mediaID {
			t.Fatalf("duplicate: code=%s", dup.Error.Code)
		}
	})

	done := h.waitJob(created.Job.ID, "succeeded", 10*time.Minute)
	for _, target := range done.Targets {
		if target.Status != "succeeded" || target.RenditionID == nil {
			t.Fatalf("target %s status=%s", target.Profile.Key, target.Status)
		}
	}

	m, status := h.getMedia(mediaID)
	if status != 200 {
		t.Fatalf("detail status %d", status)
	}
	current := currentByKey(m)
	standard, thumbnail := current["standard"], current["thumbnail"]
	if standard.ID == "" || thumbnail.ID == "" || standard.MIMEType != "image/avif" || thumbnail.MIMEType != "image/avif" {
		t.Fatalf("current renditions: %v", current)
	}
	// 2400x1600 source: long edge 1920, aspect preserved, no upscale.
	if standard.Width == nil || standard.Height == nil || *standard.Width != 1920 || *standard.Height != 1280 {
		t.Fatalf("standard rendition dimensions %v x %v", standard.Width, standard.Height)
	}
	if thumbnail.Width == nil || thumbnail.Height == nil || *thumbnail.Width != 640 || *thumbnail.Height != 427 && *thumbnail.Height != 426 {
		t.Fatalf("thumbnail rendition dimensions %v x %v", thumbnail.Width, thumbnail.Height)
	}

	t.Run("list and detail", func(t *testing.T) {
		listed := h.listIDs("profile=standard")
		if item, ok := listed[mediaID]; !ok || item.Rendition == nil || item.Rendition.ID != standard.ID {
			t.Fatal("default list lacks media with current standard rendition")
		}
		thumbs := h.listIDs("profile=thumbnail")
		if item, ok := thumbs[mediaID]; !ok || item.Rendition == nil || item.Rendition.ID != thumbnail.ID {
			t.Fatal("thumbnail list lacks current thumbnail rendition")
		}
		var display rendition
		h.expectJSON(h.do(http.MethodGet, "/media/"+mediaID+"/display", nil, nil), 200, &display)
		if display.ID != standard.ID {
			t.Fatal("display is not current standard")
		}
	})

	orig := h.original(mediaID)
	t.Run("URL bytes through Nginx", func(t *testing.T) {
		if orig.SHA256 != sha(data) {
			t.Fatal("original digest differs from uploaded bytes")
		}
		h.assertFileBytes(orig.FileURL, sha(data), "image/jpeg", int64(len(data)))
		h.mediaTreeHas(orig.FileURL, sha(data))
		for _, r := range []rendition{standard, thumbnail} {
			h.assertFileBytes(r.FileURL, r.SHA256, r.MIMEType, r.SizeBytes)
			h.mediaTreeHas(r.FileURL, r.SHA256)
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			if r := h.fetchFile(method, orig.FileURL, nil); r.Status != 405 {
				t.Fatalf("%s on file URL: %d", method, r.Status)
			}
		}
	})

	t.Run("delete restore purge", func(t *testing.T) {
		var deleted media
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID, nil, nil), 200, &deleted)
		if deleted.DeletedAt == nil {
			t.Fatal("delete did not set deleted_at")
		}
		var again media
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID, nil, nil), 200, &again)
		if again.DeletedAt == nil || *again.DeletedAt != *deleted.DeletedAt {
			t.Fatal("repeated delete recalculated deleted_at")
		}
		if _, ok := h.listIDs("profile=standard")[mediaID]; ok {
			t.Fatal("deleted media in default list")
		}
		if _, ok := h.listIDs("profile=standard&deleted=only")[mediaID]; !ok {
			t.Fatal("deleted media missing from deleted=only")
		}
		h.assertFileBytes(orig.FileURL, sha(data), "image/jpeg", int64(len(data))) // retained until purge
		var restored media
		h.expectJSON(h.do(http.MethodPost, "/media/"+mediaID+"/restore", nil, nil), 200, &restored)
		if restored.DeletedAt != nil || restored.PurgeAfter != nil {
			t.Fatal("restore did not clear deletion")
		}
		var notDeleted apiError
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID+"/purge", nil, nil), 409, &notDeleted)
		if notDeleted.Error.Code != "media_not_deleted" {
			t.Fatalf("purge of active media: %s", notDeleted.Error.Code)
		}
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID, nil, nil), 200, nil)
		var purge job
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID+"/purge", nil, nil), 202, &purge)
		var same job
		h.expectJSON(h.do(http.MethodDelete, "/media/"+mediaID+"/purge", nil, nil), 202, &same)
		if purge.Type != "purge" || same.ID != purge.ID {
			t.Fatal("purge enqueue not idempotent")
		}
		h.waitJob(purge.ID, "succeeded", 5*time.Minute)
		if _, status := h.getMedia(mediaID); status != 404 {
			t.Fatalf("purged media detail status %d", status)
		}
		for _, url := range []string{orig.FileURL, standard.FileURL, thumbnail.FileURL} {
			h.assertFileGone(url)
		}
		h.mediaTreeLacks(orig.ID)
		var jobs struct {
			Items []job `json:"items"`
		}
		h.expectJSON(h.do(http.MethodGet, "/jobs?media_id="+mediaID, nil, nil), 200, &jobs)
		if len(jobs.Items) < 2 {
			t.Fatal("job history not retained after purge")
		}
	})

	t.Run("change feed", func(t *testing.T) {
		kinds := strings.Join(changeKinds(h.changesFor(mediaID)), ",")
		for _, want := range []string{"media_upsert:upload", "media_upsert:rendition_current", "media_deleted:logical_delete", "media_upsert:restore", "media_purged:physical_purge"} {
			if !strings.Contains(kinds, want) {
				t.Fatalf("change feed for media lacks %s: %s", want, kinds)
			}
		}
		if !strings.HasSuffix(kinds, "media_purged:physical_purge") {
			t.Fatalf("tombstone is not last: %s", kinds)
		}
		h.record("changes", kinds)
	})

	t.Run("regenerate current cleanup", func(t *testing.T) {
		pngData := fixtureImage(t, "png", 1200, 900)
		var up struct {
			Media media `json:"media"`
			Job   *job  `json:"job"`
		}
		h.expectJSON(h.upload("e2e-png-"+sha(pngData)[:16], "e2e.png", "image/png", pngData), 201, &up)
		h.waitJob(up.Job.ID, "succeeded", 10*time.Minute)
		before, _ := h.getMedia(up.Media.ID)
		old := currentByKey(before)["standard"]
		// regenerate brings media up to a NEWLY active profile version: it selects
		// only media with no target for the active version. Publish standard/v2
		// with v1's exact recipes; the append-only certification registry already
		// covers every requirement, so activation needs no new certification.
		// A superseded rendition's purge_after is fixed in the publication
		// transaction from the retention configured at that moment (null =
		// never automatically), so the zero-day retention must be in force
		// before regeneration supersedes the old rendition.
		h.admin("config", "set", "--name", "superseded_rendition_retention_days", "--value", "0", "--json")
		defer h.admin("config", "set", "--name", "superseded_rendition_retention_days", "--value", "null", "--json")
		h.activateStandardV2()
		var regen struct {
			Status    string `json:"status"`
			Total     int64  `json:"total"`
			Succeeded int64  `json:"succeeded"`
			Failed    int64  `json:"failed"`
		}
		out := h.admin("regenerate", "--profile", "standard", "--media-id", up.Media.ID, "--yes", "--json")
		if err := json.Unmarshal(out, &regen); err != nil || regen.Total != 1 || regen.Succeeded != 1 || regen.Failed != 0 {
			t.Fatalf("regenerate batch: status=%s total=%d succeeded=%d failed=%d err=%v", regen.Status, regen.Total, regen.Succeeded, regen.Failed, err)
		}
		h.eventually("regenerated standard current", 10*time.Minute, func() bool {
			m, _ := h.getMedia(up.Media.ID)
			r := currentByKey(m)["standard"]
			return r.ID != "" && r.ID != old.ID
		})
		after, _ := h.getMedia(up.Media.ID)
		fresh := currentByKey(after)["standard"]
		if fresh.FileURL == old.FileURL {
			t.Fatal("regeneration reused the old URL")
		}
		h.assertFileBytes(fresh.FileURL, fresh.SHA256, fresh.MIMEType, fresh.SizeBytes)
		h.expectJSON(h.do(http.MethodGet, "/renditions/"+old.ID, nil, nil), 200, nil) // superseded, retained until cleanup
		// Zero-day retention means "due at the next scan": the Worker's
		// automatic rendition cleanup sweeps every 30 s in production. (The
		// manual `cleanup` CLI cannot delete here: the harness gives the admin
		// one-shot a read-only media view by design.)
		h.eventually("superseded rendition cleanup by the Worker's automatic sweep", 3*time.Minute, func() bool {
			return h.do(http.MethodGet, "/renditions/"+old.ID, nil, nil).Status == 404
		})
		h.assertFileGone(old.FileURL)
		h.assertFileBytes(fresh.FileURL, fresh.SHA256, fresh.MIMEType, fresh.SizeBytes) // current never eligible
		h.record("regenerate", map[string]any{"media_id": up.Media.ID, "old": old.ID, "new": fresh.ID})
	})
}
