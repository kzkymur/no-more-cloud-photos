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

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
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

// bundledProfilesActive records which of this stack's bundled profiles are
// certified and active. Tests in one binary share one stack and database, and
// certify is append-only (an identical second certification exits 4), so the
// first test to reach a profile performs it and later tests reuse it.
var bundledProfilesActive = map[string]bool{}

// activateBundledProfiles certifies the bundled thumbnail/v1 and standard/v1
// drafts on this stack's exact helpers through `nmcp-admin profile certify`
// (real transforms, independent probes, one audited transaction), then
// activates them through `nmcp-admin profile activate`.
func (h *harness) activateBundledProfiles() {
	h.t.Helper()
	fixtures := h.preparedFixtures()
	digests := h.helperDigests()
	h.record("helper_digests", digests)
	activated := false
	defer func() {
		// The Worker binds its claimable profiles to the capability envelope it
		// validates at startup, so a newly activated profile is claimed only
		// after a restart: the documented operator step after activation.
		if activated && !h.t.Failed() {
			h.run("", "control", "restart", "worker")
		}
	}()
	for _, id := range []string{thumbnailProfileID, standardProfileID} {
		if bundledProfilesActive[id] {
			continue
		}
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
		bundledProfilesActive[id] = true
		activated = true
	}
	// Exactly one active version per bundled key: later scenario steps may
	// legitimately activate a newer version (standard/v2), which retires v1.
	var profiles struct {
		Items []struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"items"`
	}
	h.expectJSON(h.do("GET", "/profiles?status=active", nil, nil), 200, &profiles)
	activeByKey := map[string][]string{}
	for _, p := range profiles.Items {
		if p.Status == "active" {
			activeByKey[p.Key] = append(activeByKey[p.Key], p.ID)
		}
	}
	if len(activeByKey["standard"]) != 1 || len(activeByKey["thumbnail"]) != 1 {
		h.t.Fatalf("bundled profile keys not exactly once active after certification: %v", activeByKey)
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

// standardV2ProfileID is the scenario's second standard version.
const standardV2ProfileID = "60000000-0000-4000-8000-000000000021"

// activateStandardV2 creates standard/v2 with standard/v1's exact parameters,
// activates it (version order and certification coverage are enforced by
// nmcp-admin), and restarts the Worker so it claims the new version.
func (h *harness) activateStandardV2() {
	h.t.Helper()
	if bundledProfilesActive[standardV2ProfileID] {
		return
	}
	v1 := profile.StandardV1()
	document, err := json.Marshal(map[string]any{
		"id": standardV2ProfileID, "key": v1.Key, "version": v1.Version + 1, "input_mime_types": v1.InputMIMETypes,
		"processor": v1.Processor, "parameters_schema_version": v1.ParametersSchemaVersion, "parameters": v1.Parameters,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if stdout, stderr, err := h.runResult(string(document), "admin", "--", "profile", "create", "--file", "/dev/stdin", "--json"); err != nil {
		h.t.Fatalf("create standard/v2: err=%v result=%s stderr=%s", err, truncate([]byte(stdout)), lastLine(stderr))
	}
	if stdout, stderr, err := h.runResult("", "admin", "--", "profile", "activate", "--id", standardV2ProfileID, "--json"); err != nil {
		h.t.Fatalf("activate standard/v2 on the existing certification registry: err=%v result=%s stderr=%s", err, truncate([]byte(stdout)), lastLine(stderr))
	}
	bundledProfilesActive[standardV2ProfileID] = true
	h.run("", "control", "restart", "worker")
}
