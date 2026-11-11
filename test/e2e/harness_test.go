// Package e2e is the #21 release-gate scenario. It runs only inside the
// infra Compose E2E lane (scripts/infra-e2e-env run), against the real API,
// Worker with pinned codec helpers, PostgreSQL 17 and HTTPS Nginx. Outside
// that lane every test skips; inside it the lane fails closed on any SKIP.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t        *testing.T
	api      string
	files    string
	control  string
	artifact string
	deadline time.Time
	apiHTTP  *http.Client
	fileHTTP *http.Client
}

// newHarness reads the infra contract environment. Missing environment means
// "not in the E2E lane": skip, never pass vacuously.
func newHarness(t *testing.T) *harness {
	t.Helper()
	api := os.Getenv("NMCP_E2E_API_URL")
	if api == "" {
		t.Skip("NMCP_E2E_API_URL unset: #21 scenario runs only in the infra Compose E2E lane")
	}
	h := &harness{t: t, api: strings.TrimRight(api, "/"), files: strings.TrimRight(os.Getenv("NMCP_E2E_FILES_URL"), "/"),
		control: os.Getenv("NMCP_E2E_CONTROL"), artifact: os.Getenv("NMCP_E2E_ARTIFACT_DIR")}
	for name, value := range map[string]string{"NMCP_E2E_FILES_URL": h.files, "NMCP_E2E_CONTROL": h.control, "NMCP_E2E_ARTIFACT_DIR": h.artifact} {
		if value == "" {
			t.Fatalf("incomplete E2E contract: %s unset", name)
		}
	}
	epoch, err := strconv.ParseInt(os.Getenv("NMCP_E2E_DEADLINE"), 10, 64)
	if err != nil || epoch <= time.Now().Unix() {
		t.Fatal("invalid or expired NMCP_E2E_DEADLINE")
	}
	h.deadline = time.Unix(epoch, 0)
	pem, err := os.ReadFile(os.Getenv("NMCP_E2E_FILES_CA_FILE"))
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		t.Fatal("NMCP_E2E_FILES_CA_FILE is not a readable PEM certificate")
	}
	// No proxy, explicit trust only in the harness CA, bounded per request.
	h.apiHTTP = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil}}
	h.fileHTTP = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return h
}

// ctx is bounded by the harness deadline minus a margin for teardown logging.
func (h *harness) ctx() (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), h.deadline.Add(-30*time.Second))
}

// run executes one control-CLI command. The CLI returns 0 only after the
// requested effect is observed.
func (h *harness) run(stdin string, args ...string) string {
	h.t.Helper()
	ctx, cancel := h.ctx()
	defer cancel()
	cmd := exec.CommandContext(ctx, h.control, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("control %s failed: %v: %s", strings.Join(args, " "), err, lastLine(stderr.String()))
	}
	return stdout.String()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// dbRead runs one read-only query and returns trimmed psql -At output.
func (h *harness) dbRead(sql string) string {
	h.t.Helper()
	return strings.TrimSpace(h.run(sql, "db-read"))
}

func (h *harness) admin(args ...string) []byte {
	h.t.Helper()
	return []byte(h.run("", append([]string{"admin", "--"}, args...)...))
}

type mediaEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

func (h *harness) mediaTree() map[string]mediaEntry {
	h.t.Helper()
	out := map[string]mediaEntry{}
	for _, line := range strings.Split(strings.TrimSpace(h.run("", "media-ls")), "\n") {
		if line == "" {
			continue
		}
		var e mediaEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			h.t.Fatalf("media-ls line not JSON: %v", err)
		}
		out[e.Path] = e
	}
	return out
}

// record appends one evidence line to the uploaded artifact.
func (h *harness) record(kind string, value any) {
	h.t.Helper()
	line, err := json.Marshal(map[string]any{"kind": kind, "at": time.Now().UTC(), "value": value})
	if err != nil {
		h.t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(h.artifact, "e2e-evidence.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (h *harness) do(method, path string, body io.Reader, header map[string]string) apiResponse {
	h.t.Helper()
	ctx, cancel := h.ctx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.api+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := h.apiHTTP.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		h.t.Fatalf("%s %s body: %v", method, path, err)
	}
	return apiResponse{resp.StatusCode, resp.Header, data}
}

// expectJSON asserts the status and decodes the body.
func (h *harness) expectJSON(r apiResponse, status int, out any) {
	h.t.Helper()
	if r.Status != status {
		h.t.Fatalf("status %d, want %d: %s", r.Status, status, truncate(r.Body))
	}
	if out != nil {
		if err := json.Unmarshal(r.Body, out); err != nil {
			h.t.Fatalf("decode %T: %v", out, err)
		}
	}
}

func truncate(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return string(b)
}

// fetchFile GETs a File Server URL through Nginx with the harness CA.
func (h *harness) fetchFile(method, url string, header map[string]string) apiResponse {
	h.t.Helper()
	if !strings.HasPrefix(url, h.files+"/") {
		h.t.Fatalf("file URL %q is not under the configured File Server root", url)
	}
	ctx, cancel := h.ctx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := h.fileHTTP.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		h.t.Fatal(err)
	}
	return apiResponse{resp.StatusCode, resp.Header, data}
}

// eventually polls until ok returns true or the context ends.
func (h *harness) eventually(what string, limit time.Duration, ok func() bool) {
	h.t.Helper()
	end := time.Now().Add(limit)
	for {
		if ok() {
			return
		}
		if time.Now().After(end) || time.Now().After(h.deadline.Add(-30*time.Second)) {
			h.t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

var _ = fmt.Sprintf
