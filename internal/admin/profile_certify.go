package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

// Profile certification is the only supported writer of the append-only
// profile_processor_certifications registry. It is SELF-AUTHENTICATING: it
// never trusts externally produced output bytes. It hashes the exact helper
// binaries it executes, copies every fixture into a private directory while
// hashing it, runs the same processors the Worker uses with the profile's own
// recipes, independently probes every output, and only then inserts all rows in
// one audited transaction whose final validator check must pass.

const (
	certifySpecMaxBytes    = 1 << 20
	certifyMaxFixtures     = 64
	certifyMaxFixtureBytes = int64(2 << 30)
	// certifyMaxTotalFixtureBytes bounds all copies of one spec together.
	certifyMaxTotalFixtureBytes = int64(4 << 30)
	certifyReceiptSchema        = "nmcp-profile-certification-receipt/1"
	certifyReceiptScope         = "proves only that this host's exact helper binaries and sRGB ICC transformed the listed fixtures with this recipe and the outputs passed an independent probe; it is not evidence for every possible input"
	certifyStatusCertified      = "certified"
	certifyAuditCommand         = "profile_certify"
	certifyProfileLockClass     = 1313686352
)

var (
	sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// certifyRequiredICCSHA256 is the only ICC the Worker accepts. It is a
	// variable solely so unit tests can run without the pinned ICC bytes.
	certifyRequiredICCSHA256 = config.RequiredSRGBICCSHA256

	// errCertifyEvidence classifies a self-authentication or output evidence
	// mismatch. It is a validation failure: nothing is inserted.
	errCertifyEvidence = fmt.Errorf("%w: certification evidence rejected", ErrValidation)
)

type CertifyFileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type CertifyHelpers struct {
	StillHelper     CertifyFileRef `json:"still_helper"`
	AnimationHelper CertifyFileRef `json:"animation_helper"`
	VideoHelper     CertifyFileRef `json:"video_helper"`
	Prlimit         CertifyFileRef `json:"prlimit"`
	FFProbe         CertifyFileRef `json:"ffprobe"`
	SRGBICC         CertifyFileRef `json:"srgb_icc"`
}

type CertifyFixture struct {
	MIMEType string `json:"mime_type"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

// CertifySpec is the strict SPEC.json document. Paths are inputs only; they are
// never written to receipts, audit records, or diagnostics.
type CertifySpec struct {
	ProfileID string           `json:"profile_id"`
	Helpers   CertifyHelpers   `json:"helpers"`
	Fixtures  []CertifyFixture `json:"fixtures"`

	digest string
}

// DecodeCertifySpec strictly decodes one bounded JSON value with no unknown or
// duplicate fields and validates every syntactic constraint before any file or
// database access.
func DecodeCertifySpec(data []byte) (CertifySpec, error) {
	if len(data) == 0 || len(data) > certifySpecMaxBytes {
		return CertifySpec{}, ErrValidation
	}
	if rejectDuplicateKeys(data) != nil {
		return CertifySpec{}, ErrValidation
	}
	var spec CertifySpec
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return CertifySpec{}, ErrValidation
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return CertifySpec{}, ErrValidation
	}
	if err := spec.validate(); err != nil {
		return CertifySpec{}, err
	}
	digest := sha256.Sum256(data)
	spec.digest = hex.EncodeToString(digest[:])
	return spec, nil
}

func (spec CertifySpec) validate() error {
	if !uuidPattern.MatchString(spec.ProfileID) {
		return ErrValidation
	}
	paths := map[string]struct{}{}
	for _, ref := range spec.helperRefs() {
		if !validCertifyRef(ref.ref) {
			return ErrValidation
		}
		if _, duplicate := paths[ref.ref.Path]; duplicate {
			return ErrValidation
		}
		paths[ref.ref.Path] = struct{}{}
	}
	// The Worker refuses any other ICC; certifying a different one would prove
	// a configuration that can never run.
	if spec.Helpers.SRGBICC.SHA256 != certifyRequiredICCSHA256 {
		return ErrValidation
	}
	if len(spec.Fixtures) == 0 || len(spec.Fixtures) > certifyMaxFixtures {
		return ErrValidation
	}
	contents := map[string]struct{}{}
	for _, fixture := range spec.Fixtures {
		if _, ok := mediaformat.Lookup(fixture.MIMEType); !ok || !validCertifyRef(CertifyFileRef{Path: fixture.Path, SHA256: fixture.SHA256}) {
			return ErrValidation
		}
		if _, duplicate := paths[fixture.Path]; duplicate {
			return ErrValidation
		}
		paths[fixture.Path] = struct{}{}
		key := fixture.MIMEType + "\x00" + fixture.SHA256
		if _, duplicate := contents[key]; duplicate {
			return ErrValidation
		}
		contents[key] = struct{}{}
	}
	return nil
}

func validCertifyRef(ref CertifyFileRef) bool {
	return sha256HexPattern.MatchString(ref.SHA256) && ref.Path != "" && filepath.IsAbs(ref.Path) &&
		filepath.Clean(ref.Path) == ref.Path && ref.Path != "/" && !strings.ContainsRune(ref.Path, 0)
}

type certifyNamedRef struct {
	name       string
	ref        CertifyFileRef
	executable bool
}

func (spec CertifySpec) helperRefs() []certifyNamedRef {
	h := spec.Helpers
	return []certifyNamedRef{
		{"still_helper", h.StillHelper, true}, {"animation_helper", h.AnimationHelper, true},
		{"video_helper", h.VideoHelper, true}, {"prlimit", h.Prlimit, true},
		{"ffprobe", h.FFProbe, true}, {"srgb_icc", h.SRGBICC, false},
	}
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > 16 {
			return ErrValidation
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrValidation
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return ErrValidation
				}
				if _, duplicate := seen[name]; duplicate {
					return ErrValidation
				}
				seen[name] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrValidation
		}
		_, err = decoder.Token()
		if err != nil {
			return ErrValidation
		}
		return nil
	}
	return walk(0)
}

// readCertifySpec reads "-" from stdin or a path through an O_NOFOLLOW regular
// file descriptor. Both are bounded to the same size limit.
func readCertifySpec(path string, stdin io.Reader) ([]byte, error) {
	var reader io.Reader
	if path == "-" {
		if stdin == nil {
			return nil, ErrValidation
		}
		reader = stdin
	} else {
		if path == "" || strings.ContainsRune(path, 0) {
			return nil, ErrValidation
		}
		file, info, err := openNoFollow(path)
		if err != nil {
			return nil, ErrValidation
		}
		defer file.Close()
		if !info.Mode().IsRegular() {
			return nil, ErrValidation
		}
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, certifySpecMaxBytes+1))
	if err != nil || len(data) > certifySpecMaxBytes {
		return nil, ErrValidation
	}
	return data, nil
}

func openNoFollow(path string) (*os.File, os.FileInfo, error) {
	// O_NONBLOCK prevents a FIFO/device from blocking before fstat rejects it.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

// certifyOwnerAcceptable is the trusted-binary ownership/mode predicate: a
// regular file owned by root or the invoking user and writable by nobody else.
func certifyOwnerAcceptable(info os.FileInfo, euid int, executable bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || (stat.Uid != 0 && int(stat.Uid) != euid) || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	return !executable || info.Mode().Perm()&0o111 != 0
}

// certifyDirectoryAcceptable rejects a resolved ancestor that another user can
// use to replace a trusted helper. A sticky shared directory is acceptable
// because others cannot rename or unlink entries they do not own.
func certifyDirectoryAcceptable(info os.FileInfo, euid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || (stat.Uid != 0 && int(stat.Uid) != euid) {
		return false
	}
	return info.Mode().Perm()&0o022 == 0 || info.Mode()&os.ModeSticky != 0
}

type certifyVerifiedFile struct {
	name     string
	path     string
	sha256   string
	dev, ino uint64
}

type certifyVerifiedFixture struct {
	index    int
	mimeType string
	sha256   string
	path     string // private workspace copy
}

type certifyWorkspace struct {
	dir      string
	euid     int
	helpers  map[string]certifyVerifiedFile
	fixtures []certifyVerifiedFixture
}

func (ws *certifyWorkspace) Close() error {
	if ws == nil || ws.dir == "" {
		return nil
	}
	return os.RemoveAll(ws.dir)
}

func (ws *certifyWorkspace) helperPath(name string) string { return ws.helpers[name].path }

func (ws *certifyWorkspace) helperDigests() map[string]string {
	result := make(map[string]string, len(ws.helpers))
	for name, helper := range ws.helpers {
		if name != "srgb_icc" {
			result[name] = helper.sha256
		}
	}
	return result
}

// prepareCertifyWorkspace verifies every trusted helper and copies every
// fixture into a private 0700 directory, binding each copy to its declared
// SHA-256. Any mismatch is tampered evidence and creates nothing durable.
func prepareCertifyWorkspace(spec CertifySpec) (*certifyWorkspace, error) {
	ws, err := newCertifyWorkspace(spec)
	if err != nil {
		return nil, err
	}
	if err := ws.copyFixtures(spec); err != nil {
		_ = ws.Close()
		return nil, err
	}
	return ws, nil
}

// newCertifyWorkspace verifies the trusted helpers only. Fixture copies (up
// to certifyMaxTotalFixtureBytes) wait until the cheap profile status and
// duplicate checks have passed (review N6).
func newCertifyWorkspace(spec CertifySpec) (*certifyWorkspace, error) {
	ws := &certifyWorkspace{euid: os.Geteuid(), helpers: map[string]certifyVerifiedFile{}}
	for _, named := range spec.helperRefs() {
		verified, err := verifyCertifyHelper(named, ws.euid)
		if err != nil {
			return nil, err
		}
		ws.helpers[named.name] = verified
	}
	return ws, nil
}

// copyFixtures copies every fixture into a fresh private 0700 directory,
// refusing before and during the copy once the total exceeds
// certifyMaxTotalFixtureBytes.
func (ws *certifyWorkspace) copyFixtures(spec CertifySpec) error {
	dir, err := os.MkdirTemp("", "nmcp-profile-certify-")
	if err != nil {
		return err
	}
	ws.dir = dir
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("private certification directory is not 0700")
	}
	remaining := certifyMaxTotalFixtureBytes
	for index, fixture := range spec.Fixtures {
		label := fmt.Sprintf("fixture[%d]:%s", index, fixture.MIMEType)
		copied, size, err := copyCertifyFixture(fixture, label, filepath.Join(dir, fmt.Sprintf("input-%02d", index)), remaining)
		if err != nil {
			return err
		}
		remaining -= size
		ws.fixtures = append(ws.fixtures, certifyVerifiedFixture{index: index, mimeType: fixture.MIMEType, sha256: fixture.SHA256, path: copied})
	}
	return nil
}

func verifyCertifyHelper(named certifyNamedRef, euid int) (certifyVerifiedFile, error) {
	file, info, err := openNoFollow(named.ref.Path)
	if err != nil {
		return certifyVerifiedFile{}, certifyInputRejected(named.name, "open")
	}
	defer file.Close()
	if !certifyOwnerAcceptable(info, euid, named.executable) {
		return certifyVerifiedFile{}, certifyInputRejected(named.name, "owner_mode")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(named.ref.Path))
	if err != nil {
		return certifyVerifiedFile{}, certifyInputRejected(named.name, "ancestor")
	}
	for current := resolved; ; current = filepath.Dir(current) {
		dirInfo, err := os.Lstat(current)
		if err != nil || !certifyDirectoryAcceptable(dirInfo, euid) {
			return certifyVerifiedFile{}, certifyInputRejected(named.name, "ancestor")
		}
		if current == "/" || current == filepath.Dir(current) {
			break
		}
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return certifyVerifiedFile{}, err
	}
	if hex.EncodeToString(digest.Sum(nil)) != named.ref.SHA256 {
		return certifyVerifiedFile{}, certifyInputRejected(named.name, "digest")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return certifyVerifiedFile{name: named.name, path: named.ref.Path, sha256: named.ref.SHA256, dev: uint64(stat.Dev), ino: stat.Ino}, nil
}

// reverifyHelpers detects a helper replaced or rewritten while it was in use.
func (ws *certifyWorkspace) reverifyHelpers() error {
	for _, helper := range ws.helpers {
		again, err := verifyCertifyHelper(certifyNamedRef{name: helper.name, ref: CertifyFileRef{Path: helper.path, SHA256: helper.sha256}, executable: helper.name != "srgb_icc"}, ws.euid)
		if err != nil {
			return err
		}
		if again.dev != helper.dev || again.ino != helper.ino {
			return errCertifyEvidence
		}
	}
	return nil
}

func copyCertifyFixture(fixture CertifyFixture, label, destination string, budget int64) (string, int64, error) {
	source, info, err := openNoFollow(fixture.Path)
	if err != nil {
		return "", 0, certifyInputRejected(label, "open")
	}
	defer source.Close()
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > certifyMaxFixtureBytes {
		return "", 0, certifyInputRejected(label, "type_size")
	}
	if info.Size() > budget {
		return "", 0, certifyInputRejected(label, "total_size")
	}
	limit := min(certifyMaxFixtureBytes, budget)
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, digest), io.LimitReader(source, limit+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		return "", 0, errors.Join(copyErr, closeErr)
	}
	if size <= 0 || size > certifyMaxFixtureBytes {
		return "", 0, certifyInputRejected(label, "type_size")
	}
	if size > budget {
		return "", 0, certifyInputRejected(label, "total_size")
	}
	if hex.EncodeToString(digest.Sum(nil)) != fixture.SHA256 {
		return "", 0, certifyInputRejected(label, "digest")
	}
	if err := os.Chmod(destination, 0o400); err != nil {
		return "", 0, err
	}
	return destination, size, nil
}

// certifyRequirement is one exact row the activation validator needs.
type certifyRequirement struct {
	MIMEType   string             `json:"input_mime_type"`
	SourceMode profile.SourceMode `json:"source_mode"`
	OutputKind profile.OutputKind `json:"output_kind"`
	MaxEdge    int                `json:"max_long_edge"`
	MinSetting int                `json:"minimum_setting"`
	MaxSetting int                `json:"maximum_setting"`
}

// certificationRequirements derives exactly the rows nmcp_profile_recipe_certified
// (migration 0024) consults: one per Worker-executable (MIME, recipe output) at
// the recipe edge and setting, from profile.WorkerExecutableOutputs.
func certificationRequirements(definition profile.Definition) ([]certifyRequirement, map[string]profile.Recipe, error) {
	requirements, recipes, err := certifyWorkerRequirements(definition)
	if err != nil {
		return nil, nil, err
	}
	for _, mimeType := range definition.InputMIMETypes {
		if err := certifyProcessorEnvelope(mimeType, recipes[mimeType]); err != nil {
			return nil, nil, err
		}
	}
	return requirements, recipes, nil
}

// certifyWorkerRequirements is the pure validator-parity derivation, without
// the processors' static envelope preflight.
func certifyWorkerRequirements(definition profile.Definition) ([]certifyRequirement, map[string]profile.Recipe, error) {
	if profile.ValidateDraft(definition) != nil {
		return nil, nil, ErrValidation
	}
	parameters, err := profile.DecodeParameters(definition.Parameters)
	if err != nil {
		return nil, nil, ErrValidation
	}
	var requirements []certifyRequirement
	for _, mimeType := range definition.InputMIMETypes {
		recipe := parameters.Recipes[mimeType]
		for _, output := range profile.WorkerExecutableOutputs(mimeType, recipe) {
			requirements = append(requirements, certifyRequirement{MIMEType: mimeType, SourceMode: recipe.SourceMode,
				OutputKind: output.Kind, MaxEdge: recipe.MaxLongEdge, MinSetting: output.Setting, MaxSetting: output.Setting})
		}
	}
	return requirements, parameters.Recipes, nil
}

// certifyProcessorEnvelope is an early fail-closed preflight mirroring the
// processors' static request limits, so an unsupported edge/setting is refused
// before any helper runs. The processors remain authoritative at run time.
func certifyProcessorEnvelope(mimeType string, recipe profile.Recipe) error {
	switch recipe.SourceMode {
	case profile.SourceStill, profile.SourceProbeAnimation:
		if recipe.MaxLongEdge > 1920 {
			return ErrValidation
		}
	case profile.SourceVideo:
		if recipe.VideoOutput != nil && (recipe.MaxLongEdge > 1920 || recipe.VideoOutput.CRF != 32) {
			return ErrValidation
		}
		if recipe.StillOutput != nil && recipe.MaxLongEdge > 640 {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	return nil
}

// checkFixtureMIMEs requires every fixture to be a required MIME and every
// required MIME to have a fixture. Partial certification is not allowed.
func checkFixtureMIMEs(definition profile.Definition, fixtures []certifyVerifiedFixture) error {
	present := map[string]bool{}
	for _, fixture := range fixtures {
		if !slices.Contains(definition.InputMIMETypes, fixture.mimeType) {
			return certifyInputRejected(certifyFixtureLabel(fixture), "mime_not_in_profile")
		}
		present[fixture.mimeType] = true
	}
	for _, mimeType := range definition.InputMIMETypes {
		if !present[mimeType] {
			return certifyInputRejected("profile:"+mimeType, "mime_without_fixture")
		}
	}
	return nil
}

// certifyFailureError carries a fixed, path-free reason code for a refused
// certification so the operator sees why without any helper or file text.
type certifyFailureError struct {
	reason      string
	requirement certifyRequirement
	input       string
	check       string
	detail      string
}

const (
	certifyReasonRequirementUncovered = "requirement_not_covered"
	// certifyReasonInputEvidence names which spec input failed which fixed
	// self-authentication check, so a refused run is diagnosable without
	// paths: input is a helper name or "fixture[N]:<mime>", check is one of
	// open, owner_mode, ancestor, digest, type_size.
	certifyReasonInputEvidence = "input_evidence_rejected"
)

func certifyInputRejected(input, check string) error {
	return &certifyFailureError{reason: certifyReasonInputEvidence, input: input, check: check}
}

// certifySafeToken bounds a probe-reported name to [a-z0-9/_.,+-] for detail.
func certifySafeToken(value string) string {
	if len(value) > 48 {
		value = value[:48]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("/_.,+-", r) {
			return r
		}
		return '?'
	}, value)
}

func certifyFixtureLabel(fixture certifyVerifiedFixture) string {
	return fmt.Sprintf("fixture[%d]:%s", fixture.index, fixture.mimeType)
}

// certifyTagValidation names the input and check of an otherwise anonymous
// validation refusal; operational errors and already-named refusals pass
// through unchanged.
func certifyTagValidation(err error, input, check string) error {
	var named *certifyFailureError
	if !errors.Is(err, ErrValidation) || errors.As(err, &named) {
		return err
	}
	return errors.Join(certifyInputRejected(input, check), err)
}

func (e *certifyFailureError) Error() string { return "certification refused: " + e.reason }
func (e *certifyFailureError) Unwrap() error { return ErrValidation }

// CertifyFailure is the sanitized CLI result for a refused requirement.
type CertifyFailure struct {
	ProfileID   string             `json:"profile_id"`
	Reason      string             `json:"reason"`
	Requirement certifyRequirement `json:"requirement"`
	Input       string             `json:"input,omitempty"`
	Check       string             `json:"check,omitempty"`
	// Detail carries only the compared numbers and sanitized codec/format
	// names of an output-evidence refusal; never paths or helper text.
	Detail string `json:"detail,omitempty"`
}

func certifyFailureResult(profileID string, err error) any {
	var failure *certifyFailureError
	if !errors.As(err, &failure) {
		return nil
	}
	return CertifyFailure{ProfileID: profileID, Reason: failure.reason, Requirement: failure.requirement, Input: failure.input, Check: failure.check, Detail: failure.detail}
}

type certifyRoute string

const (
	certifyRouteStill     certifyRoute = "still-processor"
	certifyRouteAnimation certifyRoute = "animation-processor"
	certifyRouteVideo     certifyRoute = "video-processor"
)

type certifyInputFacts struct {
	// Classification is "still", "static", "animation", or "video".
	Classification   string
	VideoStreamIndex int
}

type certifyRun struct {
	Fixture    certifyVerifiedFixture
	Facts      certifyInputFacts
	Route      certifyRoute
	OutputKind profile.OutputKind
	Recipe     profile.Recipe
	Setting    int
}

// certifyRunsFor reproduces the Worker's transformexecutor routing for one
// fixture and nothing else. A GIF under an all-frames recipe therefore yields
// only animation-webp, matching profile.WorkerExecutableOutputs.
func certifyRunsFor(fixture certifyVerifiedFixture, recipe profile.Recipe, facts certifyInputFacts) ([]certifyRun, error) {
	run := func(route certifyRoute, kind profile.OutputKind, r profile.Recipe, setting int) certifyRun {
		return certifyRun{Fixture: fixture, Facts: facts, Route: route, OutputKind: kind, Recipe: r, Setting: setting}
	}
	switch recipe.SourceMode {
	case profile.SourceStill:
		if facts.Classification != "still" || recipe.StillOutput == nil {
			return nil, ErrValidation
		}
		return []certifyRun{run(certifyRouteStill, profile.OutputStillAVIF, recipe, recipe.StillOutput.Quality)}, nil
	case profile.SourceProbeAnimation:
		if recipe.StillOutput == nil {
			return nil, ErrValidation
		}
		if fixture.mimeType == "image/webp" && facts.Classification == "static" {
			return []certifyRun{run(certifyRouteStill, profile.OutputStillAVIF, recipe, recipe.StillOutput.Quality)}, nil
		}
		if facts.Classification != "animation" {
			return nil, errCertifyEvidence
		}
		if recipe.FramePolicy == profile.FrameFirst {
			return []certifyRun{run(certifyRouteAnimation, profile.OutputStillAVIF, recipe, recipe.StillOutput.Quality)}, nil
		}
		if recipe.AnimationOutput == nil {
			return nil, ErrValidation
		}
		return []certifyRun{run(certifyRouteAnimation, profile.OutputAnimationWebP, recipe, recipe.AnimationOutput.Quality)}, nil
	case profile.SourceVideo:
		if facts.Classification != "video" || facts.VideoStreamIndex < 0 {
			return nil, errCertifyEvidence
		}
		if recipe.VideoOutput != nil {
			return []certifyRun{run(certifyRouteVideo, profile.OutputVideoAV1, recipe, recipe.VideoOutput.CRF)}, nil
		}
		if recipe.StillOutput != nil {
			return []certifyRun{run(certifyRouteVideo, profile.OutputStillAVIF, recipe, recipe.StillOutput.Quality)}, nil
		}
	}
	return nil, ErrValidation
}

func certifyCovers(run certifyRun, requirement certifyRequirement) bool {
	return run.Fixture.mimeType == requirement.MIMEType && run.OutputKind == requirement.OutputKind &&
		run.Recipe.MaxLongEdge == requirement.MaxEdge && run.Setting >= requirement.MinSetting && run.Setting <= requirement.MaxSetting
}

type certifyProcessorIdentity struct {
	HelperVersion    string            `json:"helper_version"`
	LibraryVersions  map[string]string `json:"library_versions"`
	BuildManifest    string            `json:"build_manifest,omitempty"`
	Encoders         []string          `json:"encoders"`
	DecoderMIMETypes []string          `json:"decoder_mime_types"`
}

type certifyTransformFacts struct {
	OutputMIME    string
	Width, Height int
	Setting       int
	BitDepth      int
	MaxLongEdge   int
	SourceWidth   int
	SourceHeight  int
	FrameCount    int
	AudioPresent  bool
	Decoder       string
	Encoder       string
	ToolVersion   string
}

type certifyProbeFacts struct {
	Codec        string
	Width        int
	Height       int
	PixelFormat  string
	Container    string
	VideoStreams int
	AudioStreams int
	AudioCodec   string
	// FrameCount is set only by the structural animated-WebP parser.
	FrameCount int
}

// certifyRuntime is the injectable seam over the real processors and the
// independent ffprobe probe. Production uses newProductionCertifyRuntime.
type certifyRuntime interface {
	Identity(context.Context) (map[string]certifyProcessorIdentity, error)
	Inspect(context.Context, string, *os.File) (certifyInputFacts, error)
	Transform(context.Context, certifyRun, *os.File, *os.File) (certifyTransformFacts, error)
	Probe(context.Context, profile.OutputKind, *os.File) (certifyProbeFacts, error)
}

func certifyRouteFamily(route certifyRoute) string {
	switch route {
	case certifyRouteStill:
		return "still"
	case certifyRouteVideo:
		return "video"
	default:
		return "animation"
	}
}

// evaluateCertifyRun is the fail-closed comparison of the processor's own
// validated result, the independent probe, and the requirement.
func evaluateCertifyRun(run certifyRun, transformed certifyTransformFacts, probed certifyProbeFacts) error {
	edge := run.Recipe.MaxLongEdge
	reject := func(check string) error {
		return &certifyFailureError{reason: certifyReasonInputEvidence, input: certifyFixtureLabel(run.Fixture) + "->" + string(run.OutputKind), check: check,
			detail: fmt.Sprintf("transform=%s %dx%d src=%dx%d edge=%d setting=%d depth=%d frames=%d probe=%s %dx%d video=%d audio=%d/%s frames=%d pix=%s container=%s",
				certifySafeToken(transformed.OutputMIME), transformed.Width, transformed.Height, transformed.SourceWidth, transformed.SourceHeight, edge,
				transformed.Setting, transformed.BitDepth, transformed.FrameCount, certifySafeToken(probed.Codec), probed.Width, probed.Height,
				probed.VideoStreams, probed.AudioStreams, certifySafeToken(probed.AudioCodec), probed.FrameCount, certifySafeToken(probed.PixelFormat), certifySafeToken(probed.Container))}
	}
	expectedMIME, codec, bitDepth := "image/avif", "av1", 8
	switch run.OutputKind {
	case profile.OutputAnimationWebP:
		expectedMIME, codec = "image/webp", "webp"
	case profile.OutputVideoAV1:
		expectedMIME, bitDepth = "video/mp4", 10
	}
	t := transformed
	if t.OutputMIME != expectedMIME || t.Setting != run.Setting || t.BitDepth != bitDepth || t.MaxLongEdge != edge ||
		t.Width <= 0 || t.Height <= 0 || t.SourceWidth <= 0 || t.SourceHeight <= 0 {
		return reject("output_identity")
	}
	// Each fixture must exercise the certified edge: the output long edge IS the
	// edge, with no upscale and the source aspect preserved.
	sourceLong := max(t.SourceWidth, t.SourceHeight)
	if sourceLong < edge || max(t.Width, t.Height) != edge || t.Width > t.SourceWidth || t.Height > t.SourceHeight {
		return reject("edge")
	}
	tolerance := int64(sourceLong)
	if run.OutputKind == profile.OutputVideoAV1 {
		tolerance *= 2
		if t.Width%2 != 0 || t.Height%2 != 0 {
			return reject("even_dimensions")
		}
	}
	skew := int64(t.Width)*int64(t.SourceHeight) - int64(t.Height)*int64(t.SourceWidth)
	if skew < -tolerance || skew > tolerance {
		return reject("aspect")
	}
	p := probed
	// The pinned FFmpeg 9 ffprobe names animated WebP's decoder webp_anim (older
	// ffprobe: webp); the RIFF ANIM/ANMF walk in Probe proves animation either way.
	codecMatches := p.Codec == codec || run.OutputKind == profile.OutputAnimationWebP && p.Codec == "webp_anim"
	if !codecMatches || p.Width != t.Width || p.Height != t.Height || p.VideoStreams < 1 {
		return reject("probe_identity")
	}
	switch run.OutputKind {
	case profile.OutputStillAVIF:
		if p.AudioStreams != 0 {
			return reject("probe_audio")
		}
	case profile.OutputAnimationWebP:
		if p.AudioStreams != 0 || p.FrameCount < 1 || p.FrameCount != t.FrameCount {
			return reject("probe_frames")
		}
	case profile.OutputVideoAV1:
		audio := 0
		if t.AudioPresent {
			audio = 1
		}
		if p.VideoStreams != 1 || p.AudioStreams != audio || (audio == 1 && p.AudioCodec != "aac") ||
			p.PixelFormat != "yuv420p10le" || !slices.Contains(strings.Split(p.Container, ","), "mp4") {
			return reject("probe_video_container")
		}
	default:
		return reject("output_kind")
	}
	return nil
}

type certifyRunReceipt struct {
	Route        certifyRoute `json:"route"`
	InputSHA256  string       `json:"input_sha256"`
	OutputSHA256 string       `json:"output_sha256"`
	OutputBytes  int64        `json:"output_bytes"`
	OutputMIME   string       `json:"output_mime"`
	Codec        string       `json:"probe_codec"`
	PixelFormat  string       `json:"probe_pixel_format,omitempty"`
	Container    string       `json:"probe_container,omitempty"`
	Width        int          `json:"width"`
	Height       int          `json:"height"`
	SourceWidth  int          `json:"source_width"`
	SourceHeight int          `json:"source_height"`
	FrameCount   int          `json:"frame_count,omitempty"`
	AudioPresent bool         `json:"audio_present"`
	Setting      int          `json:"setting"`
	BitDepth     int          `json:"bit_depth"`
	Decoder      string       `json:"decoder,omitempty"`
	Encoder      string       `json:"encoder,omitempty"`
	ToolVersion  string       `json:"tool_version,omitempty"`
}

type certifyProfileRef struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}

type certifyActor struct {
	Actor string `json:"actor"`
	Host  string `json:"host"`
}

// certifyReceipt is the canonical evidence text: encoding/json emits struct
// fields in declaration order and map keys sorted. No path is recorded.
type certifyReceipt struct {
	Schema                  string                              `json:"schema"`
	Scope                   string                              `json:"scope"`
	Profile                 certifyProfileRef                   `json:"profile"`
	Processor               string                              `json:"processor"`
	ParametersSchemaVersion int                                 `json:"parameters_schema_version"`
	Requirement             certifyRequirement                  `json:"requirement"`
	HelperSHA256            map[string]string                   `json:"helper_sha256"`
	ICCSHA256               string                              `json:"icc_sha256"`
	ProcessorIdentity       map[string]certifyProcessorIdentity `json:"processor_identity"`
	Runs                    []certifyRunReceipt                 `json:"runs"`
	NMCPAdmin               map[string]string                   `json:"nmcp_admin"`
	CertifiedBy             certifyActor                        `json:"certified_by"`
}

type certifyRow struct {
	ID          string
	Requirement certifyRequirement
	Evidence    string
	Runs        int
}

func adminBuildIdentity() map[string]string {
	result := map[string]string{"module_version": "unknown", "go_version": "unknown", "vcs_revision": "unknown", "vcs_modified": "unknown"}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	result["go_version"] = info.GoVersion
	if info.Main.Version != "" {
		result["module_version"] = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			result["vcs_revision"] = setting.Value
		case "vcs.modified":
			result["vcs_modified"] = setting.Value
		}
	}
	return result
}

// certifyProfile runs every real transform and probe for a loaded draft and
// returns the rows to insert, or an error with nothing to insert. It touches no
// database state, which keeps it unit-testable through the runtime seam.
func certifyProfile(ctx context.Context, definition profile.Definition, ws *certifyWorkspace, rt certifyRuntime, actor certifyActor) ([]certifyRow, error) {
	requirements, recipes, err := certificationRequirements(definition)
	if err != nil {
		return nil, err
	}
	if err := checkFixtureMIMEs(definition, ws.fixtures); err != nil {
		return nil, err
	}
	identity, err := rt.Identity(ctx)
	if err != nil {
		return nil, err
	}
	var runs []certifyRun
	for _, fixture := range ws.fixtures {
		input, err := os.Open(fixture.path)
		if err != nil {
			return nil, err
		}
		facts, err := rt.Inspect(ctx, fixture.mimeType, input)
		_ = input.Close()
		if err != nil {
			return nil, certifyTagValidation(err, certifyFixtureLabel(fixture), "inspect")
		}
		fixtureRuns, err := certifyRunsFor(fixture, recipes[fixture.mimeType], facts)
		if err != nil {
			return nil, certifyTagValidation(err, certifyFixtureLabel(fixture), "routing")
		}
		for _, run := range fixtureRuns {
			family, ok := identity[certifyRouteFamily(run.Route)]
			if !ok || !slices.Contains(family.DecoderMIMETypes, fixture.mimeType) {
				// The helper does not advertise this input MIME, so the Worker
				// would refuse it too; certification must not claim it.
				return nil, certifyInputRejected(certifyFixtureLabel(fixture), "decoder_not_advertised")
			}
		}
		runs = append(runs, fixtureRuns...)
	}
	for _, requirement := range requirements {
		if !slices.ContainsFunc(runs, func(run certifyRun) bool { return certifyCovers(run, requirement) }) {
			return nil, &certifyFailureError{reason: certifyReasonRequirementUncovered, requirement: requirement}
		}
	}
	receipts := make([]certifyRunReceipt, 0, len(runs))
	for index, run := range runs {
		receipt, err := executeCertifyRun(ctx, ws, rt, run, index)
		if err != nil {
			return nil, certifyTagValidation(err, certifyFixtureLabel(run.Fixture)+"->"+string(run.OutputKind), "run")
		}
		receipts = append(receipts, receipt)
	}
	if err := ws.reverifyHelpers(); err != nil {
		return nil, err
	}
	build := adminBuildIdentity()
	rows := make([]certifyRow, 0, len(requirements))
	for _, requirement := range requirements {
		var covered []certifyRunReceipt
		for index, run := range runs {
			if certifyCovers(run, requirement) {
				covered = append(covered, receipts[index])
			}
		}
		encoded, err := json.Marshal(certifyReceipt{
			Schema: certifyReceiptSchema, Scope: certifyReceiptScope,
			Profile:   certifyProfileRef{ID: definition.ID, Key: definition.Key, Version: definition.Version},
			Processor: definition.Processor, ParametersSchemaVersion: definition.ParametersSchemaVersion,
			Requirement: requirement, HelperSHA256: ws.helperDigests(), ICCSHA256: ws.helpers["srgb_icc"].sha256,
			ProcessorIdentity: identity, Runs: covered, NMCPAdmin: build, CertifiedBy: actor,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, certifyRow{ID: newID(), Requirement: requirement, Evidence: string(encoded), Runs: len(covered)})
	}
	return rows, nil
}

func executeCertifyRun(ctx context.Context, ws *certifyWorkspace, rt certifyRuntime, run certifyRun, index int) (certifyRunReceipt, error) {
	input, err := os.Open(run.Fixture.path)
	if err != nil {
		return certifyRunReceipt{}, err
	}
	defer input.Close()
	outputPath := filepath.Join(ws.dir, fmt.Sprintf("output-%02d", index))
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return certifyRunReceipt{}, err
	}
	defer func() {
		_ = output.Close()
		_ = os.Remove(outputPath)
	}()
	transformed, err := rt.Transform(ctx, run, input, output)
	if err != nil {
		return certifyRunReceipt{}, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return certifyRunReceipt{}, err
	}
	probed, err := rt.Probe(ctx, run.OutputKind, output)
	if err != nil {
		return certifyRunReceipt{}, err
	}
	if err := evaluateCertifyRun(run, transformed, probed); err != nil {
		return certifyRunReceipt{}, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return certifyRunReceipt{}, err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, output)
	if err != nil || size <= 0 {
		return certifyRunReceipt{}, errCertifyEvidence
	}
	return certifyRunReceipt{
		Route: run.Route, InputSHA256: run.Fixture.sha256, OutputSHA256: hex.EncodeToString(digest.Sum(nil)), OutputBytes: size,
		OutputMIME: transformed.OutputMIME, Codec: probed.Codec, PixelFormat: probed.PixelFormat, Container: probed.Container,
		Width: transformed.Width, Height: transformed.Height, SourceWidth: transformed.SourceWidth, SourceHeight: transformed.SourceHeight,
		FrameCount: transformed.FrameCount, AudioPresent: transformed.AudioPresent, Setting: transformed.Setting, BitDepth: transformed.BitDepth,
		Decoder: transformed.Decoder, Encoder: transformed.Encoder, ToolVersion: transformed.ToolVersion,
	}, nil
}

// CertifyResult is the CLI result. It contains no evidence paths.
type CertifyResult struct {
	ProfileID          string                 `json:"profile_id"`
	Status             string                 `json:"status"`
	ProfileStatus      string                 `json:"profile_status"`
	ActivationVerified bool                   `json:"activation_certification_verified"`
	Certifications     []CertifyResultSummary `json:"certifications"`
}

type CertifyResultSummary struct {
	ID string `json:"id"`
	certifyRequirement
	Runs int `json:"runs"`
}

type certifyLoadedProfile struct {
	definition profile.Definition
	status     string
	identity   certifyHelperIdentity
}

func (s *Service) loadCertifyProfile(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string, lock string) (certifyLoadedProfile, error) {
	var loaded certifyLoadedProfile
	d := &loaded.definition
	err := q.QueryRow(ctx, `SELECT id::text,key,version,status,input_mime_types,processor,parameters_schema_version,parameters
		FROM profiles WHERE id=$1`+lock, id).Scan(&d.ID, &d.Key, &d.Version, &loaded.status, &d.InputMIMETypes,
		&d.Processor, &d.ParametersSchemaVersion, &d.Parameters)
	if errors.Is(err, pgx.ErrNoRows) {
		return loaded, ErrConflict
	}
	return loaded, err
}

const certifyDuplicateSQL = `SELECT evidence FROM profile_processor_certifications
	WHERE processor=$1 AND parameters_schema_version=$2 AND input_mime_type=$3 AND source_mode=$4
	AND output_kind=$5 AND max_long_edge=$6 AND minimum_setting=$7 AND maximum_setting=$8`

// certifyHelperIdentity is the digest set that makes two certifications of the
// same requirement identical: the five helper binaries plus the sRGB ICC.
type certifyHelperIdentity struct {
	helpers map[string]string
	icc     string
}

func (ws *certifyWorkspace) helperIdentity() certifyHelperIdentity {
	return certifyHelperIdentity{helpers: ws.helperDigests(), icc: ws.helpers["srgb_icc"].sha256}
}

var certifyHelperDigestNames = []string{"animation_helper", "ffprobe", "prlimit", "still_helper", "video_helper"}

// certifyEvidenceIdentical strictly parses an existing row's evidence. Only a
// well-formed receipt of this schema for the same requirement whose helper and
// ICC digests are byte-identical counts; anything malformed or foreign is "not
// identical", so the new evidence is appended rather than refused.
func certifyEvidenceIdentical(evidence string, definition profile.Definition, requirement certifyRequirement, identity certifyHelperIdentity) bool {
	data := []byte(evidence)
	if len(data) == 0 || len(data) > certifySpecMaxBytes || rejectDuplicateKeys(data) != nil {
		return false
	}
	var receipt certifyReceipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	if receipt.Schema != certifyReceiptSchema || receipt.Processor != definition.Processor ||
		receipt.ParametersSchemaVersion != definition.ParametersSchemaVersion || receipt.Requirement != requirement ||
		!sha256HexPattern.MatchString(receipt.ICCSHA256) || receipt.ICCSHA256 != identity.icc ||
		len(receipt.HelperSHA256) != len(certifyHelperDigestNames) || len(identity.helpers) != len(certifyHelperDigestNames) {
		return false
	}
	for _, name := range certifyHelperDigestNames {
		recorded, ok := receipt.HelperSHA256[name]
		if !ok || !sha256HexPattern.MatchString(recorded) || recorded != identity.helpers[name] {
			return false
		}
	}
	return true
}

// certifyDuplicate refuses (exit 4) only when a requirement already has a row
// produced by byte-identical helpers. It reads; it never updates or deletes.
func certifyDuplicate(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, definition profile.Definition, requirements []certifyRequirement, identity certifyHelperIdentity) error {
	for _, r := range requirements {
		rows, err := q.Query(ctx, certifyDuplicateSQL, definition.Processor, definition.ParametersSchemaVersion, r.MIMEType,
			string(r.SourceMode), string(r.OutputKind), r.MaxEdge, r.MinSetting, r.MaxSetting)
		if err != nil {
			return err
		}
		identical := false
		for rows.Next() {
			var evidence string
			if err := rows.Scan(&evidence); err != nil {
				rows.Close()
				return err
			}
			identical = identical || certifyEvidenceIdentical(evidence, definition, r, identity)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if identical {
			return ErrConflict
		}
	}
	return nil
}

func sameCertifyDefinition(a, b profile.Definition) bool {
	var left, right any
	if json.Unmarshal(a.Parameters, &left) != nil || json.Unmarshal(b.Parameters, &right) != nil {
		return false
	}
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return a.ID == b.ID && a.Key == b.Key && a.Version == b.Version && a.Processor == b.Processor &&
		a.ParametersSchemaVersion == b.ParametersSchemaVersion && slices.Equal(a.InputMIMETypes, b.InputMIMETypes) && bytes.Equal(l, r)
}

// ProfileCertify certifies every requirement of one draft profile or nothing.
func (s *Service) ProfileCertify(ctx context.Context, spec CertifySpec) (CertifyResult, error) {
	if spec.digest == "" || spec.validate() != nil {
		return CertifyResult{}, ErrValidation
	}
	args := map[string]any{"profile_id": spec.ProfileID, "spec_sha256": spec.digest, "fixtures": len(spec.Fixtures)}
	loaded, rows, err := s.prepareProfileCertification(ctx, spec)
	if err != nil {
		// Failures before the mutation transaction still leave a sanitized audit.
		if failure, ok := certifyFailureResult(spec.ProfileID, err).(CertifyFailure); ok {
			args["failure_reason"] = failure.Reason
			if failure.Input != "" {
				args["failure_input"], args["failure_check"] = failure.Input, failure.Check
			}
		}
		if s.Pool != nil {
			_ = s.recordAudit(ctx, certifyAuditCommand, args, []string{spec.ProfileID}, "failed")
		}
		return CertifyResult{}, classified(err)
	}
	args["certifications"] = len(rows)
	ids := []string{spec.ProfileID}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	definition := loaded.definition
	err = s.mutate(ctx, certifyAuditCommand, "profile:"+definition.Key, args, ids, func(tx pgx.Tx) error {
		if err := normal(ctx, tx); err != nil {
			return err
		}
		// Same key-scoped lock as ProfileTransition and the database lifecycle
		// guard: certification cannot interleave with activation of this key.
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1,hashtext($2))`, certifyProfileLockClass, definition.Key).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrConflict
		}
		current, err := s.loadCertifyProfile(ctx, tx, definition.ID, ` FOR SHARE NOWAIT`)
		if err != nil {
			return err
		}
		if current.status != "draft" || !sameCertifyDefinition(current.definition, definition) {
			return ErrConflict
		}
		requirements := make([]certifyRequirement, len(rows))
		for index, row := range rows {
			requirements[index] = row.Requirement
		}
		if err := certifyDuplicate(ctx, tx, definition, requirements, loaded.identity); err != nil {
			return err
		}
		for _, row := range rows {
			r := row.Requirement
			if _, err := tx.Exec(ctx, `INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,
				input_mime_type,source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, row.ID, definition.Processor, definition.ParametersSchemaVersion,
				r.MIMEType, string(r.SourceMode), string(r.OutputKind), r.MaxEdge, r.MinSetting, r.MaxSetting, row.Evidence); err != nil {
				return err
			}
		}
		// A successful certify guarantees the activation certification check.
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT nmcp_profile_definition_valid(input_mime_types,processor,
			parameters_schema_version,parameters,true) FROM profiles WHERE id=$1`, definition.ID).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrValidation
		}
		return nil
	})
	if err != nil {
		return CertifyResult{}, err
	}
	result := CertifyResult{ProfileID: definition.ID, Status: certifyStatusCertified, ProfileStatus: "draft", ActivationVerified: true}
	for _, row := range rows {
		result.Certifications = append(result.Certifications, CertifyResultSummary{ID: row.ID, certifyRequirement: row.Requirement, Runs: row.Runs})
	}
	return result, nil
}

func (s *Service) prepareProfileCertification(ctx context.Context, spec CertifySpec) (certifyLoadedProfile, []certifyRow, error) {
	ws, err := newCertifyWorkspace(spec)
	if err != nil {
		return certifyLoadedProfile{}, nil, err
	}
	defer ws.Close()
	if s.Pool == nil {
		return certifyLoadedProfile{}, nil, ErrValidation
	}
	loaded, err := s.loadCertifyProfile(ctx, s.Pool, spec.ProfileID, "")
	if err != nil {
		return loaded, nil, err
	}
	loaded.identity = ws.helperIdentity()
	if loaded.status != "draft" {
		return loaded, nil, ErrConflict
	}
	requirements, _, err := certificationRequirements(loaded.definition)
	if err != nil {
		if errors.Is(err, ErrValidation) {
			return loaded, nil, errors.Join(certifyInputRejected("profile", "requirements"), err)
		}
		return loaded, nil, err
	}
	// Read-only early refusal; the transaction repeats it under the lock.
	if err := certifyDuplicate(ctx, s.Pool, loaded.definition, requirements, loaded.identity); err != nil {
		return loaded, nil, err
	}
	// Only now copy the fixtures (review N6): a non-draft or already
	// certified profile never costs up to 64 x 2 GiB of copying.
	if err := ws.copyFixtures(spec); err != nil {
		return loaded, nil, err
	}
	newRuntime := s.newCertifyRuntime
	if newRuntime == nil {
		newRuntime = newProductionCertifyRuntime
	}
	rt, err := newRuntime(ws)
	if err != nil {
		return loaded, nil, err
	}
	rows, err := certifyProfile(ctx, loaded.definition, ws, rt, certifyActor{Actor: s.Actor, Host: s.Host})
	return loaded, rows, err
}
