package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func certifyDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type certifyTestEnv struct {
	dir      string
	spec     CertifySpec
	contents map[string][]byte
}

// newCertifyTestEnv writes self-owned 0755 helpers, an ICC whose digest the
// test pins as the required one, and the given fixtures.
func newCertifyTestEnv(t *testing.T, fixtures map[string][]string, helperVariant ...string) *certifyTestEnv {
	t.Helper()
	dir := t.TempDir()
	env := &certifyTestEnv{dir: dir, contents: map[string][]byte{}}
	write := func(name string, data []byte, mode os.FileMode) CertifyFileRef {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		env.contents[path] = data
		return CertifyFileRef{Path: path, SHA256: certifyDigest(data)}
	}
	icc := write("sRGB.icc", []byte("test icc bytes"), 0o644)
	previous := certifyRequiredICCSHA256
	certifyRequiredICCSHA256 = icc.SHA256
	t.Cleanup(func() { certifyRequiredICCSHA256 = previous })
	env.spec = CertifySpec{ProfileID: newID(), Helpers: CertifyHelpers{
		StillHelper: write("still", []byte("#!still"+strings.Join(helperVariant, "")), 0o755), AnimationHelper: write("animation", []byte("#!animation"), 0o755),
		VideoHelper: write("video", []byte("#!video"), 0o755), Prlimit: write("prlimit", []byte("#!prlimit"), 0o755),
		FFProbe: write("ffprobe", []byte("#!ffprobe"), 0o755), SRGBICC: icc,
	}}
	index := 0
	for mime, names := range fixtures {
		for _, name := range names {
			index++
			ref := write(fmt.Sprintf("fixture-%02d", index), []byte(mime+"/"+name), 0o644)
			env.spec.Fixtures = append(env.spec.Fixtures, CertifyFixture{MIMEType: mime, Path: ref.Path, SHA256: ref.SHA256})
		}
	}
	return env
}

func (env *certifyTestEnv) encode(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(env.spec)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCertifySpecStrictDecoding(t *testing.T) {
	env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}})
	valid := env.encode(t)
	spec, err := DecodeCertifySpec(valid)
	if err != nil || spec.ProfileID != env.spec.ProfileID || spec.digest != certifyDigest(valid) {
		t.Fatalf("valid spec refused: %v", err)
	}
	mutate := func(edit func(map[string]any)) []byte {
		var document map[string]any
		if err := json.Unmarshal(valid, &document); err != nil {
			t.Fatal(err)
		}
		edit(document)
		data, _ := json.Marshal(document)
		return data
	}
	helpers := func(d map[string]any) map[string]any { return d["helpers"].(map[string]any) }
	fixtures := func(d map[string]any) []any { return d["fixtures"].([]any) }
	text := string(valid)
	for name, data := range map[string][]byte{
		"empty":                 nil,
		"null":                  []byte("null"),
		"trailing value":        append(append([]byte{}, valid...), []byte(` {}`)...),
		"trailing garbage":      append(append([]byte{}, valid...), []byte(`x`)...),
		"oversize":              append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), certifySpecMaxBytes)...),
		"duplicate top key":     []byte(strings.Replace(text, `{"profile_id"`, `{"profile_id":"x","profile_id"`, 1)),
		"duplicate nested key":  []byte(strings.Replace(text, `"still_helper":{"path"`, `"still_helper":{"path":"/x","path"`, 1)),
		"unknown top field":     mutate(func(d map[string]any) { d["evidence"] = "trust me" }),
		"unknown helper":        mutate(func(d map[string]any) { helpers(d)["ffmpeg"] = helpers(d)["ffprobe"] }),
		"unknown ref field":     mutate(func(d map[string]any) { helpers(d)["prlimit"].(map[string]any)["mode"] = "0755" }),
		"unknown fixture field": mutate(func(d map[string]any) { fixtures(d)[0].(map[string]any)["output_sha256"] = "x" }),
		"missing helper":        mutate(func(d map[string]any) { delete(helpers(d), "video_helper") }),
		"relative path":         mutate(func(d map[string]any) { helpers(d)["prlimit"].(map[string]any)["path"] = "bin/prlimit" }),
		"unclean path":          mutate(func(d map[string]any) { helpers(d)["prlimit"].(map[string]any)["path"] = "/usr/bin/../bin/prlimit" }),
		"uppercase sha": mutate(func(d map[string]any) {
			helpers(d)["prlimit"].(map[string]any)["sha256"] = strings.ToUpper(env.spec.Helpers.Prlimit.SHA256)
		}),
		"wrong icc digest":     mutate(func(d map[string]any) { helpers(d)["srgb_icc"].(map[string]any)["sha256"] = strings.Repeat("0", 64) }),
		"shared helper path":   mutate(func(d map[string]any) { helpers(d)["ffprobe"] = helpers(d)["prlimit"] }),
		"bad profile id":       mutate(func(d map[string]any) { d["profile_id"] = "standard" }),
		"no fixtures":          mutate(func(d map[string]any) { d["fixtures"] = []any{} }),
		"null fixtures":        mutate(func(d map[string]any) { d["fixtures"] = nil }),
		"unknown fixture MIME": mutate(func(d map[string]any) { fixtures(d)[0].(map[string]any)["mime_type"] = "image/*" }),
		"duplicate fixture":    mutate(func(d map[string]any) { d["fixtures"] = append(fixtures(d), fixtures(d)[0]) }),
		"fixture is helper":    mutate(func(d map[string]any) { fixtures(d)[0].(map[string]any)["path"] = env.spec.Helpers.Prlimit.Path }),
		"too many fixtures": mutate(func(d map[string]any) {
			var many []any
			for i := 0; i <= certifyMaxFixtures; i++ {
				many = append(many, map[string]any{"mime_type": "image/jpeg", "path": fmt.Sprintf("/f/%d", i), "sha256": fmt.Sprintf("%064x", i)})
			}
			d["fixtures"] = many
		}),
	} {
		if _, err := DecodeCertifySpec(data); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
	}
}

func TestCertifySpecFileAndStdinInput(t *testing.T) {
	env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}})
	valid := env.encode(t)
	data, err := readCertifySpec("-", bytes.NewReader(valid))
	if err != nil || !bytes.Equal(data, valid) {
		t.Fatalf("stdin spec refused: %v", err)
	}
	if _, err := readCertifySpec("-", bytes.NewReader(bytes.Repeat([]byte(" "), certifySpecMaxBytes+1))); !errors.Is(err, ErrValidation) {
		t.Fatal("oversize stdin accepted")
	}
	path := filepath.Join(env.dir, "spec.json")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readCertifySpec(path, nil); err != nil || !bytes.Equal(data, valid) {
		t.Fatalf("regular spec file refused: %v", err)
	}
	link := filepath.Join(env.dir, "spec-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(env.dir, "spec.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{link, env.dir, fifo, filepath.Join(env.dir, "absent"), ""} {
		if _, err := readCertifySpec(bad, nil); !errors.Is(err, ErrValidation) {
			t.Errorf("%q accepted", bad)
		}
	}

	previous := certifyStdin
	t.Cleanup(func() { certifyStdin = previous })
	certifyStdin = bytes.NewReader(valid)
	o, err := parse([]string{"profile", "certify", "--file", "-", "--json"})
	if err != nil || o.certifySpec.ProfileID != env.spec.ProfileID || o.certifySpec.digest != certifyDigest(valid) {
		t.Fatalf("stdin CLI spec refused: %v", err)
	}
	for _, input := range []string{`{"profile_id":"secret-marker"}`, string(valid) + `{"x":1}`, ``} {
		certifyStdin = strings.NewReader(input)
		var out, diagnostics bytes.Buffer
		code := Run(context.Background(), []string{"profile", "certify", "--file", "-", "--json"}, &out, &diagnostics)
		if code != 2 || strings.Contains(out.String()+diagnostics.String(), "secret-marker") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), diagnostics.String())
		}
		var document map[string]any
		if json.Unmarshal(out.Bytes(), &document) != nil || document["exit_code"] != float64(2) {
			t.Fatalf("not one JSON value: %q", out.String())
		}
	}
	for _, args := range [][]string{
		{"profile", "certify", "--json"},
		{"profile", "certify", "--file", link, "--json"},
		{"profile", "certify", "--file", path, "extra", "--json"},
		{"profile", "certify", "--file", path, "--id", newID(), "--json"},
	} {
		if code := Run(context.Background(), args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: code=%d", args, code)
		}
	}
}

type fakeStatInfo struct {
	mode os.FileMode
	stat *syscall.Stat_t
}

func (f fakeStatInfo) Name() string       { return "x" }
func (f fakeStatInfo) Size() int64        { return 1 }
func (f fakeStatInfo) Mode() os.FileMode  { return f.mode }
func (f fakeStatInfo) ModTime() time.Time { return time.Time{} }
func (f fakeStatInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeStatInfo) Sys() any           { return f.stat }

func TestCertifyTrustedFilePredicates(t *testing.T) {
	euid := 1000
	file := func(uid uint32, mode os.FileMode) fs.FileInfo {
		return fakeStatInfo{mode: mode, stat: &syscall.Stat_t{Uid: uid}}
	}
	for _, test := range []struct {
		info       fs.FileInfo
		executable bool
		ok         bool
	}{
		{file(0, 0o755), true, true}, {file(1000, 0o700), true, true}, {file(0, 0o644), false, true},
		{file(1001, 0o755), true, false}, {file(0, 0o775), true, false}, {file(0, 0o757), true, false},
		{file(0, 0o644), true, false}, {file(0, os.ModeSymlink|0o755), true, false}, {file(0, os.ModeDir|0o755), true, false},
	} {
		if certifyOwnerAcceptable(test.info, euid, test.executable) != test.ok {
			t.Errorf("file uid=%d mode=%v executable=%v", test.info.Sys().(*syscall.Stat_t).Uid, test.info.Mode(), test.executable)
		}
	}
	for _, test := range []struct {
		info fs.FileInfo
		ok   bool
	}{
		{file(0, os.ModeDir|0o755), true}, {file(1000, os.ModeDir|0o700), true}, {file(0, os.ModeDir|os.ModeSticky|0o777), true},
		{file(1001, os.ModeDir|0o755), false}, {file(0, os.ModeDir|0o777), false}, {file(1000, os.ModeDir|0o775), false}, {file(0, 0o755), false},
	} {
		if certifyDirectoryAcceptable(test.info, euid) != test.ok {
			t.Errorf("dir uid=%d mode=%v", test.info.Sys().(*syscall.Stat_t).Uid, test.info.Mode())
		}
	}
}

func TestCertifyWorkspaceRejectsTamperedEvidence(t *testing.T) {
	env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}})
	ws, err := prepareCertifyWorkspace(env.spec)
	if err != nil {
		t.Fatalf("authentic evidence refused: %v", err)
	}
	info, err := os.Stat(ws.dir)
	if err != nil || info.Mode().Perm() != 0o700 || len(ws.fixtures) != 2 {
		t.Fatal("private workspace is not exact")
	}
	for _, fixture := range ws.fixtures {
		data, err := os.ReadFile(fixture.path)
		if err != nil || certifyDigest(data) != fixture.sha256 || filepath.Dir(fixture.path) != ws.dir {
			t.Fatal("fixture copy is not bound to its digest inside the workspace")
		}
	}
	if err := ws.reverifyHelpers(); err != nil {
		t.Fatalf("unchanged helpers refused: %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workspace not removed")
	}

	// Each refusal names the spec input and the fixed check, never a path.
	wantRejection := map[string][2]string{
		"helper sha mismatch":          {"still_helper", "digest"},
		"helper content changed":       {"video_helper", "digest"},
		"fixture sha mismatch":         {"fixture[0]:image/jpeg", "digest"},
		"icc changed":                  {"srgb_icc", "digest"},
		"helper symlink":               {"prlimit", "open"},
		"fixture symlink":              {"fixture[0]:image/jpeg", "open"},
		"helper group writable":        {"animation_helper", "owner_mode"},
		"icc world writable":           {"srgb_icc", "owner_mode"},
		"helper not executable":        {"ffprobe", "owner_mode"},
		"helper in writable directory": {"ffprobe", "ancestor"},
		"fixture is a directory":       {"fixture[0]:image/jpeg", "type_size"},
		"fixture is a fifo":            {"fixture[0]:image/jpeg", "type_size"},
		"fixture missing":              {"fixture[0]:image/jpeg", "open"},
	}
	cases := map[string]func(t *testing.T, spec *CertifySpec){
		"helper sha mismatch": func(t *testing.T, spec *CertifySpec) {
			spec.Helpers.StillHelper.SHA256 = certifyDigest([]byte("other"))
		},
		"helper content changed": func(t *testing.T, spec *CertifySpec) {
			if err := os.WriteFile(spec.Helpers.VideoHelper.Path, []byte("#!evil"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"fixture sha mismatch": func(t *testing.T, spec *CertifySpec) {
			spec.Fixtures[0].SHA256 = certifyDigest([]byte("other"))
		},
		"icc changed": func(t *testing.T, spec *CertifySpec) {
			if err := os.WriteFile(spec.Helpers.SRGBICC.Path, []byte("other icc"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"helper symlink": func(t *testing.T, spec *CertifySpec) {
			link := spec.Helpers.Prlimit.Path + ".link"
			if err := os.Symlink(spec.Helpers.Prlimit.Path, link); err != nil {
				t.Fatal(err)
			}
			spec.Helpers.Prlimit.Path = link
		},
		"fixture symlink": func(t *testing.T, spec *CertifySpec) {
			link := spec.Fixtures[0].Path + ".link"
			if err := os.Symlink(spec.Fixtures[0].Path, link); err != nil {
				t.Fatal(err)
			}
			spec.Fixtures[0].Path = link
		},
		"helper group writable": func(t *testing.T, spec *CertifySpec) {
			if err := os.Chmod(spec.Helpers.AnimationHelper.Path, 0o775); err != nil {
				t.Fatal(err)
			}
		},
		"icc world writable": func(t *testing.T, spec *CertifySpec) {
			if err := os.Chmod(spec.Helpers.SRGBICC.Path, 0o646); err != nil {
				t.Fatal(err)
			}
		},
		"helper not executable": func(t *testing.T, spec *CertifySpec) {
			if err := os.Chmod(spec.Helpers.FFProbe.Path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"helper in writable directory": func(t *testing.T, spec *CertifySpec) {
			dir := filepath.Join(filepath.Dir(spec.Helpers.FFProbe.Path), "shared")
			if err := os.Mkdir(dir, 0o777); err != nil || os.Chmod(dir, 0o777) != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(dir, "ffprobe")
			if err := os.Rename(spec.Helpers.FFProbe.Path, moved); err != nil {
				t.Fatal(err)
			}
			spec.Helpers.FFProbe.Path = moved
		},
		"fixture is a directory": func(t *testing.T, spec *CertifySpec) {
			dir := spec.Fixtures[0].Path + ".dir"
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			spec.Fixtures[0].Path = dir
		},
		"fixture is a fifo": func(t *testing.T, spec *CertifySpec) {
			fifo := spec.Fixtures[0].Path + ".fifo"
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			spec.Fixtures[0].Path = fifo
		},
		"fixture missing": func(t *testing.T, spec *CertifySpec) {
			spec.Fixtures[0].Path += ".absent"
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}})
			spec := env.spec
			tamper(t, &spec)
			ws, err := prepareCertifyWorkspace(spec)
			if !errors.Is(err, ErrValidation) || ws != nil {
				t.Fatalf("tampered evidence accepted: %v", err)
			}
			failure, ok := certifyFailureResult(spec.ProfileID, err).(CertifyFailure)
			want := wantRejection[name]
			if !ok || failure.Reason != certifyReasonInputEvidence || failure.Input != want[0] || failure.Check != want[1] {
				t.Fatalf("rejection = %+v, want input %q check %q", failure, want[0], want[1])
			}
			if encoded, _ := json.Marshal(failure); bytes.Contains(encoded, []byte(filepath.Dir(env.spec.Helpers.StillHelper.Path))) {
				t.Fatalf("rejection leaks a path: %s", encoded)
			}
		})
	}

	t.Run("helper replaced after verification", func(t *testing.T) {
		env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}})
		ws, err := prepareCertifyWorkspace(env.spec)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		// Same bytes, new inode: a swap during use is still refused.
		path := env.spec.Helpers.StillHelper.Path
		if err := os.WriteFile(path+".new", env.contents[path], 0o755); err != nil || os.Chmod(path+".new", 0o755) != nil || os.Rename(path+".new", path) != nil {
			t.Fatal("replace helper")
		}
		if err := ws.reverifyHelpers(); !errors.Is(err, ErrValidation) {
			t.Fatalf("replaced helper accepted: %v", err)
		}
	})
}

func certifyDraft(t *testing.T, base profile.Definition, mimes []string, edit func(map[string]profile.Recipe)) profile.Definition {
	t.Helper()
	parameters, err := profile.DecodeParameters(base.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	recipes := map[string]profile.Recipe{}
	for _, mime := range mimes {
		recipes[mime] = parameters.Recipes[mime]
	}
	if edit != nil {
		edit(recipes)
	}
	parameters.Recipes = recipes
	base.ID, base.InputMIMETypes = newID(), mimes
	base.Parameters, _ = json.Marshal(parameters)
	return base
}

func TestCertificationRequirementsFromBundledProfiles(t *testing.T) {
	thumbnail, _, err := certificationRequirements(profile.ThumbnailV1())
	if err != nil || len(thumbnail) != len(profile.ThumbnailV1().InputMIMETypes) {
		t.Fatalf("thumbnail requirements: %v %d", err, len(thumbnail))
	}
	for _, r := range thumbnail {
		if r.OutputKind != profile.OutputStillAVIF || r.MaxEdge != 640 || r.MinSetting != 50 || r.MaxSetting != 50 {
			t.Fatalf("thumbnail requirement %+v", r)
		}
	}
	standard, _, err := certificationRequirements(profile.StandardV1())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[profile.OutputKind]int{}
	for _, r := range standard {
		counts[r.OutputKind]++
		want := map[profile.OutputKind]int{profile.OutputStillAVIF: 60, profile.OutputAnimationWebP: 80, profile.OutputVideoAV1: 32}[r.OutputKind]
		if r.MaxEdge != 1920 || r.MinSetting != want || r.MaxSetting != want {
			t.Fatalf("standard requirement %+v", r)
		}
		if (r.MIMEType == "image/gif" || r.MIMEType == "image/webp") != (r.SourceMode == profile.SourceProbeAnimation) {
			t.Fatalf("source mode %+v", r)
		}
	}
	for _, r := range standard {
		if r.MIMEType == "image/gif" && r.OutputKind == profile.OutputStillAVIF {
			t.Fatal("standard GIF still output is not Worker-executable")
		}
	}
	if counts[profile.OutputStillAVIF] != 14 || counts[profile.OutputAnimationWebP] != 2 || counts[profile.OutputVideoAV1] != 2 {
		t.Fatalf("standard requirement counts %v", counts)
	}
}

func TestCertificationRequirementsRejectUnsupportedProfiles(t *testing.T) {
	jpeg := []string{"image/jpeg"}
	for name, definition := range map[string]profile.Definition{
		"wrong processor": func() profile.Definition {
			d := certifyDraft(t, profile.ThumbnailV1(), jpeg, nil)
			d.Processor = "other-media"
			return d
		}(),
		"wrong schema version": func() profile.Definition {
			d := certifyDraft(t, profile.ThumbnailV1(), jpeg, nil)
			d.ParametersSchemaVersion = 2
			return d
		}(),
		"recipe for another MIME": func() profile.Definition {
			d := certifyDraft(t, profile.ThumbnailV1(), jpeg, nil)
			d.InputMIMETypes = []string{"image/png"}
			return d
		}(),
		"still edge beyond processor": certifyDraft(t, profile.ThumbnailV1(), jpeg, func(r map[string]profile.Recipe) {
			recipe := r["image/jpeg"]
			recipe.MaxLongEdge = 4096
			r["image/jpeg"] = recipe
		}),
		"video CRF beyond processor": certifyDraft(t, profile.StandardV1(), []string{"video/mp4"}, func(r map[string]profile.Recipe) {
			recipe := r["video/mp4"]
			output := *recipe.VideoOutput
			output.CRF = 30
			recipe.VideoOutput = &output
			r["video/mp4"] = recipe
		}),
		"video thumbnail edge beyond processor": certifyDraft(t, profile.ThumbnailV1(), []string{"video/mp4"}, func(r map[string]profile.Recipe) {
			recipe := r["video/mp4"]
			recipe.MaxLongEdge = 1000
			r["video/mp4"] = recipe
		}),
	} {
		if _, _, err := certificationRequirements(definition); !errors.Is(err, ErrValidation) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestCertifyFixtureMIMEsMustExactlyCoverProfile(t *testing.T) {
	definition := certifyDraft(t, profile.ThumbnailV1(), []string{"image/jpeg", "image/png"}, nil)
	fixture := func(mime string) certifyVerifiedFixture { return certifyVerifiedFixture{mimeType: mime} }
	if checkFixtureMIMEs(definition, []certifyVerifiedFixture{fixture("image/jpeg"), fixture("image/png"), fixture("image/png")}) != nil {
		t.Fatal("exact cover refused")
	}
	if !errors.Is(checkFixtureMIMEs(definition, []certifyVerifiedFixture{fixture("image/jpeg")}), ErrValidation) {
		t.Fatal("required MIME without fixture accepted")
	}
	if !errors.Is(checkFixtureMIMEs(definition, []certifyVerifiedFixture{fixture("image/jpeg"), fixture("image/png"), fixture("image/gif")}), ErrValidation) {
		t.Fatal("fixture MIME not required by the profile accepted")
	}
}

func TestCertifyRunsReproduceWorkerRouting(t *testing.T) {
	standard, _ := profile.DecodeParameters(profile.StandardV1().Parameters)
	thumbnail, _ := profile.DecodeParameters(profile.ThumbnailV1().Parameters)
	fixture := func(mime string) certifyVerifiedFixture { return certifyVerifiedFixture{mimeType: mime} }
	type want struct {
		route certifyRoute
		kind  profile.OutputKind
		set   int
	}
	for name, test := range map[string]struct {
		mime   string
		recipe profile.Recipe
		facts  certifyInputFacts
		want   []want
	}{
		"jpeg standard":        {"image/jpeg", standard.Recipes["image/jpeg"], certifyInputFacts{Classification: "still"}, []want{{certifyRouteStill, profile.OutputStillAVIF, 60}}},
		"static webp standard": {"image/webp", standard.Recipes["image/webp"], certifyInputFacts{Classification: "static"}, []want{{certifyRouteStill, profile.OutputStillAVIF, 60}}},
		"animated webp standard": {"image/webp", standard.Recipes["image/webp"], certifyInputFacts{Classification: "animation"},
			[]want{{certifyRouteAnimation, profile.OutputAnimationWebP, 80}}},
		"animated webp thumbnail": {"image/webp", thumbnail.Recipes["image/webp"], certifyInputFacts{Classification: "animation"},
			[]want{{certifyRouteAnimation, profile.OutputStillAVIF, 50}}},
		"gif standard": {"image/gif", standard.Recipes["image/gif"], certifyInputFacts{Classification: "animation"},
			[]want{{certifyRouteAnimation, profile.OutputAnimationWebP, 80}}},
		"gif thumbnail":   {"image/gif", thumbnail.Recipes["image/gif"], certifyInputFacts{Classification: "animation"}, []want{{certifyRouteAnimation, profile.OutputStillAVIF, 50}}},
		"video standard":  {"video/mp4", standard.Recipes["video/mp4"], certifyInputFacts{Classification: "video"}, []want{{certifyRouteVideo, profile.OutputVideoAV1, 32}}},
		"video thumbnail": {"video/quicktime", thumbnail.Recipes["video/quicktime"], certifyInputFacts{Classification: "video", VideoStreamIndex: 1}, []want{{certifyRouteVideo, profile.OutputStillAVIF, 50}}},
	} {
		runs, err := certifyRunsFor(fixture(test.mime), test.recipe, test.facts)
		if err != nil || len(runs) != len(test.want) {
			t.Fatalf("%s: %v %d", name, err, len(runs))
		}
		for i, run := range runs {
			if run.Route != test.want[i].route || run.OutputKind != test.want[i].kind || run.Setting != test.want[i].set || run.Recipe.MaxLongEdge != test.recipe.MaxLongEdge || run.Facts != test.facts {
				t.Fatalf("%s run %d: %+v", name, i, run)
			}
			if run.Recipe.FramePolicy != test.recipe.FramePolicy || (run.Recipe.AnimationOutput == nil) != (test.recipe.AnimationOutput == nil) {
				t.Fatalf("%s run %d does not use the exact profile recipe", name, i)
			}
		}
	}
	for name, test := range map[string]struct {
		mime   string
		recipe profile.Recipe
		facts  certifyInputFacts
	}{
		"static gif":           {"image/gif", standard.Recipes["image/gif"], certifyInputFacts{Classification: "static"}},
		"unclassified webp":    {"image/webp", standard.Recipes["image/webp"], certifyInputFacts{}},
		"video without stream": {"video/mp4", standard.Recipes["video/mp4"], certifyInputFacts{Classification: "video", VideoStreamIndex: -1}},
		"still misclassified":  {"image/png", standard.Recipes["image/png"], certifyInputFacts{Classification: "animation"}},
	} {
		if _, err := certifyRunsFor(fixture(test.mime), test.recipe, test.facts); !errors.Is(err, ErrValidation) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func certifyGoodFacts(run certifyRun) (certifyTransformFacts, certifyProbeFacts) {
	edge := run.Recipe.MaxLongEdge
	transformed := certifyTransformFacts{OutputMIME: "image/avif", Width: edge, Height: edge * 3 / 4, Setting: run.Setting, BitDepth: 8,
		MaxLongEdge: edge, SourceWidth: edge * 2, SourceHeight: edge * 3 / 2, FrameCount: 1, Decoder: "d", Encoder: "e", ToolVersion: "t"}
	probed := certifyProbeFacts{Codec: "av1", Width: transformed.Width, Height: transformed.Height, VideoStreams: 1, Container: "mov,mp4,m4a,3gp,3g2,mj2"}
	switch run.OutputKind {
	case profile.OutputAnimationWebP:
		transformed.OutputMIME, transformed.FrameCount = "image/webp", 3
		probed.Codec, probed.FrameCount, probed.Container = "webp", 3, "webp_pipe"
	case profile.OutputVideoAV1:
		transformed.OutputMIME, transformed.BitDepth, transformed.AudioPresent, transformed.FrameCount = "video/mp4", 10, true, 0
		probed.PixelFormat, probed.AudioStreams, probed.AudioCodec = "yuv420p10le", 1, "aac"
	}
	return transformed, probed
}

func TestEvaluateCertifyRunFailsClosed(t *testing.T) {
	recipe := profile.Recipe{MaxLongEdge: 640}
	for _, kind := range []profile.OutputKind{profile.OutputStillAVIF, profile.OutputAnimationWebP, profile.OutputVideoAV1} {
		run := certifyRun{OutputKind: kind, Recipe: recipe, Setting: 50}
		transformed, probed := certifyGoodFacts(run)
		if err := evaluateCertifyRun(run, transformed, probed); err != nil {
			t.Fatalf("%s valid evidence refused: %v", kind, err)
		}
		mutations := map[string]func(*certifyTransformFacts, *certifyProbeFacts){
			"output mime":           func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.OutputMIME = "image/png" },
			"setting outside range": func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.Setting = 51 },
			"wrong reported edge":   func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.MaxLongEdge = 1920 },
			"edge not exercised": func(tf *certifyTransformFacts, p *certifyProbeFacts) {
				tf.SourceWidth, tf.SourceHeight, tf.Width, tf.Height = 320, 240, 320, 240
				p.Width, p.Height = 320, 240
			},
			"upscaled":           func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.SourceWidth, tf.SourceHeight = 640, 400 },
			"aspect changed":     func(tf *certifyTransformFacts, p *certifyProbeFacts) { tf.Height, p.Height = 400, 400 },
			"bit depth":          func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.BitDepth = 12 },
			"probe codec":        func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.Codec = "h264" },
			"probe width":        func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.Width-- },
			"probe height":       func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.Height++ },
			"probe no video":     func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.VideoStreams = 0 },
			"probe extra audio":  func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.AudioStreams++ },
			"zero output":        func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.Width = 0 },
			"processor mismatch": func(tf *certifyTransformFacts, _ *certifyProbeFacts) { tf.SourceHeight = 0 },
		}
		// The pinned FFmpeg 9 ffprobe reports animated WebP as webp_anim; that
		// name is accepted for animated WebP output only.
		animTransformed, animProbed := certifyGoodFacts(run)
		animProbed.Codec = "webp_anim"
		if err := evaluateCertifyRun(run, animTransformed, animProbed); (err == nil) != (kind == profile.OutputAnimationWebP) {
			t.Fatalf("%s with probe codec webp_anim: %v", kind, err)
		}
		switch kind {
		case profile.OutputAnimationWebP:
			mutations["frame count"] = func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.FrameCount = 2 }
		case profile.OutputVideoAV1:
			mutations["pixel format"] = func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.PixelFormat = "yuv420p" }
			mutations["container"] = func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.Container = "matroska,webm" }
			mutations["audio codec"] = func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.AudioCodec = "opus" }
			mutations["two video streams"] = func(_ *certifyTransformFacts, p *certifyProbeFacts) { p.VideoStreams = 2 }
			mutations["odd dimension"] = func(tf *certifyTransformFacts, p *certifyProbeFacts) { tf.Height, p.Height = 479, 479 }
		}
		for name, mutate := range mutations {
			tf, pf := certifyGoodFacts(run)
			mutate(&tf, &pf)
			if err := evaluateCertifyRun(run, tf, pf); !errors.Is(err, ErrValidation) {
				t.Errorf("%s/%s accepted", kind, name)
			}
		}
	}
}

// fakeCertifyRuntime emulates validated processor results without codecs.
type fakeCertifyRuntime struct {
	classification map[string]string
	decoders       []string
	failTransform  func(certifyRun) error
	tamperProbe    func(certifyRun, *certifyProbeFacts)
	afterTransform func(certifyRun)
	runs           []certifyRun
	last           certifyRun
	lastFacts      certifyTransformFacts
}

func (f *fakeCertifyRuntime) Identity(context.Context) (map[string]certifyProcessorIdentity, error) {
	decoders := f.decoders
	if decoders == nil {
		decoders = profile.ThumbnailV1().InputMIMETypes
	}
	identity := certifyProcessorIdentity{HelperVersion: "fake/1", LibraryVersions: map[string]string{"fake": "1"}, Encoders: []string{"aom"}, DecoderMIMETypes: decoders}
	return map[string]certifyProcessorIdentity{"still": identity, "animation": identity, "video": identity}, nil
}

func (f *fakeCertifyRuntime) Inspect(_ context.Context, mime string, input *os.File) (certifyInputFacts, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return certifyInputFacts{}, err
	}
	switch {
	case mime == "image/gif":
		return certifyInputFacts{Classification: "animation", VideoStreamIndex: -1}, nil
	case mime == "image/webp":
		return certifyInputFacts{Classification: f.classification[certifyDigest(data)], VideoStreamIndex: -1}, nil
	case strings.HasPrefix(mime, "video/"):
		return certifyInputFacts{Classification: "video", VideoStreamIndex: 0}, nil
	}
	return certifyInputFacts{Classification: "still", VideoStreamIndex: -1}, nil
}

func (f *fakeCertifyRuntime) Transform(_ context.Context, run certifyRun, _ *os.File, output *os.File) (certifyTransformFacts, error) {
	f.runs = append(f.runs, run)
	if f.failTransform != nil {
		if err := f.failTransform(run); err != nil {
			return certifyTransformFacts{}, err
		}
	}
	if _, err := output.WriteString("output for " + string(run.Route) + run.Fixture.sha256); err != nil {
		return certifyTransformFacts{}, err
	}
	if f.afterTransform != nil {
		f.afterTransform(run)
	}
	f.last = run
	f.lastFacts, _ = certifyGoodFacts(run)
	return f.lastFacts, nil
}

func (f *fakeCertifyRuntime) Probe(_ context.Context, kind profile.OutputKind, _ *os.File) (certifyProbeFacts, error) {
	_, probed := certifyGoodFacts(f.last)
	if kind != f.last.OutputKind {
		probed.Codec = "mismatch"
	}
	if f.tamperProbe != nil {
		f.tamperProbe(f.last, &probed)
	}
	return probed, nil
}

func TestCertifyProfileAllOrNothingThroughSeam(t *testing.T) {
	ctx := context.Background()
	actor := certifyActor{Actor: "operator", Host: "photos-host"}
	definition := certifyDraft(t, profile.StandardV1(), []string{"image/jpeg", "image/webp", "video/mp4"}, nil)
	env := newCertifyTestEnv(t, map[string][]string{"image/jpeg": {"a"}, "image/webp": {"static", "anim"}, "video/mp4": {"clip"}})
	classification := map[string]string{}
	for _, fixture := range env.spec.Fixtures {
		if fixture.MIMEType == "image/webp" {
			classification[fixture.SHA256] = strings.TrimPrefix(string(env.contents[fixture.Path]), "image/webp/")
			if classification[fixture.SHA256] == "anim" {
				classification[fixture.SHA256] = "animation"
			}
		}
	}
	prepare := func(t *testing.T) *certifyWorkspace {
		ws, err := prepareCertifyWorkspace(env.spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ws.Close() })
		return ws
	}

	ws := prepare(t)
	runtime := &fakeCertifyRuntime{classification: classification}
	rows, err := certifyProfile(ctx, definition, ws, runtime, actor)
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	// jpeg still; webp still (static fixture) + animation; mp4 video.
	if len(rows) != 4 || len(runtime.runs) != 4 {
		t.Fatalf("rows=%d runs=%d", len(rows), len(runtime.runs))
	}
	secrets := []string{ws.dir, env.dir}
	for _, fixture := range env.spec.Fixtures {
		secrets = append(secrets, fixture.Path)
	}
	for _, row := range rows {
		for _, secret := range secrets {
			if strings.Contains(row.Evidence, secret) {
				t.Fatal("receipt leaks a path")
			}
		}
		var receipt certifyReceipt
		decoder := json.NewDecoder(strings.NewReader(row.Evidence))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&receipt); err != nil {
			t.Fatalf("receipt is not canonical JSON: %v", err)
		}
		reencoded, _ := json.Marshal(receipt)
		if string(reencoded) != row.Evidence {
			t.Fatal("receipt is not in canonical encoding")
		}
		if receipt.Schema != certifyReceiptSchema || receipt.Scope != certifyReceiptScope || receipt.Requirement != row.Requirement ||
			receipt.ICCSHA256 != env.spec.Helpers.SRGBICC.SHA256 || receipt.HelperSHA256["still_helper"] != env.spec.Helpers.StillHelper.SHA256 ||
			receipt.HelperSHA256["ffprobe"] != env.spec.Helpers.FFProbe.SHA256 || len(receipt.HelperSHA256) != 5 ||
			receipt.CertifiedBy != actor || receipt.Profile.ID != definition.ID || receipt.NMCPAdmin["vcs_revision"] == "" ||
			len(receipt.Runs) == 0 || receipt.Runs[0].OutputSHA256 == "" || receipt.Runs[0].InputSHA256 == "" || len(receipt.ProcessorIdentity) != 3 {
			t.Fatalf("receipt incomplete: %s", row.Evidence)
		}
		if row.Requirement.MIMEType == "image/webp" && row.Requirement.OutputKind == profile.OutputStillAVIF && receipt.Runs[0].Route != certifyRouteStill {
			t.Fatal("static webp did not use the Worker still route")
		}
	}

	for name, configure := range map[string]func(*fakeCertifyRuntime){
		"one transform fails": func(f *fakeCertifyRuntime) {
			f.failTransform = func(run certifyRun) error {
				if run.Fixture.mimeType == "video/mp4" {
					return errors.New("helper process failure")
				}
				return nil
			}
		},
		"one probe mismatches": func(f *fakeCertifyRuntime) {
			f.tamperProbe = func(run certifyRun, p *certifyProbeFacts) {
				if run.OutputKind == profile.OutputAnimationWebP {
					p.FrameCount++
				}
			}
		},
		"decoder not advertised": func(f *fakeCertifyRuntime) { f.decoders = []string{"image/jpeg"} },
		"no static webp fixture": func(f *fakeCertifyRuntime) {
			f.classification = map[string]string{}
			for sha := range classification {
				f.classification[sha] = "animation"
			}
		},
		"helper rewritten during run": func(f *fakeCertifyRuntime) {
			f.afterTransform = func(certifyRun) {
				_ = os.WriteFile(env.spec.Helpers.StillHelper.Path, []byte("#!evil"), 0o755)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if err := os.WriteFile(env.spec.Helpers.StillHelper.Path, env.contents[env.spec.Helpers.StillHelper.Path], 0o755); err != nil {
					t.Fatal(err)
				}
			}()
			f := &fakeCertifyRuntime{classification: classification}
			configure(f)
			rows, err := certifyProfile(ctx, definition, prepare(t), f, actor)
			if err == nil || len(rows) != 0 {
				t.Fatalf("partial certification accepted: rows=%d", len(rows))
			}
			if name == "decoder not advertised" {
				failure, ok := certifyFailureResult(definition.ID, err).(CertifyFailure)
				if !ok || failure.Reason != certifyReasonInputEvidence || failure.Check != "decoder_not_advertised" || !strings.HasPrefix(failure.Input, "fixture[") {
					t.Fatalf("decoder refusal = %+v (%v)", failure, err)
				}
			}
		})
	}

	t.Run("no static webp fixture names the uncovered requirement", func(t *testing.T) {
		f := &fakeCertifyRuntime{classification: map[string]string{}}
		for sha := range classification {
			f.classification[sha] = "animation"
		}
		_, err := certifyProfile(ctx, definition, prepare(t), f, actor)
		var failure *certifyFailureError
		if !errors.Is(err, ErrValidation) || !errors.As(err, &failure) || failure.reason != certifyReasonRequirementUncovered ||
			failure.requirement.MIMEType != "image/webp" || failure.requirement.OutputKind != profile.OutputStillAVIF || len(f.runs) != 0 {
			t.Fatalf("uncovered requirement: %v", err)
		}
	})

	t.Run("gif under all frames needs only animation-webp", func(t *testing.T) {
		gif := certifyDraft(t, profile.StandardV1(), []string{"image/gif"}, nil)
		gifEnv := newCertifyTestEnv(t, map[string][]string{"image/gif": {"anim", "anim2"}})
		ws, err := prepareCertifyWorkspace(gifEnv.spec)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		f := &fakeCertifyRuntime{}
		rows, err := certifyProfile(ctx, gif, ws, f, actor)
		if err != nil || len(rows) != 1 || rows[0].Requirement.OutputKind != profile.OutputAnimationWebP || rows[0].Runs != 2 || len(f.runs) != 2 {
			t.Fatalf("gif standard: rows=%d err=%v", len(rows), err)
		}
		for _, run := range f.runs {
			if run.Route != certifyRouteAnimation || run.OutputKind != profile.OutputAnimationWebP {
				t.Fatalf("non-Worker route: %+v", run)
			}
		}
	})

	t.Run("uncovered requirement is a sanitized CLI result", func(t *testing.T) {
		f := &fakeCertifyRuntime{classification: map[string]string{}}
		for sha := range classification {
			f.classification[sha] = "animation"
		}
		_, err := certifyProfile(ctx, definition, prepare(t), f, actor)
		var out, diagnostics bytes.Buffer
		if code := finish(&out, &diagnostics, true, certifyFailureResult(definition.ID, err), err); code != 2 {
			t.Fatalf("code=%d", code)
		}
		var document struct {
			ExitCode int            `json:"exit_code"`
			Result   CertifyFailure `json:"result"`
		}
		if json.Unmarshal(out.Bytes(), &document) != nil || document.ExitCode != 2 || document.Result.Reason != certifyReasonRequirementUncovered ||
			document.Result.Requirement.MIMEType != "image/webp" || document.Result.ProfileID != definition.ID || strings.Contains(out.String(), env.dir) {
			t.Fatalf("CLI failure result: %s", out.String())
		}
	})

	t.Run("fixture MIME not required", func(t *testing.T) {
		narrow := certifyDraft(t, profile.StandardV1(), []string{"image/jpeg", "image/webp"}, nil)
		if _, err := certifyProfile(ctx, narrow, prepare(t), &fakeCertifyRuntime{classification: classification}, actor); !errors.Is(err, ErrValidation) {
			t.Fatalf("extra fixture MIME accepted: %v", err)
		}
	})
}

func certifyTestWebP(width, height, frames int, animation bool) []byte {
	var body bytes.Buffer
	chunk := func(name string, payload []byte) {
		body.WriteString(name)
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(payload)))
		body.Write(payload)
		if len(payload)%2 == 1 {
			body.WriteByte(0)
		}
	}
	vp8x := make([]byte, 10)
	if animation {
		vp8x[0] = 0x02
	}
	w, h := width-1, height-1
	vp8x[4], vp8x[5], vp8x[6] = byte(w), byte(w>>8), byte(w>>16)
	vp8x[7], vp8x[8], vp8x[9] = byte(h), byte(h>>8), byte(h>>16)
	chunk("VP8X", vp8x)
	chunk("ANIM", make([]byte, 6))
	for i := 0; i < frames; i++ {
		chunk("ANMF", make([]byte, 17))
	}
	var file bytes.Buffer
	file.WriteString("RIFF")
	_ = binary.Write(&file, binary.LittleEndian, uint32(body.Len()+4))
	file.WriteString("WEBP")
	file.Write(body.Bytes())
	return file.Bytes()
}

func TestCertifyIndependentProbeParsers(t *testing.T) {
	width, height, frames, ok := certifyAnimatedWebPStructure(bytes.NewReader(certifyTestWebP(1920, 1080, 3, true)))
	if !ok || width != 1920 || height != 1080 || frames != 3 {
		t.Fatalf("animated webp structure: %d %d %d %v", width, height, frames, ok)
	}
	valid := certifyTestWebP(640, 480, 2, true)
	for name, data := range map[string][]byte{
		"not animated": certifyTestWebP(640, 480, 2, false),
		"no frames":    certifyTestWebP(640, 480, 0, true),
		"truncated":    valid[:len(valid)-4],
		"not riff":     append([]byte("RIFX"), valid[4:]...),
		"empty":        {},
	} {
		if _, _, _, ok := certifyAnimatedWebPStructure(bytes.NewReader(data)); ok {
			t.Errorf("%s accepted", name)
		}
	}
	facts, err := parseCertifyFFProbe([]byte(`{"streams":[{"index":1,"codec_type":"audio","codec_name":"aac"},{"index":0,"codec_type":"video","codec_name":"av1","width":1920,"height":1080,"pix_fmt":"yuv420p10le"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`))
	if err != nil || facts.Codec != "av1" || facts.Width != 1920 || facts.Height != 1080 || facts.PixelFormat != "yuv420p10le" || facts.VideoStreams != 1 || facts.AudioStreams != 1 || facts.AudioCodec != "aac" {
		t.Fatalf("ffprobe facts: %+v %v", facts, err)
	}
	for _, bad := range []string{``, `not json`, `{"streams":[{"index":0,"codec_type":"subtitle","codec_name":"mov_text"}]}`} {
		if _, err := parseCertifyFFProbe([]byte(bad)); !errors.Is(err, ErrValidation) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCertifyProcessorErrorsAreSanitizedAndClassified(t *testing.T) {
	if err := certifyProcessorError(errors.New("postgres://CANARY")); errors.Is(err, ErrValidation) {
		t.Fatal("operational failure classified as validation")
	}
	var out, diagnostics bytes.Buffer
	if code := finish(&out, &diagnostics, true, nil, certifyProcessorError(errors.New("postgres://CANARY"))); code != 1 || strings.Contains(out.String()+diagnostics.String(), "CANARY") {
		t.Fatalf("code=%d output leaked", code)
	}
}

func TestCertifyEvidenceIdentityIsStrict(t *testing.T) {
	definition := certifyDraft(t, profile.ThumbnailV1(), []string{"image/jpeg"}, nil)
	requirement := certifyRequirement{MIMEType: "image/jpeg", SourceMode: profile.SourceStill, OutputKind: profile.OutputStillAVIF, MaxEdge: 640, MinSetting: 50, MaxSetting: 50}
	digest := func(name string) string { return certifyDigest([]byte(name)) }
	identity := certifyHelperIdentity{icc: digest("icc"), helpers: map[string]string{}}
	for _, name := range certifyHelperDigestNames {
		identity.helpers[name] = digest(name)
	}
	receipt := func(edit func(*certifyReceipt)) string {
		helpers := map[string]string{}
		for name, value := range identity.helpers {
			helpers[name] = value
		}
		r := certifyReceipt{Schema: certifyReceiptSchema, Scope: certifyReceiptScope, Profile: certifyProfileRef{ID: newID(), Key: "older", Version: 1},
			Processor: profile.Processor, ParametersSchemaVersion: profile.ParametersSchemaVersion, Requirement: requirement,
			HelperSHA256: helpers, ICCSHA256: identity.icc, Runs: []certifyRunReceipt{{Route: certifyRouteStill}}, NMCPAdmin: map[string]string{"vcs_revision": "x"}}
		if edit != nil {
			edit(&r)
		}
		data, _ := json.Marshal(r)
		return string(data)
	}
	// The profile that produced the old row does not matter; only the requirement and digests.
	if !certifyEvidenceIdentical(receipt(nil), definition, requirement, identity) {
		t.Fatal("identical helper digests not detected")
	}
	valid := receipt(nil)
	for name, evidence := range map[string]string{
		"changed still helper":  receipt(func(r *certifyReceipt) { r.HelperSHA256["still_helper"] = digest("new still") }),
		"changed ffprobe":       receipt(func(r *certifyReceipt) { r.HelperSHA256["ffprobe"] = digest("new ffprobe") }),
		"changed prlimit":       receipt(func(r *certifyReceipt) { r.HelperSHA256["prlimit"] = digest("new prlimit") }),
		"changed icc":           receipt(func(r *certifyReceipt) { r.ICCSHA256 = digest("new icc") }),
		"missing helper digest": receipt(func(r *certifyReceipt) { delete(r.HelperSHA256, "video_helper") }),
		"extra helper digest":   receipt(func(r *certifyReceipt) { r.HelperSHA256["ffmpeg"] = digest("ffmpeg") }),
		"other schema":          receipt(func(r *certifyReceipt) { r.Schema = "nmcp-profile-certification-receipt/0" }),
		"other requirement":     receipt(func(r *certifyReceipt) { r.Requirement.MaxEdge = 1920 }),
		"other processor":       receipt(func(r *certifyReceipt) { r.Processor = "other" }),
		"foreign text":          "admin integration fixture only",
		"empty":                 "",
		"malformed":             valid[:len(valid)-1],
		"trailing value":        valid + ` {}`,
		"unknown field":         strings.Replace(valid, `{"schema"`, `{"extra":1,"schema"`, 1),
		"duplicate digest key":  strings.Replace(valid, `"still_helper":"`, `"still_helper":"`+digest("x")+`","still_helper":"`, 1),
		"uppercase digest":      strings.ReplaceAll(valid, identity.helpers["still_helper"], strings.ToUpper(identity.helpers["still_helper"])),
		"null helpers":          receipt(func(r *certifyReceipt) { r.HelperSHA256 = nil }),
	} {
		if certifyEvidenceIdentical(evidence, definition, requirement, identity) {
			t.Errorf("%s counted as identical", name)
		}
	}
}

// TestCertifyRunAsDropsRootHelpers pins review D2: a root certify never runs
// helper or probe processes as root.
func TestCertifyRunAsDropsRootHelpers(t *testing.T) {
	previousEUID, previousLookup := certifyEUID, certifyLookupUser
	t.Cleanup(func() { certifyEUID, certifyLookupUser = previousEUID, previousLookup })
	certifyEUID = func() int { return 1000 }
	if runAs, err := certifyRunAs(); err != nil || runAs != nil {
		t.Fatalf("non-root certify changed identity: %+v %v", runAs, err)
	}
	certifyEUID = func() int { return 0 }
	certifyLookupUser = func(name string) (*user.User, error) {
		if name != "nmcp" {
			t.Fatalf("looked up %q", name)
		}
		return &user.User{Uid: "999", Gid: "998", Username: "nmcp"}, nil
	}
	if runAs, err := certifyRunAs(); err != nil || runAs == nil || runAs.UID != 999 || runAs.GID != 998 {
		t.Fatalf("root certify did not drop to nmcp: %+v %v", runAs, err)
	}
	for name, account := range map[string]*user.User{"root-owned nmcp": {Uid: "0", Gid: "0"}, "unparsable": {Uid: "x", Gid: "1"}} {
		certifyLookupUser = func(string) (*user.User, error) { return account, nil }
		if runAs, err := certifyRunAs(); !errors.Is(err, ErrValidation) || runAs != nil {
			t.Fatalf("%s accepted: %+v %v", name, runAs, err)
		}
	}
	certifyLookupUser = func(string) (*user.User, error) { return nil, user.UnknownUserError("nmcp") }
	if runAs, err := certifyRunAs(); !errors.Is(err, ErrValidation) || runAs != nil {
		t.Fatalf("missing nmcp user did not refuse root certify: %+v %v", runAs, err)
	}
}
