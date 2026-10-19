package purgeexecutor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

type fakeService struct {
	lease     medialifecycle.PurgeLease
	steps     int
	finalized int
	err       error
	finalErr  error
}

func (f *fakeService) StartNextPurge(context.Context, int) (medialifecycle.PurgeLease, error) {
	return f.lease, f.err
}
func (f *fakeService) RunPurgeFileStep(context.Context, string, string, medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error) {
	f.steps++
	return medialifecycle.PurgeStepResult{Done: f.steps > 2}, f.err
}
func (f *fakeService) FinalizePurge(context.Context, string, string) error {
	f.finalized++
	if f.finalErr != nil {
		return f.finalErr
	}
	return f.err
}
func (f *fakeService) HeartbeatPurge(context.Context, string, string) (time.Time, error) {
	return time.Now(), f.err
}
func (f *fakeService) FinishPurgeAttempt(context.Context, string, string, string) error { return f.err }

func TestExecutorDrainsThenFinalizesOnce(t *testing.T) {
	f := &fakeService{}
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lease := job.Lease{ID: "job", MediaID: "media", Token: "token", Type: job.TypePurge}
	if err := (Executor{Service: f, Store: store}).Execute(context.Background(), lease, worker.ExecutionLimits{}); err != nil {
		t.Fatal(err)
	}
	if f.steps != 3 || f.finalized != 1 {
		t.Fatalf("steps=%d finalized=%d", f.steps, f.finalized)
	}
}

func TestRepositoryClaimsOnlyPurge(t *testing.T) {
	f := &fakeService{lease: medialifecycle.PurgeLease{JobID: "job", MediaID: "media", Token: "token"}}
	lease, err := (Repository{Service: f}).Claim(context.Background(), []job.Type{job.TypePurge})
	if err != nil || lease.Type != job.TypePurge {
		t.Fatalf("lease=%#v err=%v", lease, err)
	}
	if _, err = (Repository{Service: f}).Claim(context.Background(), []job.Type{job.TypeTransform}); err != job.ErrInvalid {
		t.Fatalf("error=%v", err)
	}
}

func TestFinalizeCommitUncertaintyRemainsInfrastructureFailure(t *testing.T) {
	f := &fakeService{finalErr: &medialifecycle.CommitOutcomeUnknown{Cause: errors.New("commit response lost")}}
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lease := job.Lease{ID: "job", MediaID: "media", Token: "token", Type: job.TypePurge}
	err = (Executor{Service: f, Store: store}).Execute(context.Background(), lease, worker.ExecutionLimits{})
	if !errors.Is(err, job.ErrDatabaseUnavailable) || f.finalized != 1 {
		t.Fatalf("finalizer uncertainty error=%v finalized=%d", err, f.finalized)
	}
}
