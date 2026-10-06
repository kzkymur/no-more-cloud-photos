//go:build linux

package transformexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	jobID       = "10000000-0000-4000-8000-000000000001"
	mediaID     = "20000000-0000-4000-8000-000000000001"
	originalID  = "30000000-0000-4000-8000-000000000001"
	targetID    = "40000000-0000-4000-8000-000000000001"
	targetTwo   = "40000000-0000-4000-8000-000000000002"
	attemptOne  = "50000000-0000-4000-8000-000000000001"
	attemptTwo  = "50000000-0000-4000-8000-000000000002"
	renderOne   = "70000000-0000-4000-8000-000000000001"
	renderTwo   = "70000000-0000-4000-8000-000000000002"
	renderThree = "70000000-0000-4000-8000-000000000003"
)

type fakeRepository struct {
	events      *[]string
	candidates  []job.Rendition
	publishErr  error
	publishHook func(job.Rendition) error
	beginHook   func() error
	marks       []job.FailureCode
}

func (repository *fakeRepository) BeginTarget(context.Context, string, string, string) error {
	*repository.events = append(*repository.events, "begin-target")
	if repository.beginHook != nil {
		return repository.beginHook()
	}
	return nil
}

func (repository *fakeRepository) MarkTargetFailed(_ context.Context, _, _, _ string, code job.FailureCode) error {
	repository.marks = append(repository.marks, code)
	return nil
}

func (repository *fakeRepository) PublishRendition(_ context.Context, candidate job.Rendition) (job.Publication, error) {
	*repository.events = append(*repository.events, "db-publish")
	repository.candidates = append(repository.candidates, candidate)
	if repository.publishHook != nil {
		return job.Publication{}, repository.publishHook(candidate)
	}
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

func (store *fakeStorage) OpenOriginal(_ context.Context, _ storage.OriginalKey, validation storage.Validation) (Original, error) {
	file, err := os.CreateTemp(store.t.TempDir(), "original")
	if err != nil {
		store.t.Fatal(err)
	}
	if _, err := file.Write([]byte("original")); err != nil {
		store.t.Fatal(err)
	}
	contents := []byte("original")
	digest := sha256.Sum256(contents)
	if validation.ExpectedSize != int64(len(contents)) || validation.ExpectedSHA256 == nil || *validation.ExpectedSHA256 != digest {
		_ = file.Close()
		return nil, storage.ErrValidation
	}
	return &fakeOriginal{file: file}, nil
}

type fakeOriginal struct{ file *os.File }

func (original *fakeOriginal) UseReadOnlyFile(use func(*os.File) error) error {
	file, err := os.Open("/proc/self/fd/" + strconv.Itoa(int(original.file.Fd())))
	if err != nil {
		return err
	}
	defer file.Close()
	return use(file)
}

func (original *fakeOriginal) Close() error { return original.file.Close() }

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
	videoHook      func(videoprocessor.Request) error
	inspectHook    func(animationprocessor.InspectRequest) error
	animationHook  func(animationprocessor.Request) error
	stillInput     [][]byte
	inspectInput   [][]byte
	animationInput [][]byte
	videoRequests  []videoprocessor.Request
}

func (processors *fakeProcessors) Inspect(_ context.Context, request animationprocessor.InspectRequest) (animationprocessor.Inspection, error) {
	processors.calls = append(processors.calls, "inspect")
	contents, err := io.ReadAll(request.Input)
	if err != nil {
		return animationprocessor.Inspection{}, err
	}
	processors.inspectInput = append(processors.inspectInput, contents)
	if processors.inspectHook != nil {
		if err := processors.inspectHook(request); err != nil {
			return animationprocessor.Inspection{}, err
		}
	}
	return animationprocessor.Inspection{Classification: processors.classification}, nil
}

func (processors *fakeProcessors) Transform(ctx context.Context, request stillprocessor.Request) (stillprocessor.Result, error) {
	processors.calls = append(processors.calls, "still")
	contents, err := io.ReadAll(request.Input)
	if err != nil {
		return stillprocessor.Result{}, err
	}
	processors.stillInput = append(processors.stillInput, contents)
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
	contents, err := io.ReadAll(request.Input)
	if err != nil {
		return animationprocessor.Result{}, err
	}
	processors.animationInput = append(processors.animationInput, contents)
	if processors.animationHook != nil {
		if err := processors.animationHook(request); err != nil {
			return animationprocessor.Result{}, err
		}
	}
	_, _ = request.Output.Write([]byte("animation"))
	return animationprocessor.Result{OutputMIME: "image/webp", OutputExtension: "webp", Width: 10, Height: 8,
		Source: animationprocessor.Inspection{DurationMS: 250}, Audit: animationprocessor.Audit{Decoder: "fake"}}, nil
}

func (processors *fakeProcessors) TransformVideo(_ context.Context, request videoprocessor.Request) (videoprocessor.Result, error) {
	processors.calls = append(processors.calls, "video")
	processors.videoRequests = append(processors.videoRequests, request)
	if processors.videoHook != nil {
		if err := processors.videoHook(request); err != nil {
			return videoprocessor.Result{}, err
		}
	}
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
			var audit struct {
				SchemaVersion int             `json:"schema_version"`
				Family        worker.Family   `json:"family"`
				Result        json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(repository.candidates[0].ProcessorAudit, &audit); err != nil || audit.SchemaVersion != 1 || audit.Family == "" || len(audit.Result) == 0 || string(audit.Result) == "{}" {
				t.Fatalf("processor audit = %s, %v", repository.candidates[0].ProcessorAudit, err)
			}
			if test.mime == "video/mp4" && (repository.candidates[0].DurationMS == nil || *repository.candidates[0].DurationMS != 250) {
				t.Fatalf("video duration = %v, want verified output 250ms", repository.candidates[0].DurationMS)
			}
			if test.mime == "video/mp4" && (len(processors.videoRequests) != 1 || processors.videoRequests[0].ExpectedVideoStreamIndex == nil || *processors.videoRequests[0].ExpectedVideoStreamIndex != 2) {
				t.Fatalf("expected video stream propagation = %+v", processors.videoRequests)
			}
			if !slices.Equal(*store.events, []string{"begin-target", "storage-publish", "db-publish"}) {
				t.Fatalf("events = %v", *store.events)
			}
		})
	}
}

func TestExecutorWebPInspectionAndTransformUseIndependentOffsets(t *testing.T) {
	executor, _, _, processors := fixture(t, animationprocessor.ClassificationAnimation, nil, nil, []string{renderOne})
	if err := executor.Execute(context.Background(), leaseFor(t, "image/webp", attemptOne, 1, 0), executionLimits()); err != nil {
		t.Fatal(err)
	}
	if len(processors.inspectInput) != 1 || string(processors.inspectInput[0]) != "original" ||
		len(processors.animationInput) != 1 || string(processors.animationInput[0]) != "original" {
		t.Fatalf("processor inputs: inspect=%q transform=%q", processors.inspectInput, processors.animationInput)
	}
}

func TestExecutorRejectsMissingOrUnexpectedPrimaryVideoStream(t *testing.T) {
	stream := 2
	tests := []struct {
		name   string
		mime   string
		stream *int
	}{
		{name: "MP4 missing", mime: "video/mp4"},
		{name: "QuickTime missing", mime: "video/quicktime"},
		{name: "still unexpectedly set", mime: "image/jpeg", stream: &stream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, repository, store, processors := fixture(t, "", nil, nil, []string{renderOne})
			lease := leaseFor(t, test.mime, attemptOne, 1, 0)
			lease.Original.PrimaryVideoStreamIndex = test.stream
			if err := executor.Execute(context.Background(), lease, executionLimits()); !errors.Is(err, job.ErrInvalid) {
				t.Fatalf("Execute() error = %v, want ErrInvalid", err)
			}
			if len(*store.events) != 0 || len(processors.calls) != 0 || len(repository.candidates) != 0 {
				t.Fatalf("invalid lease performed work: events=%v calls=%v candidates=%d", *store.events, processors.calls, len(repository.candidates))
			}
		})
	}
}

func TestExecutorRejectsOverlyLargeOriginalBeforeWork(t *testing.T) {
	executor, repository, store, processors := fixture(t, "", nil, nil, []string{renderOne})
	lease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
	lease.Original.SizeBytes = maxOriginalSizeBytes + 1
	if err := executor.Execute(context.Background(), lease, executionLimits()); !errors.Is(err, job.ErrInvalid) {
		t.Fatalf("Execute() error = %v, want ErrInvalid", err)
	}
	if len(*store.events) != 0 || len(store.temps) != 0 || len(processors.calls) != 0 || len(repository.candidates) != 0 {
		t.Fatalf("oversized lease performed work: events=%v temps=%d processors=%v DB=%d", *store.events, len(store.temps), processors.calls, len(repository.candidates))
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

func TestExecutorPropagatesPartialRetryGeneratedBytesAtLimitBoundary(t *testing.T) {
	for _, generated := range []int64{videoprocessor.MaxGeneratedOutputBytes, videoprocessor.MaxGeneratedOutputBytes + 1} {
		t.Run(fmt.Sprintf("%d", generated), func(t *testing.T) {
			executor, repository, store, processors := fixture(t, "", nil, nil, []string{renderOne})
			wantErr := videoprocessor.ErrResourcePolicy
			if generated > videoprocessor.MaxGeneratedOutputBytes {
				wantErr = videoprocessor.ErrInvalid
			}
			processors.videoHook = func(videoprocessor.Request) error { return wantErr }
			lease := leaseFor(t, "video/mp4", attemptTwo, 2, 1)
			lease.GeneratedBytes = generated
			err := executor.Execute(context.Background(), lease, executionLimits())
			if !errors.Is(err, wantErr) || len(processors.videoRequests) != 1 || processors.videoRequests[0].GeneratedBytesBefore != generated {
				t.Fatalf("Execute() error=%v requests=%+v, want generated bytes %d", err, processors.videoRequests, generated)
			}
			if len(repository.candidates) != 0 || len(store.temps) != 1 || store.temps[0].aborts != 1 {
				t.Fatalf("rejected retry published=%d temps=%d aborts=%d", len(repository.candidates), len(store.temps), store.temps[0].aborts)
			}
		})
	}
}

func TestExecutorCountsHydratedAndAttemptBytesExactlyOnce(t *testing.T) {
	executor, _, _, processors := fixture(t, "", nil, nil, []string{renderOne, renderTwo})
	lease := leaseFor(t, "video/mp4", attemptTwo, 2, 1)
	lease.GeneratedBytes = 11
	second := lease.Targets[0]
	second.ID = targetTwo
	lease.Targets = append(lease.Targets, second)
	if err := executor.Execute(context.Background(), lease, executionLimits()); err != nil {
		t.Fatal(err)
	}
	if len(processors.videoRequests) != 2 || processors.videoRequests[0].GeneratedBytesBefore != 11 || processors.videoRequests[1].GeneratedBytesBefore != 16 {
		t.Fatalf("generated byte propagation = %+v", processors.videoRequests)
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

func TestExecutorRealStorePublishesDurablyBeforeDatabase(t *testing.T) {
	root := t.TempDir()
	var boundaries []storage.FaultEvent
	store := openExecutorStore(t, root, storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
		boundaries = append(boundaries, event)
		return nil
	}))
	lease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
	publishExecutorOriginal(t, store, lease)
	boundaries = nil

	events := []string{}
	repository := &fakeRepository{events: &events}
	repository.publishHook = func(candidate job.Rendition) error {
		want := []storage.FaultEvent{
			{Boundary: storage.BoundaryFileSync, Phase: storage.After, Key: candidate.RelativePath},
			{Boundary: storage.BoundaryRename, Phase: storage.After, Key: candidate.RelativePath},
			{Boundary: storage.BoundaryFinalDirectorySync, Phase: storage.After, Key: candidate.RelativePath},
		}
		position := 0
		for _, event := range boundaries {
			if position < len(want) && event == want[position] {
				position++
			}
		}
		if position != len(want) {
			t.Fatalf("completed publish boundaries before DB = %v, want ordered %v", boundaries, want)
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(candidate.RelativePath)))
		if err != nil {
			t.Fatalf("read durable rendition at DB boundary: %v", err)
		}
		digest := sha256.Sum256(contents)
		if string(contents) != "still" || candidate.SizeBytes != int64(len(contents)) || candidate.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("candidate does not describe durable bytes: candidate=%+v bytes=%q", candidate, contents)
		}
		return nil
	}
	executor := realStoreExecutor(t, repository, store, []string{renderOne})
	if err := executor.Execute(context.Background(), lease, executionLimits()); err != nil {
		t.Fatal(err)
	}
	if len(repository.candidates) != 1 {
		t.Fatalf("database publications = %d, want 1", len(repository.candidates))
	}
}

func TestExecutorRealStoreRejectsOriginalDescriptorMismatchBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*job.Lease)
	}{
		{name: "size", mutate: func(lease *job.Lease) { lease.Original.SizeBytes++ }},
		{name: "SHA-256", mutate: func(lease *job.Lease) { lease.Original.SHA256 = hex.EncodeToString(make([]byte, sha256.Size)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openExecutorStore(t, root, nil)
			lease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
			publishExecutorOriginal(t, store, lease)
			test.mutate(&lease)
			events := []string{}
			repository := &fakeRepository{events: &events}
			processors := &fakeProcessors{}
			executor := integrationExecutor(t, repository, store, processors, []string{renderOne})
			if err := executor.Execute(context.Background(), lease, executionLimits()); !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("Execute() error = %v, want ErrValidation", err)
			}
			if len(events) != 0 || len(processors.calls) != 0 || len(repository.candidates) != 0 {
				t.Fatalf("mismatch performed work: events=%v processors=%v DB=%d", events, processors.calls, len(repository.candidates))
			}
			if _, err := os.Stat(filepath.Join(root, "renditions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mismatch staged output: %v", err)
			}
		})
	}
}

func TestExecutorRealStorePathReplacementCannotSwapValidatedDescriptor(t *testing.T) {
	root := t.TempDir()
	store := openExecutorStore(t, root, nil)
	lease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
	publishExecutorOriginal(t, store, lease)
	originalPath := filepath.Join(root, filepath.FromSlash(lease.Original.RelativePath))
	events := []string{}
	repository := &fakeRepository{events: &events, beginHook: func() error {
		if err := os.Rename(originalPath, originalPath+".replaced"); err != nil {
			return err
		}
		return os.WriteFile(originalPath, []byte("attacker"), 0o600)
	}}
	processors := &fakeProcessors{}
	executor := integrationExecutor(t, repository, store, processors, []string{renderOne})
	if err := executor.Execute(context.Background(), lease, executionLimits()); err != nil {
		t.Fatal(err)
	}
	if len(processors.stillInput) != 1 || string(processors.stillInput[0]) != "original" {
		t.Fatalf("processor input = %q, want pinned original", processors.stillInput)
	}
}

func TestExecutorRealStorePublicationFaultsPreserveOrphanRules(t *testing.T) {
	fault := errors.New("injected publication fault")
	for _, test := range []struct {
		name             string
		boundary         storage.Boundary
		phase            storage.Phase
		published        bool
		uncertain        bool
		wantPublishError bool
	}{
		{name: "before rename", boundary: storage.BoundaryRename, phase: storage.Before},
		{name: "after rename", boundary: storage.BoundaryRename, phase: storage.After, published: true, uncertain: true, wantPublishError: true},
		{name: "after directory fsync", boundary: storage.BoundaryFinalDirectorySync, phase: storage.After, published: true, wantPublishError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			armed := false
			store := openExecutorStore(t, root, storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
				if armed && event.Boundary == test.boundary && event.Phase == test.phase {
					return fault
				}
				return nil
			}))
			lease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
			publishExecutorOriginal(t, store, lease)
			armed = true
			events := []string{}
			repository := &fakeRepository{events: &events}
			executor := realStoreExecutor(t, repository, store, []string{renderOne})
			err := executor.Execute(context.Background(), lease, executionLimits())
			if !errors.Is(err, fault) {
				t.Fatalf("Execute() error = %v, want injected fault", err)
			}
			var publishError *storage.PublishError
			if got := errors.As(err, &publishError); got != test.wantPublishError {
				t.Fatalf("PublishError present = %v, want %v: %v", got, test.wantPublishError, err)
			}
			if publishError != nil && (publishError.Published != test.published || publishError.Uncertain != test.uncertain) {
				t.Fatalf("PublishError = %+v, want published=%v uncertain=%v", publishError, test.published, test.uncertain)
			}
			if len(repository.candidates) != 0 {
				t.Fatalf("database publications = %d, want 0", len(repository.candidates))
			}

			finalPath, tempPath := renditionPaths(root, renderOne, attemptOne)
			_, finalErr := os.Stat(finalPath)
			if test.published && finalErr != nil {
				t.Fatalf("published final missing: %v", finalErr)
			}
			if test.published {
				contents, err := os.ReadFile(finalPath)
				if err != nil || string(contents) != "still" {
					t.Fatalf("published final bytes = %q, %v", contents, err)
				}
			}
			if !test.published && !errors.Is(finalErr, os.ErrNotExist) {
				t.Fatalf("pre-rename final exists: %v", finalErr)
			}
			if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary file survived executor cleanup: %v", err)
			}
		})
	}
}

func TestExecutorRealStoreRetryUsesFreshRenditionAndCollisionNeverOverwrites(t *testing.T) {
	t.Run("fresh retry preserves first orphan", func(t *testing.T) {
		root := t.TempDir()
		store := openExecutorStore(t, root, nil)
		firstLease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
		publishExecutorOriginal(t, store, firstLease)
		events := []string{}
		databaseFault := errors.New("database publication failed")
		repository := &fakeRepository{events: &events}
		repository.publishHook = func(job.Rendition) error {
			if len(repository.candidates) == 1 {
				return databaseFault
			}
			return nil
		}
		executor := realStoreExecutor(t, repository, store, []string{renderOne, renderTwo})
		if err := executor.Execute(context.Background(), firstLease, executionLimits()); !errors.Is(err, databaseFault) {
			t.Fatalf("first Execute() error = %v", err)
		}
		firstPath, _ := renditionPaths(root, renderOne, attemptOne)
		firstBytes, err := os.ReadFile(firstPath)
		if err != nil {
			t.Fatal(err)
		}
		secondLease := leaseFor(t, "image/jpeg", attemptTwo, 2, 1)
		if err := executor.Execute(context.Background(), secondLease, executionLimits()); err != nil {
			t.Fatal(err)
		}
		secondPath, _ := renditionPaths(root, renderTwo, attemptTwo)
		secondBytes, err := os.ReadFile(secondPath)
		if err != nil {
			t.Fatal(err)
		}
		unchanged, err := os.ReadFile(firstPath)
		if err != nil {
			t.Fatal(err)
		}
		if firstPath == secondPath || string(firstBytes) != "still" || string(secondBytes) != "still" || !slices.Equal(unchanged, firstBytes) {
			t.Fatalf("retry files: first=%q unchanged=%q second=%q", firstBytes, unchanged, secondBytes)
		}
		if len(repository.candidates) != 2 || repository.candidates[0].ID == repository.candidates[1].ID {
			t.Fatalf("publication candidates = %+v", repository.candidates)
		}
	})

	t.Run("forced UUID reuse collides without overwrite or DB call", func(t *testing.T) {
		root := t.TempDir()
		store := openExecutorStore(t, root, nil)
		firstLease := leaseFor(t, "image/jpeg", attemptOne, 1, 0)
		publishExecutorOriginal(t, store, firstLease)
		events := []string{}
		repository := &fakeRepository{events: &events}
		executor := realStoreExecutor(t, repository, store, []string{renderOne, renderOne})
		if err := executor.Execute(context.Background(), firstLease, executionLimits()); err != nil {
			t.Fatal(err)
		}
		finalPath, _ := renditionPaths(root, renderOne, attemptOne)
		before, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := executor.Execute(context.Background(), leaseFor(t, "image/jpeg", attemptTwo, 2, 1), executionLimits()); !errors.Is(err, storage.ErrCollision) {
			t.Fatalf("UUID reuse error = %v, want ErrCollision", err)
		}
		after, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(after, before) || len(repository.candidates) != 1 {
			t.Fatalf("collision changed final or called DB: before=%q after=%q calls=%d", before, after, len(repository.candidates))
		}
		_, secondTemp := renditionPaths(root, renderOne, attemptTwo)
		if _, err := os.Stat(secondTemp); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("collision temporary survived: %v", err)
		}
	})
}

func openExecutorStore(t *testing.T, root string, faults storage.FaultInjector) *storage.Store {
	t.Helper()
	store, err := storage.Open(root, storage.Options{Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})
	return store
}

func publishExecutorOriginal(t *testing.T, store *storage.Store, lease job.Lease) {
	t.Helper()
	key, err := storage.ParseOriginalKey(lease.Original.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	attempt, _ := storage.ParseAttemptID("50000000-0000-4000-8000-000000000099")
	temporary, err := store.BeginOriginal(context.Background(), key, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(context.Background(), storage.Validation{ExpectedSize: int64(len("original"))}); err != nil {
		t.Fatal(err)
	}
}

func realStoreExecutor(t *testing.T, repository Repository, store *storage.Store, ids []string) *Executor {
	t.Helper()
	processors := &fakeProcessors{}
	index := 0
	executor, err := New(repository, StoreAdapter{Store: store}, processors, animationAdapter{processors}, videoAdapter{processors}, Options{
		UUID: func() (string, error) {
			id := ids[index]
			index++
			return id, nil
		},
		AbortTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func renditionPaths(root, renditionID, attemptID string) (string, string) {
	key := filepath.Join(root, "renditions", originalID[:2], originalID, targetID, renditionID+".avif")
	temporary := filepath.Join(filepath.Dir(key), "."+filepath.Base(key)+"."+attemptID+".tmp")
	return key, temporary
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
	var primaryVideoStreamIndex *int
	if mime == "video/mp4" || mime == "video/quicktime" {
		value := 2
		primaryVideoStreamIndex = &value
	}
	digest := sha256.Sum256([]byte("original"))
	return job.Lease{ID: jobID, Type: job.TypeTransform, MediaID: mediaID, Token: attempt, Attempts: attempts, MaxAttempts: 3,
		Original: &job.Original{ID: originalID, MediaID: mediaID, RelativePath: key.String(), MIMEType: mime, SizeBytes: 8, SHA256: hex.EncodeToString(digest[:]), PrimaryVideoStreamIndex: primaryVideoStreamIndex},
		Targets: []job.Target{{ID: targetID, Status: job.TargetPending, Attempts: targetAttempts, Profile: job.Profile{
			ID: definition.ID, Key: definition.Key, Version: definition.Version, Processor: definition.Processor,
			ParametersSchemaVersion: definition.ParametersSchemaVersion, InputMIMETypes: definition.InputMIMETypes, Parameters: definition.Parameters,
		}}}}
}

func executionLimits() worker.ExecutionLimits {
	return worker.ExecutionLimits{Threads: 1, OutputBytesPerStream: 1 << 20, StillTimeoutCeiling: time.Minute, AnimationTimeoutCeiling: time.Minute, VideoTimeoutCeiling: time.Minute}
}
