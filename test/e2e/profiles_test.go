package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fixed production-shaped helper locations inside the core image. Their
// expected digests come from a separate one-shot container of the same image
// (control image-sha256), never from the admin container that certifies.
var certifyHelpers = map[string]string{
	"still_helper":     "/opt/nmcp/bin/nmcp-still-helper",
	"animation_helper": "/opt/nmcp/bin/nmcp-animation-helper",
	"video_helper":     "/opt/nmcp/bin/nmcp-video-helper",
	"ffprobe":          "/opt/nmcp/bin/ffprobe",
	"prlimit":          "/usr/bin/prlimit",
	"srgb_icc":         "/opt/nmcp/share/sRGB2014.icc",
}

const (
	standardProfileID  = "60000000-0000-4000-8000-000000000001"
	thumbnailProfileID = "60000000-0000-4000-8000-000000000002"
)

type preparedFixture struct {
	Name          string `json:"name"`
	MIMEType      string `json:"mime_type"`
	File          string `json:"file"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	Certification bool   `json:"certification"`
}

func (h *harness) preparedFixtures() []preparedFixture {
	h.t.Helper()
	dir := os.Getenv("NMCP_E2E_FIXTURES_DIR")
	if dir == "" {
		h.t.Fatal("NMCP_E2E_FIXTURES_DIR unset")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "prepared", "prepared.json"))
	if err != nil {
		h.t.Fatal("prepared fixture manifest missing: run scripts/prepare-e2e-fixtures --out $NMCP_E2E_FIXTURES_DIR/prepared")
	}
	var out []preparedFixture
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		h.t.Fatalf("prepared fixture manifest invalid: %v", err)
	}
	return out
}

// runResult executes one control command without failing the test, so a
// refused certification can report its closed reason.
func (h *harness) runResult(stdin string, args ...string) (string, string, error) {
	ctx, cancel := h.ctx()
	defer cancel()
	cmd := exec.CommandContext(ctx, h.control, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (h *harness) helperDigests() map[string]string {
	h.t.Helper()
	paths := make([]string, 0, len(certifyHelpers))
	for _, p := range certifyHelpers {
		paths = append(paths, p)
	}
	out := h.run("", append([]string{"image-sha256"}, paths...)...)
	byPath := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			h.t.Fatalf("image-sha256 line malformed: %q", line)
		}
		byPath[fields[1]] = fields[0]
	}
	digests := map[string]string{}
	for name, p := range certifyHelpers {
		if byPath[p] == "" {
			h.t.Fatalf("image-sha256 lacks %s", p)
		}
		digests[name] = byPath[p]
	}
	return digests
}

func certifySpec(profileID string, digests map[string]string, fixtures []preparedFixture) []byte {
	helpers := map[string]map[string]string{}
	for name, p := range certifyHelpers {
		helpers[name] = map[string]string{"path": p, "sha256": digests[name]}
	}
	var items []map[string]string
	seen := map[string]bool{}
	for _, f := range fixtures {
		key := f.MIMEType + "/" + f.SHA256
		if !f.Certification || seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, map[string]string{"mime_type": f.MIMEType, "path": "/e2e/fixtures/prepared/" + f.File, "sha256": f.SHA256})
	}
	spec, _ := json.Marshal(map[string]any{"profile_id": profileID, "helpers": helpers, "fixtures": items})
	return spec
}

// activateBundledProfiles certifies the bundled thumbnail/v1 and standard/v1
// drafts on this stack's exact helpers through `nmcp-admin profile certify`
// (real transforms, independent probes, one audited transaction), then
// activates them through `nmcp-admin profile activate`.
func (h *harness) activateBundledProfiles() {
	h.t.Helper()
	fixtures := h.preparedFixtures()
	digests := h.helperDigests()
	h.record("helper_digests", digests)
	for _, id := range []string{thumbnailProfileID, standardProfileID} {
		stdout, stderr, err := h.runResult(string(certifySpec(id, digests, fixtures)), "admin", "--", "profile", "certify", "--file", "-", "--json")
		var result struct {
			Status             string `json:"status"`
			ProfileStatus      string `json:"profile_status"`
			ActivationVerified bool   `json:"activation_certification_verified"`
			Certifications     []struct {
				ID       string `json:"id"`
				MIMEType string `json:"mime_type"`
			} `json:"certifications"`
		}
		decodeErr := json.Unmarshal([]byte(stdout), &result)
		h.record("profile_certify", map[string]any{"profile_id": id, "exit_error": errText(err), "result": json.RawMessage(nonEmptyJSON(stdout)), "stderr_tail": lastLine(stderr)})
		if err != nil || decodeErr != nil || !result.ActivationVerified || len(result.Certifications) == 0 {
			h.t.Fatalf("certify %s failed: err=%v result=%s stderr=%s", id, err, truncate([]byte(stdout)), lastLine(stderr))
		}
		h.admin("profile", "activate", "--id", id, "--json")
	}
	var profiles struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	h.expectJSON(h.do("GET", "/profiles?status=active", nil, nil), 200, &profiles)
	active := map[string]bool{}
	for _, p := range profiles.Items {
		active[p.ID] = p.Status == "active"
	}
	if !active[standardProfileID] || !active[thumbnailProfileID] {
		h.t.Fatalf("bundled profiles not active after certification: %v", active)
	}
}

func errText(err error) string {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &exit):
		return exit.String()
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return "error"
}

func nonEmptyJSON(s string) string {
	if json.Valid([]byte(strings.TrimSpace(s))) && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return "null"
}
