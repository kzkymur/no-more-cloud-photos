package job

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	profiledefinition "github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformcapability"
)

func TestNewRepositoryOptions(t *testing.T) {
	const fileBaseURL = "https://files.example.test/files/"
	if _, err := NewRepository(nil, Options{FileBaseURL: fileBaseURL}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil pool error = %v", err)
	}
	pool := &pgxpool.Pool{}
	for _, options := range []Options{
		{FileBaseURL: fileBaseURL, LeaseDuration: -time.Second},
		{FileBaseURL: fileBaseURL, LeaseDuration: time.Nanosecond},
		{FileBaseURL: fileBaseURL, ReclaimBatch: -1},
		{FileBaseURL: fileBaseURL, ReclaimBatch: 51},
		{FileBaseURL: "http://files.example.test/files/"},
	} {
		if _, err := NewRepository(pool, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid options %+v error = %v", options, err)
		}
	}
	repository, err := NewRepository(pool, Options{FileBaseURL: fileBaseURL})
	if err != nil || repository.leaseDuration != 2*time.Minute || repository.reclaimBatch != 50 || repository.jitter == nil || repository.uuid == nil {
		t.Fatalf("default repository = %+v, %v", repository, err)
	}
}

func TestBindTransformClaimsRejectsEmptyEnvelope(t *testing.T) {
	repository := &Repository{}
	if _, err := repository.BindTransformClaims(transformcapability.Envelope{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty envelope error = %v", err)
	}
}

func TestTransformClaimerRejectsUnboundConstruction(t *testing.T) {
	typeOfClaimer := reflect.TypeOf(TransformClaimer{})
	for index := 0; index < typeOfClaimer.NumField(); index++ {
		if typeOfClaimer.Field(index).IsExported() {
			t.Fatalf("TransformClaimer exposes forgeable field %q", typeOfClaimer.Field(index).Name)
		}
	}
	for _, claimer := range []*TransformClaimer{
		{},
		{repository: &Repository{}, bound: true},
		{repository: &Repository{}, profileIDs: []string{}},
	} {
		if _, err := claimer.Claim(context.Background(), []Type{TypeTransform}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unbound claimer error = %v", err)
		}
	}
	if _, err := (&Repository{}).claim(context.Background(), []Type{TypeTransform}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil profile snapshot error = %v", err)
	}
}

func TestBackoffMaximumIsBoundedWithoutOverflow(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: 5 * time.Second},
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 8, want: 10*time.Minute + 40*time.Second},
		{attempt: 9, want: 15 * time.Minute},
		{attempt: 1_000_000, want: 15 * time.Minute},
	}
	for _, test := range tests {
		if got := backoffMaximum(test.attempt); got != test.want {
			t.Errorf("backoffMaximum(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestRetryDelayValidatesInjectedFullJitter(t *testing.T) {
	repository := &Repository{jitter: func(maximum time.Duration) time.Duration { return maximum }}
	if got, err := repository.retryDelay(3); err != nil || got != 20*time.Second {
		t.Fatalf("retryDelay valid boundary = %s, %v", got, err)
	}
	repository.jitter = func(maximum time.Duration) time.Duration { return maximum + 1 }
	if _, err := repository.retryDelay(3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retryDelay excessive jitter error = %v", err)
	}
	repository.jitter = func(time.Duration) time.Duration { return -1 }
	if _, err := repository.retryDelay(3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retryDelay negative jitter error = %v", err)
	}
}

func TestFailureCodesHaveOnlyFixedSafeMessages(t *testing.T) {
	want := map[FailureCode]string{
		FailureProcessTimeout:     "processing timed out",
		FailureProcessFailed:      "processing failed",
		FailureProcessOutputLimit: "processing output limit exceeded",
		FailureLeaseExpired:       "job lease expired",
		FailureWorkerShutdown:     "worker shut down before completion",
		FailureMaintenancePaused:  "job publication paused by maintenance",
	}
	if len(safeFailureMessages) != len(want) {
		t.Fatalf("safe message count = %d, want %d", len(safeFailureMessages), len(want))
	}
	for code, message := range want {
		if safeFailureMessages[code] != message {
			t.Errorf("message for %q = %q", code, safeFailureMessages[code])
		}
	}
	if _, ok := workerFailure(FailureLeaseExpired); ok {
		t.Fatal("worker may persist lease_expired")
	}
	if _, ok := workerFailure(FailureCode("/private/path: stderr secret")); ok {
		t.Fatal("arbitrary failure code accepted")
	}
}

func TestValidateTypes(t *testing.T) {
	got, err := validateTypes([]Type{TypeTransform, TypeTransform})
	if err != nil || len(got) != 1 || got[0] != "transform" {
		t.Fatalf("validateTypes = %#v, %v", got, err)
	}
	for _, unsupported := range []Type{TypePurge, "unsupported"} {
		if _, err := validateTypes([]Type{unsupported}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsupported type %q error = %v", unsupported, err)
		}
	}
}

func TestJSONAuditArgumentsMustBeObject(t *testing.T) {
	for _, value := range []string{"", "null", "[]", `"value"`, "{bad"} {
		if jsonObject([]byte(value)) {
			t.Errorf("jsonObject(%q) = true", value)
		}
	}
	if !jsonObject([]byte(`{"job_id":"redacted"}`)) {
		t.Fatal("valid object rejected")
	}
}

func TestDatabaseErrorClassification(t *testing.T) {
	for _, code := range []string{"08006", "40001", "53000", "55P03", "57014", "57P01", "57P05"} {
		err := classifyDatabaseError(&pgconn.PgError{Code: code})
		if !errors.Is(err, ErrDatabaseUnavailable) {
			t.Errorf("SQLSTATE %s classified as %v", code, err)
		}
	}
	if err := classifyDatabaseError(&pgconn.PgError{Code: "23514"}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("constraint error classified as %v", err)
	}
	if err := classifyDatabaseError(errors.New("scan includes /secret/path")); !errors.Is(err, ErrInvariant) {
		t.Fatalf("scan error classified as %v", err)
	}
	if err := classifyDatabaseError(context.DeadlineExceeded); !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("deadline classified as %v", err)
	}
}

func TestRenditionValidation(t *testing.T) {
	ids := make([]string, 6)
	for index := range ids {
		ids[index] = mustTestUUID(t)
	}
	width, height := 10, 20
	candidate := Rendition{
		ID: ids[0], JobID: ids[1], LeaseToken: ids[2], TargetID: ids[3], OriginalID: ids[4], MediaID: ids[5],
		ProfileID: ids[1], RelativePath: "renditions/" + ids[4][:2] + "/" + ids[4] + "/" + ids[3] + "/" + ids[0] + ".avif",
		MIMEType: "image/avif", Width: &width, Height: &height, SizeBytes: 1, SHA256: strings.Repeat("a", 64),
		ProcessorAudit: json.RawMessage(`{"schema_version":1,"family":"still","result":{"tool":"test"}}`),
	}
	if !validRendition(candidate) {
		t.Fatal("valid rendition rejected")
	}
	tests := map[string]func(*Rendition){
		"bad path provenance":     func(value *Rendition) { value.OriginalID = ids[5] },
		"MIME extension mismatch": func(value *Rendition) { value.MIMEType = "video/mp4" },
		"unpaired dimensions":     func(value *Rendition) { value.Height = nil },
		"negative size":           func(value *Rendition) { value.SizeBytes = -1 },
		"invalid digest":          func(value *Rendition) { value.SHA256 = "A" + value.SHA256[1:] },
		"non-object audit":        func(value *Rendition) { value.ProcessorAudit = json.RawMessage(`[]`) },
		"empty audit":             func(value *Rendition) { value.ProcessorAudit = json.RawMessage(`{}`) },
		"missing audit schema": func(value *Rendition) {
			value.ProcessorAudit = json.RawMessage(`{"family":"still","result":{"tool":"test"}}`)
		},
		"unknown audit schema": func(value *Rendition) {
			value.ProcessorAudit = json.RawMessage(`{"schema_version":2,"family":"still","result":{"tool":"test"}}`)
		},
		"unknown audit family": func(value *Rendition) {
			value.ProcessorAudit = json.RawMessage(`{"schema_version":1,"family":"other","result":{"tool":"test"}}`)
		},
		"empty audit result": func(value *Rendition) {
			value.ProcessorAudit = json.RawMessage(`{"schema_version":1,"family":"still","result":{}}`)
		},
		"non-object audit result": func(value *Rendition) {
			value.ProcessorAudit = json.RawMessage(`{"schema_version":1,"family":"still","result":[]}`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			invalid := candidate
			mutate(&invalid)
			if validRendition(invalid) {
				t.Fatal("invalid rendition accepted")
			}
		})
	}
}

func TestPinnedProfileSelectsExactRecipeOutput(t *testing.T) {
	toPinned := func(definition profiledefinition.Definition) Profile {
		return Profile{
			ID: definition.ID, Key: definition.Key, Version: definition.Version,
			Processor: definition.Processor, ParametersSchemaVersion: definition.ParametersSchemaVersion,
			InputMIMETypes: definition.InputMIMETypes, Parameters: definition.Parameters,
		}
	}
	tests := []struct {
		name         string
		profile      Profile
		originalMIME string
		outputMIME   string
		suffix       string
	}{
		{name: "still AVIF", profile: toPinned(profiledefinition.StandardV1()), originalMIME: "image/jpeg", outputMIME: "image/avif", suffix: ".avif"},
		{name: "animation WebP", profile: toPinned(profiledefinition.StandardV1()), originalMIME: "image/gif", outputMIME: "image/webp", suffix: ".webp"},
		{name: "video MP4", profile: toPinned(profiledefinition.StandardV1()), originalMIME: "video/mp4", outputMIME: "video/mp4", suffix: ".mp4"},
		{name: "animation thumbnail AVIF", profile: toPinned(profiledefinition.ThumbnailV1()), originalMIME: "image/gif", outputMIME: "image/avif", suffix: ".avif"},
		{name: "video thumbnail AVIF", profile: toPinned(profiledefinition.ThumbnailV1()), originalMIME: "video/quicktime", outputMIME: "image/avif", suffix: ".avif"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePinnedOutput(test.profile, "active", test.originalMIME, test.outputMIME, "rendition"+test.suffix); err != nil {
				t.Fatalf("valid output rejected: %v", err)
			}
			if err := validatePinnedOutput(test.profile, "active", test.originalMIME, "video/mp4", "rendition.mp4"); test.outputMIME != "video/mp4" && !errors.Is(err, ErrConflict) {
				t.Fatalf("wrong output error = %v", err)
			}
		})
	}
	if err := validatePinnedOutput(toPinned(profiledefinition.StandardV1()), "draft", "image/jpeg", "image/avif", "rendition.avif"); !errors.Is(err, ErrInvariant) {
		t.Fatalf("draft status error = %v", err)
	}
}

func mustTestUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNewUUIDv4(t *testing.T) {
	for range 20 {
		id, err := newUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || id[14] != '4' || !containsByte("89ab", id[19]) {
			t.Fatalf("not UUIDv4: %q", id)
		}
	}
}

func containsByte(value string, target byte) bool {
	for index := range len(value) {
		if value[index] == target {
			return true
		}
	}
	return false
}
