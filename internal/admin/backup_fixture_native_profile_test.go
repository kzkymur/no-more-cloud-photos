package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
)

func fixtureNativeProfile() profile.Definition {
	definition := profile.ThumbnailV1()
	definition.ID, definition.Key, definition.Version = newID(), "live-native-thumbnail", 1
	definition.InputMIMETypes = []string{"image/jpeg", "image/png"}
	parameters := profile.ThumbnailV1Parameters()
	parameters.Recipes = map[string]profile.Recipe{"image/jpeg": parameters.Recipes["image/jpeg"], "image/png": parameters.Recipes["image/png"]}
	definition.Parameters, _ = json.Marshal(parameters)
	return definition
}

func fixtureNativeFailure(err error) string {
	if err == nil {
		return "result_mismatch"
	}
	for _, item := range []struct {
		err  error
		code string
	}{
		{stillprocessor.ErrTimeout, "timeout"}, {processrunner.ErrTimeout, "timeout"}, {context.DeadlineExceeded, "deadline"},
		{stillprocessor.ErrInvalid, "invalid_input"}, {stillprocessor.ErrDecode, "decode"}, {stillprocessor.ErrCapability, "capability"},
		{stillprocessor.ErrResourcePolicy, "resource_policy"}, {stillprocessor.ErrProcess, "process"}, {processrunner.ErrFailed, "process"},
		{stillprocessor.ErrLogOutputLimit, "output_limit"}, {processrunner.ErrOutputLimit, "output_limit"},
	} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return "unrecognized"
}

func fixtureNativeResultValid(r stillprocessor.Result, isPNG bool) bool {
	return r.OutputMIME == "image/avif" && r.OutputExtension == "avif" && r.Quality == 50 && r.BitDepth == 8 && r.MaxLongEdge == 640 && r.Threads == 1 && r.Width > 0 && r.Height > 0 && max(r.Width, r.Height) == 640 && r.Audit.ICCSHA256 == config.RequiredSRGBICCSHA256 && (!isPNG || r.Width == 640 && r.Height == 480)
}

// This is narrowly scoped fixture evidence, not automatic production certification.
// Both real native transformations and independent AVIF probes MUST finish before
// any registry insert. The later API/production-worker jobs remain mandatory.
func (f *liveCutoverFixture) prepareLiveNativeProfile(pool *pgxpool.Pool, jpeg []byte) {
	t := f.t
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 150*time.Second)
	defer cancel()
	if pg17Scalar(t, ctx, pool, `SELECT count(*)::text FROM profiles WHERE status='active'`) != "0" {
		t.Fatal("fresh fixture has unexpected active profiles")
	}
	definition := fixtureNativeProfile()
	service := New(pool)
	if err := service.ProfileCreate(ctx, definition); err != nil {
		t.Fatalf("create narrow native fixture profile: sqlstate=%s", fixtureSQLState(err))
	}
	if err := service.ProfileTransition(ctx, definition.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatal("uncertified native profile activation was not refused")
	}
	t.Log("actual uncertified fixture profile activation refused before native evidence")
	parameters := profile.ThumbnailV1Parameters()
	policy := stillprocessor.DefaultPolicy()
	policy.Timeout = 60 * time.Second
	policy.GeneratedOutputMaxBytes = 8 << 20
	policy.LogBytesPerStream = 64 << 10
	const helper = "/opt/nmcp/current/bin/nmcp-still-helper"
	processor, err := stillprocessor.New(stillprocessor.Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: "/opt/nmcp/current/sRGB.icc", SRGBICCSHA256: config.RequiredSRGBICCSHA256, Policy: policy})
	if err != nil {
		t.Fatal("configure bounded native fixture proof")
	}
	probe, err := processrunner.New("/usr/bin/prlimit", processrunner.Limits{Timeout: 15 * time.Second, AddressSpaceBytes: 2 << 30, FileSizeBytes: 8 << 20, OutputBytesPerStream: 64 << 10})
	if err != nil {
		t.Fatal("configure bounded independent AVIF probe")
	}
	helperFile, err := os.Open(helper)
	if err != nil {
		t.Fatal("open exact native helper")
	}
	helperHash := sha256.New()
	_, err = io.Copy(helperHash, helperFile)
	helperFile.Close()
	if err != nil {
		t.Fatal("hash exact native helper")
	}
	img := image.NewRGBA(image.Rect(0, 0, 960, 720))
	for y := 0; y < 720; y++ {
		for x := 0; x < 960; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 91, A: 255})
		}
	}
	var encoded bytes.Buffer
	if png.Encode(&encoded, img) != nil {
		t.Fatal("encode native PNG proof")
	}
	dir := filepath.Join(f.root, "native-profile-proof")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("exclusively create native proof directory")
	}
	evidence := make([]string, 0, 2)
	for i, data := range [][]byte{jpeg, encoded.Bytes()} {
		mime := definition.InputMIMETypes[i]
		inputPath := filepath.Join(dir, fmt.Sprintf("input-%d", i))
		f.write(inputPath, string(data), 0600)
		input, err := os.Open(inputPath)
		if err != nil {
			t.Fatal("open native proof input")
		}
		defer input.Close()
		output, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("output-%d.avif", i)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal("exclusively create native proof output")
		}
		defer output.Close()
		result, err := processor.Transform(ctx, stillprocessor.Request{Input: input, Output: output, MIMEType: mime, Recipe: parameters.Recipes[mime]})
		if err != nil || !fixtureNativeResultValid(result, i == 1) {
			t.Fatalf("actual native certification transform failed: mime=%s code=%s", mime, fixtureNativeFailure(err))
		}
		inspected, err := probe.Run(ctx, processrunner.Command{Executable: "/opt/nmcp-native-ci/bin/ffprobe", Arguments: []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height", "-of", "json", "/proc/self/fd/3"}, Files: []*os.File{output}})
		if err != nil {
			t.Fatalf("actual independent AVIF probe failed: mime=%s code=%s", mime, fixtureNativeFailure(err))
		}
		var document struct {
			Streams []struct {
				Codec         string `json:"codec_name"`
				Width, Height int
			} `json:"streams"`
		}
		if json.Unmarshal(inspected.Stdout, &document) != nil || len(document.Streams) != 1 || document.Streams[0].Codec != "av1" || document.Streams[0].Width != result.Width || document.Streams[0].Height != result.Height {
			t.Fatal("native output independent AVIF evidence mismatch")
		}
		if _, err = output.Seek(0, io.SeekStart); err != nil {
			t.Fatal("rewind certified output")
		}
		outputHash := sha256.New()
		size, err := io.Copy(outputHash, output)
		if err != nil || size <= 0 {
			t.Fatal("hash actual native output")
		}
		receipt, _ := json.Marshal(map[string]any{"scope": "isolated backup fixture only; actual native transform and independent AVIF probe", "candidate": os.Getenv("TEST_BACKUP_CANDIDATE_SHA"), "mime": mime, "helper_sha256": fmt.Sprintf("%x", helperHash.Sum(nil)), "icc_sha256": config.RequiredSRGBICCSHA256, "input_sha256": fmt.Sprintf("%x", sha256.Sum256(data)), "output_sha256": fmt.Sprintf("%x", outputHash.Sum(nil)), "width": result.Width, "height": result.Height, "quality": 50, "max_long_edge": 640})
		evidence = append(evidence, string(receipt))
		t.Logf("actual fixture native certification proof: mime=%s AVIF probe passed; edge=640 quality=50", mime)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin paired native fixture evidence")
	}
	defer rollback(tx)
	for i, mime := range definition.InputMIMETypes {
		_, err = tx.Exec(ctx, `INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence) VALUES($1,'nmcp-media',1,$2,'still','still-avif',640,50,50,$3)`, newID(), mime, evidence[i])
		if err != nil {
			t.Fatalf("record actual fixture evidence: sqlstate=%s", fixtureSQLState(err))
		}
	}
	if tx.Commit(ctx) != nil {
		t.Fatal("commit paired native fixture evidence")
	}
	if err = service.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatalf("activate evidence-backed native fixture profile: sqlstate=%s", fixtureSQLState(err))
	}
	if pg17Scalar(t, ctx, pool, `SELECT count(*)::text FROM profiles WHERE status='active' AND id=$1 AND 'image/jpeg'=ANY(input_mime_types) AND 'image/png'=ANY(input_mime_types)`, definition.ID) != "1" {
		t.Fatal("active native workload prerequisite absent")
	}
	if pg17Scalar(t, ctx, pool, `SELECT count(*)::text FROM admin_audit WHERE command IN ('profile_create','profile_activate') AND outcome='succeeded' AND $1::uuid=ANY(affected_ids)`, definition.ID) != "2" {
		t.Fatal("native fixture administrative success audits absent")
	}
	t.Log("actual JPEG/PNG fixture profile created and activated through administrative boundary before worker startup")
}

func fixtureUploadHasNativeJob(reader io.Reader) bool {
	body, err := io.ReadAll(io.LimitReader(reader, 65537))
	if err != nil || len(body) > 65536 {
		return false
	}
	var response struct {
		Job *struct {
			ID, Type string
			Targets  []struct {
				ID      string
				Profile struct{ Key string }
			}
		}
	}
	if json.Unmarshal(body, &response) != nil || response.Job == nil || !uuidPattern.MatchString(response.Job.ID) || response.Job.Type != "transform" || len(response.Job.Targets) != 1 {
		return false
	}
	return uuidPattern.MatchString(response.Job.Targets[0].ID) && response.Job.Targets[0].Profile.Key == "live-native-thumbnail"
}

func TestBackupFixtureNativeWorkloadPrerequisites(t *testing.T) {
	definition := fixtureNativeProfile()
	if profile.ValidateDraft(definition) != nil {
		t.Fatal("invalid fixture profile definition")
	}
	if fixtureNativeFailure(errors.New("postgres://CANARY")) != "unrecognized" || fixtureNativeFailure(stillprocessor.ErrTimeout) != "timeout" {
		t.Fatal("native error classification leaked or changed")
	}
	var parameters profile.Parameters
	if json.Unmarshal(definition.Parameters, &parameters) != nil || len(definition.InputMIMETypes) != 2 || len(parameters.Recipes) != 2 || parameters.EvidenceStatus != profile.EvidenceProvisional {
		t.Fatal("fixture profile expanded beyond proved MIME pair")
	}
	for _, mime := range definition.InputMIMETypes {
		recipe, ok := parameters.Recipes[mime]
		if !ok || recipe.MaxLongEdge != 640 || recipe.StillOutput == nil || recipe.StillOutput.Quality != 50 || recipe.StillOutput.BitDepth != 8 {
			t.Fatal("fixture recipe exceeds actual proof scope")
		}
	}
	valid := `{"job":{"id":"60000000-0000-4000-8000-000000000001","type":"transform","targets":[{"id":"60000000-0000-4000-8000-000000000002","profile":{"key":"live-native-thumbnail"}}]}}`
	if !fixtureUploadHasNativeJob(bytes.NewBufferString(valid)) {
		t.Fatal("valid native workload refused")
	}
	for _, body := range []string{`{"job":null}`, `{}`, `{"job":{"targets":[]}}`, valid + "CANARY", string(bytes.Repeat([]byte("x"), 65537))} {
		if fixtureUploadHasNativeJob(bytes.NewBufferString(body)) {
			t.Fatal("missing or unbounded native workload accepted")
		}
	}
	result := stillprocessor.Result{OutputMIME: "image/avif", OutputExtension: "avif", Width: 640, Height: 480, Quality: 50, BitDepth: 8, MaxLongEdge: 640, Threads: 1, Audit: stillprocessor.Audit{ICCSHA256: config.RequiredSRGBICCSHA256}}
	if !fixtureNativeResultValid(result, true) {
		t.Fatal("valid exact native recipe refused")
	}
	result.Quality = 51
	if fixtureNativeResultValid(result, true) {
		t.Fatal("unproved quality accepted")
	}
}
