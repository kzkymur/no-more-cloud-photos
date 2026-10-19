package purgeexecutor

import (
	"context"
	"errors"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

type Service interface {
	StartNextPurge(context.Context, int) (medialifecycle.PurgeLease, error)
	RunPurgeFileStep(context.Context, string, string, medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error)
	FinalizePurge(context.Context, string, string) error
	HeartbeatPurge(context.Context, string, string) (time.Time, error)
	FinishPurgeAttempt(context.Context, string, string, string) error
}

type Repository struct{ Service Service }

func (r Repository) Claim(ctx context.Context, types []job.Type) (job.Lease, error) {
	if r.Service == nil || len(types) != 1 || types[0] != job.TypePurge {
		return job.Lease{}, job.ErrInvalid
	}
	lease, err := r.Service.StartNextPurge(ctx, 50)
	if err != nil {
		return job.Lease{}, mapError(err)
	}
	return job.Lease{ID: lease.JobID, Type: job.TypePurge, MediaID: lease.MediaID, Token: lease.Token, Attempts: lease.Attempts, MaxAttempts: lease.MaxAttempts, LeaseExpiresAt: lease.LeaseExpiresAt, StartedAt: lease.StartedAt, AvailableAt: lease.AvailableAt, CreatedAt: lease.CreatedAt}, nil
}
func (r Repository) Heartbeat(ctx context.Context, id, token string) (time.Time, error) {
	v, e := r.Service.HeartbeatPurge(ctx, id, token)
	return v, mapError(e)
}
func (r Repository) FinishAttempt(ctx context.Context, id, token string, code job.FailureCode) error {
	return mapError(r.Service.FinishPurgeAttempt(ctx, id, token, string(code)))
}
func (r Repository) ReclaimExpired(context.Context) (int, error) { return 0, nil }

type Executor struct {
	Service Service
	Store   *storage.Store
}

func (e Executor) Execute(ctx context.Context, lease job.Lease, _ worker.ExecutionLimits) error {
	if e.Service == nil || e.Store == nil || lease.Type != job.TypePurge || lease.ID == "" || lease.MediaID == "" || lease.Token == "" {
		return job.ErrInvalid
	}
	action := medialifecycle.StoragePurgeFileAction(e.Store)
	for {
		step, err := e.Service.RunPurgeFileStep(ctx, lease.ID, lease.Token, action)
		if err != nil {
			return mapError(err)
		}
		if step.Done {
			return mapError(e.Service.FinalizePurge(ctx, lease.ID, lease.Token))
		}
	}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, medialifecycle.ErrNoPurgeWork) {
		return job.ErrNoWork
	}
	if errors.Is(err, medialifecycle.ErrPurgeLeaseLost) {
		return job.ErrLeaseLost
	}
	var unknown *medialifecycle.CommitOutcomeUnknown
	if errors.As(err, &unknown) || medialifecycle.IsKind(err, medialifecycle.KindDatabaseUnavailable) {
		return job.ErrDatabaseUnavailable
	}
	if medialifecycle.IsKind(err, medialifecycle.KindInvalidRequest) {
		return job.ErrInvalid
	}
	if medialifecycle.IsKind(err, medialifecycle.KindInvariant) {
		return job.ErrInvariant
	}
	return err
}
