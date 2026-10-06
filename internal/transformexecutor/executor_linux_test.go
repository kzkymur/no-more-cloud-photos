//go:build linux

package transformexecutor

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

const (
	jobID      = "10000000-0000-4000-8000-000000000001"
	mediaID    = "20000000-0000-4000-8000-000000000001"
	originalID = "30000000-0000-4000-8000-000000000001"
	targetID   = "40000000-0000-4000-8000-000000000001"
	attemptOne = "50000000-0000-4000-8000-000000000001"
	attemptTwo = "50000000-0000-4000-8000-000000000002"
	renderOne  = "70000000-0000-4000-8000-000000000001"
	renderTwo  = "70000000-0000-4000-8000-000000000002"
)

type fakeRepository struct {
	events     *[]string
	candidates []job.Rendition
	publishErr error
	marks      []job.FailureCode
}

func (repository *fakeRepository) BeginTarget(context.Context, string, string, string) error {
	*repository.events = append(*repository.events, "begin-target")
	return nil
}

func (repository *fakeRepository) MarkTargetFailed(_ context.Context, _, _, _ string, code job.FailureCode) error {
	repository.marks = append(repository.marks, code)
	return nil
}

func (repository *fakeRepository) PublishRendition(_ context.Context, candidate job.Rendition) (job.Publication, error) {
	*repository.events = append(*repository.events, "db-publish")
	repository.candidates = append(repository.candidates, candidate)
	return job.Publication{}, repository.publishErr
}

type fakeStorage struct {
	t          *testing.T
	events     *[]string
	temps      []*fakeTemporary
	publishErr error
	beginKeys  []string
	attemptIDs []string
}

func (store *fakeStorage) OpenOriginal(context.Context, storage.OriginalKey) (*os.File, error) {
	file, err := os.CreateTemp(store.t.TempDir(), "original")
	if err != nil {
		store.t.Fatal(err)
	}
	if _, err := file.Write([]byte("original")); err != nil {
		store.t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		store.t.Fatal(err)
	}
	return file, nil
}

func (store *fakeStorage) BeginRendition(_ context.Context, key storage.RenditionKey, attempt storage.AttemptID) (Temporary, error) {
	file, err := os.CreateTemp(store.t.TempDir(), "rendition")
	if err != nil {
		store.t.Fatal(err)
	}
	temporary := &fakeTemporary{file: file, events: store.events, publishErr: store.publishErr}
	store.temps = append(store.temps, temporary)
	store.beginKeys = append(store.beginKeys, key.String())
	store.attemptIDs = append(store.attemptIDs, attempt.String())
	return temporary, nil
}

type fakeTemporary struct {
	file       *os.File
	events     *[]string
	publishErr error
	aborts     int
}

func (temporary *fakeTemporary) UseWritableFile(use func(*os.File) error) error {
	return use(temporary.file)
}

func (temporary *fakeTemporary) Publish(context.Context, storage.Validation) (storage.ObjectInfo, error) {
	*temporary.events = append(*temporary.events, "storage-publish")
	if temporary.publishErr != nil {
		return storage.ObjectInfo{}, temporary.publishErr
	}
	contents, err := os.ReadFile(temporary.file.Name())
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{Size: int64(len(contents)), SHA256: sha256.Sum256(contents)}, nil
}

func (temporary *fakeTemporary) Abort(context.Context) error {
	temporary.aborts++
	return temporary.file.Close()
}

type fakeProcessors struct {
	calls          []string
	classification animationprocessor.Classification
	stillHook      func(context.Context) error
}

func (processors *fakeProcessors) Inspect(context.Context, animationprocessor.InspectRequest) (animationprocessor.Inspection, error) {
	processors.calls = append(processors.calls, "inspect")
	return animationprocessor.Inspection{Classification: processors.classification}, nil
}

func (processors *fakeProcessors) Transform(ctx context.Context, request stillprocessor.Request) (stillprocessor.Result, error) {
	processors.calls = append(processors.calls, "still")
	if processors.stillHook != nil {
		if err := processors.stillHook(ctx); err != nil {
			return stillprocessor.Result{}, err
		}
	}
	_, _ = request.Output.Write([]byte("still"))
	return stillprocessor.Result{OutputMIME: "image/avif", OutputExtension: "avif", Width: 10, Height: 8, Audit: stillprocessor.Audit{Decoder: "fake"}}, nil
}

func (processors *fakeProcessors) TransformAnimation(_ context.Context, request animationprocessor.Request) (animationprocessor.Result, error) {
	processors.calls = append(processors.calls, "animation")
	_, _ = request.Output.Write([]byte("animation"))
	return animationprocessor.Result{OutputMIME: "image/webp", OutputExtension: "webp", Width: 10, Height: 8,
		Source: animationprocessor.Inspection{DurationMS: 250}, Audit: animationprocessor.Audit{Decoder: "fake"}}, nil
}

func (processors *fakeProcessors) TransformVideo(_ context.Context, request videoprocessor.Request) (videoprocessor.Result, error) {
	processors.calls = append(processors.calls, "video")
	_, _ = request.Output.Write([]byte("video"))
	return videoprocessor.Result{OutputMIME: "video/mp4", OutputExtension: "mp4", Width: 10, Height: 8,
		OutputDurationUS: 250000, Source: videoprocessor.Inspection{DurationUS: 300000}, Audit: videoprocessor.Audit{ToolVersion: "fake"}}, nil
}

type animationAdapter struct{ processors *fakeProcessors }

func (adapter animationAdapter) Inspect(ctx context.Context, request animationprocessor.InspectRequest) (animationprocessor.Inspection, error) {
	return adapter.processors.Inspect(ctx, request)
}

func (adapter animationAdapter) Transform(ctx context.Context, request animationprocessor.Request) (animationprocessor.Result, error) {
	return adapter.processors.TransformAnimation(ctx, request)
}

type videoAdapter struct{ processors *fakeProcessors }

func (adapter videoAdapter) Transform(ctx context.Context, request videoprocessor.Request) (videoprocessor.Result, error) {
	return adapter.processors.TransformVideo(ctx, request)
}

func TestExecutorDispatchesByMIMEAndWebPInspection(t *testing.T) {
	tests := []struct {
		name, mime     string
		classification animationprocessor.Classification
		wantCalls      []string
		wantMIME       string
	}{
		{"jpeg", "image/jpeg", "", []string{"still"}, "image/avif"},
		{"gif", "image/gif", "", []string{"animation"}, "image/webp"},
		{"static webp", "image/webp", animationprocessor.ClassificationStatic, []string{"inspect", "still"}, "image/avif"},
		{"animated webp", "image/webp", animationprocessor.ClassificationAnimation, []string{"inspect", "animation"}, "image/webp"},
		{"video", "video/mp4", "", []string{"video"}, "video/mp4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, repository, store, processors := fixture(t, test.classification, nil, nil, []string{renderOne})
			if err := executor.Execute(context.Background(), leaseFor(t, test.mime, attemptOne, 1, 0), executionLimits()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(processors.calls, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", processors.calls, test.wantCalls)
			}
			if got := repository.candidates[0].MIMEType; got != test.wantMIME {
				t.Fatalf("MIME = %q, want %q", got, test.wantMIME)
			}
			if test.mime == "video/mp4" && (repository.candidates[0].DurationMS == nil || *repository.candidates[0].DurationMS != 250) {
				t.Fatalf("video duration = %v, want verified output 250ms", repository.candidates[0].DurationMS)
			}
			if !slices.Equal(*store.events, []string{"begin-target", "storage-publish", "db-publish"}) {
				t.Fatalf("events = %v", *store.events)
			}
		})
	}
}

func TestExecutorUsesFreshRenditionPerAttempt(t *testing.T) {
	executor, repository, store, _ := fixture(t, "", nil, nil, []string{renderOne, renderTwo})
	if err := executor.Execute(context.Background(), leaseFor(t, "image/jpeg", attemptOne, 1, 0), executionLimits()); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), leaseFor(t, "image/jpeg", attemptTwo, 2, 1), executionLimits()); err != nil {
		t.Fatal(err)
	}
	if repository.candidates[0].ID == repository.candidates[1].ID || store.beginKeys[0] == store.beginKeys[1] {
		t.Fatal("attempts reused rendition identity")
	}
	if !slices.Equal(store.attemptIDs, []string{attemptOne, attemptTwo}) {
		t.Fatalf("attempt IDs = %v", store.attemptIDs)
	}
}

func TestExecutorPreservesCancellationCause(t *testing.T) {
	cause := errors.New("lease heartbeat failed")
	ctx, cancel := context.WithCancelCause(context.Background())
	executor, repository, store, processors := fixture(t, "", nil, nil, []string{renderOne})
	processors.stillHook = func(context.Context) error {
		cancel(cause)
		return context.Canceled
	}
	err := executor.Execute(ctx, leaseFor(t, "image/jpeg", attemptOne, 1, 0), executionLimits())
	if !errors.Is(err, cause) || len(repository.marks) != 0 || store.temps[0].aborts != 1 {
		t.Fatalf("err=%v marks=%v aborts=%d", err, repository.marks, store.temps[0].aborts)
	}
}

func TestExecutorTranslatesProcessorLimits(t *testing.T) {
	tests := []struct {
		name                   string
		processorErr, sentinel error
		code                   job.FailureCode
	}{
		{"timeout", stillprocessor.ErrTimeout, processrunner.ErrTimeout, job.FailureProcessTimeout},
		{"log output", stillprocessor.ErrLogOutputLimit, processrunner.ErrOutputLimit, job.FailureProcessOutputLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, repository, _, processors := fixture(t, "", nil, nil, []string{renderOne})
			processors.stillHook = func(context.Context) error { return test.processorErr }
			err := executor.Execute(context.Background(), leaseFor(t, "image/jpeg", attemptOne, 1, 0), executionLimits())
			if !errors.Is(err, test.sentinel) || !slices.Equal(repository.marks, []job.FailureCode{test.code}) {
				t.Fatalf("err=%v marks=%v", err, repository.marks)
			}
		})
	}
}

func TestExecutorNeverAbortsAfterPublication(t *testing.T) {
	tests := []struct {
		name                    string
		storageErr, databaseErr error
	}{
		{"published storage error", &storage.PublishError{Published: true}, nil},
		{"database error", nil, job.ErrDatabaseUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, _, store, _ := fixture(t, "", test.storageErr, test.databaseErr, []string{renderOne})
			if err := executor.Execute(context.Background(), leaseFor(t, "image/jpeg", attemptOne, 1, 0), executionLimits()); err == nil {
				t.Fatal("expected error")
			}
			if store.temps[0].aborts != 0 {
				t.Fatalf("aborts = %d", store.temps[0].aborts)
			}
		})
	}
}

func fixture(t *testing.T, classification animationprocessor.Classification, storageErr, databaseErr error, ids []string) (*Executor, *fakeRepository, *fakeStorage, *fakeProcessors) {
	t.Helper()
	events := []string{}
	repository := &fakeRepository{events: &events, publishErr: databaseErr}
	store := &fakeStorage{t: t, events: &events, publishErr: storageErr}
	processors := &fakeProcessors{classification: classification}
	index := 0
	executor, err := New(repository, store, processors, animationAdapter{processors}, videoAdapter{processors}, Options{UUID: func() (string, error) {
		id := ids[index]
		index++
		return id, nil
	}, AbortTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return executor, repository, store, processors
}

func leaseFor(t *testing.T, mime, attempt string, attempts, targetAttempts int) job.Lease {
	t.Helper()
	definition := profile.StandardV1()
	extension, err := storage.OriginalExtensionForMIME(mime)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := storage.ParseOriginalID(originalID)
	key, _ := storage.NewOriginalKey(id, extension)
	return job.Lease{ID: jobID, Type: job.TypeTransform, MediaID: mediaID, Token: attempt, Attempts: attempts, MaxAttempts: 3,
		Original: &job.Original{ID: originalID, MediaID: mediaID, RelativePath: key.String(), MIMEType: mime, SizeBytes: 8, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Targets: []job.Target{{ID: targetID, Status: job.TargetPending, Attempts: targetAttempts, Profile: job.Profile{
			ID: definition.ID, Key: definition.Key, Version: definition.Version, Processor: definition.Processor,
			ParametersSchemaVersion: definition.ParametersSchemaVersion, InputMIMETypes: definition.InputMIMETypes, Parameters: definition.Parameters,
		}}}}
}

func executionLimits() worker.ExecutionLimits {
	return worker.ExecutionLimits{Threads: 1, OutputBytesPerStream: 1 << 20, StillTimeoutCeiling: time.Minute, AnimationTimeoutCeiling: time.Minute, VideoTimeoutCeiling: time.Minute}
}
