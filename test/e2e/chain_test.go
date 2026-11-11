package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

type rendition struct {
	ID          string `json:"id"`
	MediaID     string `json:"media_id"`
	JobTargetID string `json:"job_target_id"`
	Profile     struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Version int    `json:"version"`
	} `json:"profile"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Width     *int   `json:"width"`
	Height    *int   `json:"height"`
	SHA256    string `json:"sha256"`
	FileURL   string `json:"file_url"`
}

type jobTarget struct {
	ID      string `json:"id"`
	Profile struct {
		Key string `json:"key"`
	} `json:"profile"`
	Status      string  `json:"status"`
	RenditionID *string `json:"rendition_id"`
}

type job struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	Status      string      `json:"status"`
	MediaID     string      `json:"media_id"`
	Attempts    int         `json:"attempts"`
	MaxAttempts int         `json:"max_attempts"`
	StartedAt   *string     `json:"started_at"`
	Error       *struct{}   `json:"error"`
	Targets     []jobTarget `json:"targets"`
}

type media struct {
	ID                string      `json:"id"`
	MIMEType          string      `json:"mime_type"`
	OriginalFilename  *string     `json:"original_filename"`
	SizeBytes         int64       `json:"size_bytes"`
	DeletedAt         *string     `json:"deleted_at"`
	PurgeAfter        *string     `json:"purge_after"`
	Rendition         *rendition  `json:"rendition"`
	CurrentRenditions []rendition `json:"current_renditions"`
	Jobs              []job       `json:"jobs"`
}

type original struct {
	ID        string `json:"id"`
	MediaID   string `json:"media_id"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	FileURL   string `json:"file_url"`
}

type change struct {
	ID       string          `json:"id"`
	Position string          `json:"position"`
	Type     string          `json:"type"`
	MediaID  string          `json:"media_id"`
	Media    json.RawMessage `json:"media"`
	Reason   *string         `json:"reason"`
}

type apiError struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// fixtureImage generates a unique, deterministic-size image so every run
// uploads new content (SHA uniqueness) without committed binary fixtures.
func fixtureImage(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	var salt [8]byte
	if _, err := rand.Read(salt[:]); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x) ^ salt[x%8], G: uint8(y) ^ salt[y%8], B: salt[(x+y)%8], A: 255})
		}
	}
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	default:
		t.Fatalf("unknown fixture format %s", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (h *harness) upload(key, filename, contentType string, data []byte) apiResponse {
	h.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		h.t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPost, "/media", &body, map[string]string{"Content-Type": w.FormDataContentType(), "Idempotency-Key": key})
}

func (h *harness) getJob(id string) job {
	h.t.Helper()
	var j job
	h.expectJSON(h.do(http.MethodGet, "/jobs/"+id, nil, nil), 200, &j)
	return j
}

func (h *harness) waitJob(id string, want string, limit time.Duration) job {
	h.t.Helper()
	var j job
	h.eventually("job "+id+" "+want, limit, func() bool {
		j = h.getJob(id)
		if j.Status == "failed" && want != "failed" {
			h.t.Fatalf("job %s failed: %+v", id, j)
		}
		return j.Status == want
	})
	return j
}

func (h *harness) getMedia(id string) (media, int) {
	h.t.Helper()
	r := h.do(http.MethodGet, "/media/"+id, nil, nil)
	var m media
	if r.Status == 200 {
		h.expectJSON(r, 200, &m)
	}
	return m, r.Status
}

func (h *harness) listIDs(query string) map[string]media {
	h.t.Helper()
	out := map[string]media{}
	cursor := ""
	for page := 0; page < 100; page++ {
		path := "/media?" + query
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var resp struct {
			Items      []media `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		h.expectJSON(h.do(http.MethodGet, path, nil, nil), 200, &resp)
		for _, m := range resp.Items {
			out[m.ID] = m
		}
		if resp.NextCursor == nil {
			return out
		}
		cursor = *resp.NextCursor
	}
	h.t.Fatal("media list did not terminate")
	return nil
}

func (h *harness) changesFor(mediaID string) []change {
	h.t.Helper()
	var out []change
	cursor := ""
	for page := 0; page < 1000; page++ {
		path := "/changes?limit=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var resp struct {
			Items      []change `json:"items"`
			NextCursor *string  `json:"next_cursor"`
		}
		h.expectJSON(h.do(http.MethodGet, path, nil, nil), 200, &resp)
		for _, c := range resp.Items {
			if c.MediaID == mediaID {
				out = append(out, c)
			}
		}
		if resp.NextCursor == nil || len(resp.Items) == 0 {
			return out
		}
		cursor = *resp.NextCursor
	}
	h.t.Fatal("change feed did not terminate")
	return nil
}

func changeKinds(cs []change) []string {
	var out []string
	last := int64(0)
	for _, c := range cs {
		pos, err := strconv.ParseInt(c.Position, 10, 64)
		if err != nil || pos <= last {
			return []string{"non-monotonic-position"}
		}
		last = pos
		reason := ""
		if c.Reason != nil {
			reason = *c.Reason
		}
		out = append(out, c.Type+":"+reason)
	}
	return out
}

// assertFileBytes fetches a File Server URL through Nginx and proves exact
// bytes, HEAD, a single byte range, and the immutable header contract.
func (h *harness) assertFileBytes(url, wantSHA, wantMIME string, wantSize int64) {
	h.t.Helper()
	got := h.fetchFile(http.MethodGet, url, nil)
	if got.Status != 200 || sha(got.Body) != wantSHA || int64(len(got.Body)) != wantSize {
		h.t.Fatalf("GET %s: status=%d size=%d sha_match=%t", url, got.Status, len(got.Body), sha(got.Body) == wantSHA)
	}
	for header, want := range map[string]string{"Content-Type": wantMIME, "X-Content-Type-Options": "nosniff", "Cache-Control": "private, max-age=31536000, immutable", "Accept-Ranges": "bytes"} {
		if v := got.Header.Get(header); v != want {
			h.t.Fatalf("GET %s header %s=%q want %q", url, header, v, want)
		}
	}
	head := h.fetchFile(http.MethodHead, url, nil)
	if head.Status != 200 || len(head.Body) != 0 || head.Header.Get("Content-Length") != strconv.FormatInt(wantSize, 10) {
		h.t.Fatalf("HEAD %s: status=%d length=%q", url, head.Status, head.Header.Get("Content-Length"))
	}
	ranged := h.fetchFile(http.MethodGet, url, map[string]string{"Range": "bytes=1-8"})
	if ranged.Status != 206 || !bytes.Equal(ranged.Body, got.Body[1:9]) {
		h.t.Fatalf("Range %s: status=%d", url, ranged.Status)
	}
	if beyond := h.fetchFile(http.MethodGet, url, map[string]string{"Range": fmt.Sprintf("bytes=%d-", wantSize+10)}); beyond.Status != 416 {
		h.t.Fatalf("unsatisfiable Range %s: status=%d", url, beyond.Status)
	}
}

func (h *harness) assertFileGone(url string) {
	h.t.Helper()
	if r := h.fetchFile(http.MethodGet, url, nil); r.Status != 404 {
		h.t.Fatalf("purged/cleaned file %s still served: status=%d", url, r.Status)
	}
}

// mediaTreeHas proves the published file exists on the media volume with the
// API-reported digest (the File Server is not the only witness).
func (h *harness) mediaTreeHas(url, wantSHA string) {
	h.t.Helper()
	key := strings.TrimPrefix(url, h.files+"/")
	tree := h.mediaTree()
	for path, e := range tree {
		if strings.HasSuffix(path, key) {
			if e.Type != "file" || e.SHA256 != wantSHA {
				h.t.Fatalf("media volume %s: type=%s sha_match=%t", key, e.Type, e.SHA256 == wantSHA)
			}
			return
		}
	}
	h.t.Fatalf("media volume lacks published key %s", key)
}

func (h *harness) mediaTreeLacks(fragment string) {
	h.t.Helper()
	for path := range h.mediaTree() {
		if strings.Contains(path, fragment) {
			h.t.Fatalf("media volume still contains %s", path)
		}
	}
}

func (h *harness) original(mediaID string) original {
	h.t.Helper()
	var o original
	h.expectJSON(h.do(http.MethodGet, "/media/"+mediaID+"/original", nil, nil), 200, &o)
	return o
}

func currentByKey(m media) map[string]rendition {
	out := map[string]rendition{}
	for _, r := range m.CurrentRenditions {
		out[r.Profile.Key] = r
	}
	return out
}

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
		h.admin("config", "set", "--name", "superseded_rendition_retention_days", "--value", "0", "--json")
		h.admin("cleanup", "--kind", "renditions", "--yes", "--json")
		h.eventually("superseded rendition cleanup", 2*time.Minute, func() bool {
			return h.do(http.MethodGet, "/renditions/"+old.ID, nil, nil).Status == 404
		})
		h.assertFileGone(old.FileURL)
		h.assertFileBytes(fresh.FileURL, fresh.SHA256, fresh.MIMEType, fresh.SizeBytes) // current never eligible
		h.record("regenerate", map[string]any{"media_id": up.Media.ID, "old": old.ID, "new": fresh.ID})
	})
}
