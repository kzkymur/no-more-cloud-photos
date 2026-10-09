//go:build linux

package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kzkymur/no-more-cloud-photos/internal/metadata"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestServiceAcceptStreamsAndPublishesAfterFinalize(t *testing.T) {
	events := []string{}
	temporary := &fakeStagedOriginal{events: &events}
	store := &fakeOriginalStore{temporary: temporary, events: &events}
	prober := &fakeMetadataProber{events: &events, result: validMetadata()}
	repository := &fakeAcceptanceRepository{timezone: "Asia/Tokyo", events: &events}
	repository.finalize = func(_ context.Context, input acceptance, publish func() (string, error)) (Outcome, error) {
		events = append(events, "finalize")
		if input.Size != 6 || input.SHA256 != sha256.Sum256([]byte("abcdef")) {
			t.Fatalf("finalize input size/digest = %d/%x", input.Size, input.SHA256)
		}
		if _, err := publish(); err != nil {
			t.Fatal(err)
		}
		return Outcome{Status: 201, Body: json.RawMessage(`{"media":{},"job":null}`)}, nil
	}
	service := testService(store, prober, repository)

	outcome, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("abcdef")))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if outcome.Status != 201 || temporary.aborted || !temporary.published {
		t.Fatalf("outcome/temp = %+v, aborted=%v published=%v", outcome, temporary.aborted, temporary.published)
	}
	wantEvents := []string{"begin", "write", "seal", "timezone", "read-only", "probe", "finalize", "publish"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestServiceAcceptSealFailurePreventsProbeAndDatabase(t *testing.T) {
	sealErr := errors.New("file sync failed")
	events := []string{}
	temporary := &fakeStagedOriginal{events: &events, sealErr: sealErr}
	prober := &fakeMetadataProber{events: &events, result: validMetadata()}
	repository := &fakeAcceptanceRepository{timezone: "UTC", events: &events}
	service := testService(&fakeOriginalStore{temporary: temporary, events: &events}, prober, repository)

	_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("body")))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "unavailable" || !errors.Is(err, sealErr) {
		t.Fatalf("Accept() error = %#v, want dependency failure", err)
	}
	if prober.calls != 0 || repository.timezoneCalls != 0 || repository.finalizeCalls != 0 {
		t.Fatalf("probe/timezone/finalize calls = %d/%d/%d", prober.calls, repository.timezoneCalls, repository.finalizeCalls)
	}
	if !temporary.aborted || temporary.published {
		t.Fatalf("aborted=%v published=%v", temporary.aborted, temporary.published)
	}
	wantEvents := []string{"begin", "write", "seal", "abort"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestServiceAcceptSealFailureClassification(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "validation failure", err: storage.ErrValidation, status: 503},
		{name: "storage failure", err: storage.ErrDurability, status: 503},
		{name: "canceled", err: context.Canceled, status: 408},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			temporary := &fakeStagedOriginal{sealErr: test.err}
			service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, &fakeAcceptanceRepository{timezone: "UTC"})
			_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("body")))
			var failure *Failure
			if !errors.As(err, &failure) || failure.Status != test.status {
				t.Fatalf("Accept() error = %#v, want status %d", err, test.status)
			}
			if !temporary.aborted {
				t.Fatal("temporary was not aborted")
			}
		})
	}
}

func TestServiceAcceptPublishUsesCachedSealValidation(t *testing.T) {
	temporary := &fakeStagedOriginal{}
	repository := &fakeAcceptanceRepository{timezone: "UTC", finalize: func(_ context.Context, _ acceptance, publish func() (string, error)) (Outcome, error) {
		_, err := publish()
		return Outcome{Status: 201}, err
	}}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, repository)

	if _, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("body"))); err != nil {
		t.Fatal(err)
	}
	if temporary.sealCalls != 1 || temporary.publishCalls != 1 || temporary.validationReads != 1 {
		t.Fatalf("seal/publish/validation reads = %d/%d/%d, want 1/1/1", temporary.sealCalls, temporary.publishCalls, temporary.validationReads)
	}
}

func TestServiceAcceptReplayAbortsWithoutPublish(t *testing.T) {
	temporary := &fakeStagedOriginal{}
	repository := &fakeAcceptanceRepository{timezone: "UTC"}
	repository.finalize = func(context.Context, acceptance, func() (string, error)) (Outcome, error) {
		return Outcome{Status: 201, Body: json.RawMessage(`{"future":1}`), Replayed: true}, nil
	}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, repository)

	outcome, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("body")))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Replayed || string(outcome.Body) != `{"future":1}` || !temporary.aborted || temporary.published {
		t.Fatalf("outcome/temp = %+v, aborted=%v published=%v", outcome, temporary.aborted, temporary.published)
	}
}

func TestServiceAcceptStreamingBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		body   io.Reader
		status int
		code   string
	}{
		{name: "empty", body: bytes.NewReader(nil), status: 400, code: "invalid_multipart"},
		{name: "too large", body: bytes.NewBufferString("12345"), status: 413, code: "upload_too_large"},
		{name: "read error", body: io.MultiReader(bytes.NewBufferString("12"), bodyErrorReader{}), status: 400, code: "invalid_multipart"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			temporary := &fakeStagedOriginal{}
			service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, &fakeAcceptanceRepository{timezone: "UTC"})
			service.maxBytes = 4
			_, err := service.Accept(context.Background(), validRequest(test.body))
			var failure *Failure
			if !errors.As(err, &failure) || failure.Status != test.status || failure.Code != test.code {
				t.Fatalf("Accept() error = %#v, want %d/%s", err, test.status, test.code)
			}
			if !temporary.aborted || temporary.published {
				t.Fatalf("aborted=%v published=%v", temporary.aborted, temporary.published)
			}
			if test.name == "too large" && len(temporary.data) != 5 {
				t.Fatalf("streamed %d bytes, want max+1", len(temporary.data))
			}
		})
	}
}

func TestServiceAcceptClassifiesTemporaryWriteFailureAsDependency(t *testing.T) {
	temporary := &fakeStagedOriginal{writeErr: errors.New("disk full")}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, &fakeAcceptanceRepository{timezone: "UTC"})
	_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("body")))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "unavailable" || !temporary.aborted {
		t.Fatalf("error=%#v aborted=%v", err, temporary.aborted)
	}
}

func TestServiceAcceptClassifiesProbeErrors(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{metadata.ErrUnsupportedMediaType, 415, "unsupported_media_type"},
		{metadata.ErrInvalidMedia, 422, "invalid_media"},
		{metadata.ErrPolicyViolation, 422, "invalid_media"},
		{metadata.ErrProbeTimeout, 422, "invalid_media"},
		{metadata.ErrProbeFailed, 422, "invalid_media"},
		{errors.New("tool unavailable"), 503, "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.err.Error(), func(t *testing.T) {
			temporary := &fakeStagedOriginal{}
			service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{err: test.err}, &fakeAcceptanceRepository{timezone: "UTC"})
			_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("x")))
			var failure *Failure
			if !errors.As(err, &failure) || failure.Status != test.status || failure.Code != test.code {
				t.Fatalf("Accept() error = %#v, want %d/%s", err, test.status, test.code)
			}
			if !temporary.aborted {
				t.Fatal("temporary was not aborted")
			}
		})
	}
}

func TestServiceAcceptReresolvesTimezoneWithoutReprobe(t *testing.T) {
	result := validMetadata()
	result.Derived.Capture.Candidates = []metadata.CaptureCandidate{{
		Name: "DateTimeOriginal", Kind: metadata.CaptureCandidateEXIF, DateTime: "2026:01:02 03:04:05",
	}}
	prober := &fakeMetadataProber{result: result}
	temporary := &fakeStagedOriginal{}
	repository := &fakeAcceptanceRepository{timezone: "Asia/Tokyo"}
	calls := 0
	repository.finalize = func(_ context.Context, input acceptance, publish func() (string, error)) (Outcome, error) {
		calls++
		if calls == 1 {
			return Outcome{}, &timezoneChangedError{Timezone: "UTC"}
		}
		if input.Timezone != "UTC" || input.Metadata.Derived.Capture.Timezone == nil || *input.Metadata.Derived.Capture.Timezone != "UTC" {
			t.Fatalf("retry capture = %+v in timezone %q", input.Metadata.Derived.Capture, input.Timezone)
		}
		_, err := publish()
		return Outcome{Status: 201, Body: json.RawMessage(`{"media":{},"job":null}`)}, err
	}
	service := testService(&fakeOriginalStore{temporary: temporary}, prober, repository)

	if _, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("x"))); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || prober.calls != 1 {
		t.Fatalf("finalize calls=%d probe calls=%d, want 2/1", calls, prober.calls)
	}
}

func TestServiceAcceptTimezoneRetryExhaustionAborts(t *testing.T) {
	temporary := &fakeStagedOriginal{}
	repository := &fakeAcceptanceRepository{timezone: "UTC", finalize: func(context.Context, acceptance, func() (string, error)) (Outcome, error) {
		return Outcome{}, &timezoneChangedError{Timezone: "Asia/Tokyo"}
	}}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, repository)
	service.timezoneTries = 2
	_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("x")))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 503 || !temporary.aborted {
		t.Fatalf("error=%#v aborted=%v", err, temporary.aborted)
	}
}

func TestServiceAcceptPreservesPublishedFileOnUnknownCommit(t *testing.T) {
	temporary := &fakeStagedOriginal{}
	repository := &fakeAcceptanceRepository{timezone: "UTC", finalize: func(_ context.Context, _ acceptance, publish func() (string, error)) (Outcome, error) {
		if _, err := publish(); err != nil {
			return Outcome{}, err
		}
		return Outcome{}, &OutcomeUnknown{Cause: errors.New("lost commit response")}
	}}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, repository)
	_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("x")))
	var unknown *OutcomeUnknown
	if !errors.As(err, &unknown) || !temporary.published || temporary.aborted {
		t.Fatalf("error=%#v published=%v aborted=%v", err, temporary.published, temporary.aborted)
	}
}

func TestCommitErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name       string
		commitErr  error
		rolledBack bool
	}{
		{name: "explicit rollback", commitErr: pgx.ErrTxCommitRollback, rolledBack: true},
		{name: "server error response", commitErr: &pgconn.PgError{Code: "23514", Message: "deferred constraint failed"}, rolledBack: true},
		{name: "connection loss", commitErr: errors.New("connection lost")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := commitUpload(context.Background(), commitErrorTx{commitErr: test.commitErr})
			var rolledBack *CommitRolledBack
			var unknown *OutcomeUnknown
			if errors.As(err, &rolledBack) != test.rolledBack || errors.As(err, &unknown) == test.rolledBack {
				t.Fatalf("commitUpload() error = %#v, rolled back=%v", err, test.rolledBack)
			}
		})
	}
}

func TestServiceAcceptDoesNotAbortPublishedPublishFailure(t *testing.T) {
	publishErr := &storage.PublishError{Published: true, Uncertain: true}
	temporary := &fakeStagedOriginal{publishErr: publishErr}
	repository := &fakeAcceptanceRepository{timezone: "UTC", finalize: func(_ context.Context, _ acceptance, publish func() (string, error)) (Outcome, error) {
		_, err := publish()
		return Outcome{}, err
	}}
	service := testService(&fakeOriginalStore{temporary: temporary}, &fakeMetadataProber{result: validMetadata()}, repository)

	_, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("x")))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 503 || !temporary.published || temporary.aborted {
		t.Fatalf("error=%#v published=%v aborted=%v", err, temporary.published, temporary.aborted)
	}
}

func TestDatabaseFailureClassification(t *testing.T) {
	service := &Service{}
	tests := []struct {
		name   string
		cause  error
		status int
	}{
		{name: "constraint invariant", cause: &pgconn.PgError{Code: "23514"}, status: 500},
		{name: "schema invariant", cause: &pgconn.PgError{Code: "42P01"}, status: 500},
		{name: "serialization", cause: &pgconn.PgError{Code: "40001"}, status: 503},
		{name: "connection", cause: &pgconn.PgError{Code: "08006"}, status: 503},
		{name: "network", cause: errors.New("connection lost"), status: 503},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var failure *Failure
			if err := service.databaseFailure(context.Background(), context.Background(), test.cause); !errors.As(err, &failure) || failure.Status != test.status {
				t.Fatalf("databaseFailure() = %#v, want status %d", err, test.status)
			}
		})
	}
}

func TestValidRequestIDUsesVisibleHTTPBytes(t *testing.T) {
	for _, valid := range []string{"!", "request-1", "~"} {
		if !validRequestID(valid) {
			t.Fatalf("validRequestID(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", " ", "request id", string([]byte{0x20}), string([]byte{0x7f})} {
		if validRequestID(invalid) {
			t.Fatalf("validRequestID(%q) = true", invalid)
		}
	}
}

func TestUploadAndJobResponseContract(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	originalID := testUUIDs[0]
	body := marshalResponse(uploadResponse{
		Media: mediaSummary{ID: testUUIDs[1], MIMEType: "image/jpeg", SizeBytes: 1, Width: intPointer(1), Height: intPointer(1), TakenAtSource: "unknown", CreatedAt: now},
		Job: &jobResponse{ID: testUUIDs[2], Type: "transform", Status: "queued", MediaID: testUUIDs[1], OriginalID: &originalID,
			MaxAttempts: 3, AvailableAt: now, CreatedAt: now, UpdatedAt: now, Targets: []targetResponse{{
				ID: testUUIDs[3], Profile: profileResponse{ID: testUUIDs[4], Key: "standard", Version: 1}, Status: "pending", UpdatedAt: now,
			}}},
	})
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	job := decoded["job"].(map[string]any)
	if job["max_attempts"] != float64(3) || job["attempts"] != float64(0) || job["error"] != nil {
		t.Fatalf("job response = %s", body)
	}
	target := job["targets"].([]any)[0].(map[string]any)
	if target["rendition_id"] != nil || target["error"] != nil || target["status"] != "pending" {
		t.Fatalf("target response = %s", body)
	}
	zeroProfile := marshalResponse(uploadResponse{Media: mediaSummary{ID: testUUIDs[1], MIMEType: "image/jpeg", TakenAtSource: "unknown", CreatedAt: now}})
	if !bytes.Contains(zeroProfile, []byte(`"job":null`)) {
		t.Fatalf("zero-profile response = %s", zeroProfile)
	}
}

type fakeOriginalStore struct {
	temporary stagedOriginal
	events    *[]string
}

func (s *fakeOriginalStore) Begin(context.Context, string, string) (stagedOriginal, error) {
	appendEvent(s.events, "begin")
	return s.temporary, nil
}

type fakeStagedOriginal struct {
	data            []byte
	events          *[]string
	aborted         bool
	published       bool
	sealed          bool
	writeErr        error
	sealErr         error
	publishErr      error
	sealCalls       int
	publishCalls    int
	validationReads int
}

func (f *fakeStagedOriginal) Seal(_ context.Context, validation storage.Validation) error {
	appendEvent(f.events, "seal")
	f.sealCalls++
	if f.sealErr != nil {
		return f.sealErr
	}
	f.validationReads++
	digest := sha256.Sum256(f.data)
	if validation.ExpectedSize != int64(len(f.data)) || validation.ExpectedSHA256 == nil || *validation.ExpectedSHA256 != digest {
		return storage.ErrValidation
	}
	f.sealed = true
	return nil
}

func (f *fakeStagedOriginal) Write(value []byte) (int, error) {
	appendEvent(f.events, "write")
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.data = append(f.data, value...)
	return len(value), nil
}

func (f *fakeStagedOriginal) UseReadOnlyFile(use func(*os.File) error) error {
	appendEvent(f.events, "read-only")
	file, err := os.CreateTemp(tTempRoot(), "upload-service-test-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(f.data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	readOnly, err := os.Open(name)
	if err != nil {
		return err
	}
	defer readOnly.Close()
	return use(readOnly)
}

func (f *fakeStagedOriginal) PublishSealed(_ context.Context, _ storage.OriginalExtension) (string, error) {
	appendEvent(f.events, "publish")
	f.publishCalls++
	if !f.sealed {
		return "", storage.ErrValidation
	}
	f.published = true
	if f.publishErr != nil {
		return "originals/00/id/original.jpg", f.publishErr
	}
	return "originals/00/id/original.jpg", nil
}

func (f *fakeStagedOriginal) Abort(context.Context) error {
	appendEvent(f.events, "abort")
	f.aborted = true
	return nil
}

type fakeMetadataProber struct {
	result metadata.Result
	err    error
	events *[]string
	calls  int
}

func (p *fakeMetadataProber) Probe(_ context.Context, file *os.File, _ string) (metadata.Result, error) {
	appendEvent(p.events, "probe")
	p.calls++
	if file == nil {
		return metadata.Result{}, errors.New("nil file")
	}
	return p.result, p.err
}

func TestUploadAttemptHeartbeatCancelsOperationOnLeaseFailure(t *testing.T) {
	want := errors.New("lease heartbeat failed")
	called := make(chan struct{}, 1)
	repository := &fakeAcceptanceRepository{heartbeat: func(context.Context, string) error {
		called <- struct{}{}
		return want
	}}
	service := newService(&fakeOriginalStore{}, &fakeMetadataProber{}, repository)
	service.heartbeatInterval = time.Millisecond
	ctx, cancel := context.WithCancelCause(context.Background())
	heartbeat := service.startAttemptHeartbeat(ctx, cancel, testUUIDs[1])
	defer heartbeat.Stop()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("heartbeat was not attempted")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("heartbeat failure did not cancel operation")
	}
	if !errors.Is(context.Cause(ctx), want) || !errors.Is(heartbeat.Err(), want) {
		t.Fatalf("heartbeat cause/context = %v/%v, want %v", heartbeat.Err(), context.Cause(ctx), want)
	}
}

type fakeAcceptanceRepository struct {
	timezone      string
	err           error
	events        *[]string
	timezoneCalls int
	finalizeCalls int
	finalize      func(context.Context, acceptance, func() (string, error)) (Outcome, error)
	heartbeat     func(context.Context, string) error
}

func (r *fakeAcceptanceRepository) RegisterAttempt(context.Context, string, string) error { return nil }
func (r *fakeAcceptanceRepository) HeartbeatAttempt(ctx context.Context, attemptID string) error {
	if r.heartbeat != nil {
		return r.heartbeat(ctx, attemptID)
	}
	return nil
}
func (r *fakeAcceptanceRepository) CompleteAttempt(context.Context, string, string) error { return nil }

func (r *fakeAcceptanceRepository) DefaultTimezone(context.Context) (string, error) {
	appendEvent(r.events, "timezone")
	r.timezoneCalls++
	return r.timezone, r.err
}

func (r *fakeAcceptanceRepository) Finalize(ctx context.Context, input acceptance, publish func() (string, error)) (Outcome, error) {
	r.finalizeCalls++
	if r.finalize == nil {
		return Outcome{}, errors.New("unexpected finalize")
	}
	return r.finalize(ctx, input, publish)
}

type bodyErrorReader struct{}

func (bodyErrorReader) Read([]byte) (int, error) { return 0, errors.New("body read failed") }

type commitErrorTx struct {
	pgx.Tx
	commitErr error
}

func (tx commitErrorTx) Commit(context.Context) error { return tx.commitErr }
func (commitErrorTx) Conn() *pgx.Conn                 { return nil }

func testService(store originalStore, prober metadataProber, repository acceptanceRepository) *Service {
	service := newService(store, prober, repository)
	index := 0
	service.newID = func() (string, error) {
		id := testUUIDs[index%len(testUUIDs)]
		index++
		return id, nil
	}
	return service
}

var testUUIDs = []string{
	"00000000-0000-4000-8000-000000000001",
	"00000000-0000-4000-8000-000000000002",
	"00000000-0000-4000-8000-000000000003",
	"00000000-0000-4000-8000-000000000004",
	"00000000-0000-4000-8000-000000000005",
}

func validRequest(body io.Reader) Request {
	filename := "photo.jpg"
	return Request{Body: body, Filename: &filename, IdempotencyKey: "upload-key", RequestID: "request-1"}
}

func validMetadata() metadata.Result {
	return metadata.Result{
		MIMEType: "image/jpeg", Extension: "jpg", Width: 10, Height: 20,
		Derived: metadata.DerivedMetadata{Capture: metadata.Capture{Source: metadata.TakenAtUnknown}},
	}
}

func appendEvent(events *[]string, value string) {
	if events != nil {
		*events = append(*events, value)
	}
}

func intPointer(value int) *int { return &value }

func tTempRoot() string {
	if value := os.Getenv("TMPDIR"); value != "" {
		return value
	}
	return "/tmp"
}
