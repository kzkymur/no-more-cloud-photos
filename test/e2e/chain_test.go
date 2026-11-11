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
