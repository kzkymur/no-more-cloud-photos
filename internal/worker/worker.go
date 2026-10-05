package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
)

const (
	DefaultPollInterval            = time.Second
	DefaultHeartbeatInterval       = 30 * time.Second
	MaximumHeartbeatInterval       = 30 * time.Second
	DefaultDatabaseTimeout         = 10 * time.Second
	DefaultShutdownReleaseTimeout  = 10 * time.Second
	DefaultThreads                 = 1
	DefaultOutputBytesPerStream    = int64(1 << 20)
	DefaultStillTimeoutCeiling     = 30 * time.Minute
	DefaultAnimationTimeoutCeiling = 2 * time.Hour
	DefaultVideoTimeoutCeiling     = 24 * time.Hour
)

type Family string

const (
	FamilyStill     Family = "still"
	FamilyAnimation Family = "animation"
	FamilyVideo     Family = "video"
)

type ExecutionLimits struct {
	Threads                 int
	OutputBytesPerStream    int64
	StillTimeoutCeiling     time.Duration
	AnimationTimeoutCeiling time.Duration
	VideoTimeoutCeiling     time.Duration
}

func (l ExecutionLimits) TimeoutCeiling(family Family) (time.Duration, bool) {
	switch family {
	case FamilyStill:
		return l.StillTimeoutCeiling, true
	case FamilyAnimation:
		return l.AnimationTimeoutCeiling, true
	case FamilyVideo:
		return l.VideoTimeoutCeiling, true
	default:
		return 0, false
	}
}

type Options struct {
	PollInterval           time.Duration
	HeartbeatInterval      time.Duration
	DatabaseTimeout        time.Duration
	ShutdownReleaseTimeout time.Duration
	ExecutionLimits        ExecutionLimits
}

type Repository interface {
	Claim(context.Context, []job.Type) (job.Lease, error)
	Heartbeat(context.Context, string, string) (time.Time, error)
	FinishAttempt(context.Context, string, string, job.FailureCode) error
	ReclaimExpired(context.Context) (int, error)
}

// Executor owns the type-specific target lifecycle. It must synchronously stop
// and reap every child before returning after ctx cancellation. Registered
// executors are trusted in-process components built on processrunner, not
// plugins. A nil result means it has already committed the successful aggregate
// transition under the live lease; Worker never turns an unimplemented/no-op
// handler into success.
type Executor interface {
	Execute(context.Context, job.Lease, ExecutionLimits) error
}

type Worker struct {
	repository      Repository
	executors       map[job.Type]Executor
	registeredTypes []job.Type
	options         Options
	logger          *slog.Logger
}

func New(repository Repository, executors map[job.Type]Executor, options Options, logger *slog.Logger) (*Worker, error) {
	if repository == nil {
		return nil, fmt.Errorf("worker repository: %w", job.ErrInvalid)
	}
	options = withDefaults(options)
	if !validOptions(options) {
		return nil, fmt.Errorf("worker options: %w", job.ErrInvalid)
	}
	owned := make(map[job.Type]Executor, len(executors))
	registered := make([]job.Type, 0, len(executors))
	for jobType, executor := range executors {
		// Purge first-start acquisition is intentionally owned by Issue #16.
		if jobType != job.TypeTransform || executor == nil {
			return nil, fmt.Errorf("worker executor registration: %w", job.ErrInvalid)
		}
		owned[jobType] = executor
		registered = append(registered, jobType)
	}
	sort.Slice(registered, func(i, j int) bool { return registered[i] < registered[j] })
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Worker{repository: repository, executors: owned, registeredTypes: registered, options: options, logger: logger}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return job.ErrInvalid
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if err := w.reclaim(ctx); err != nil {
			if errors.Is(err, job.ErrDatabaseUnavailable) {
				w.logger.WarnContext(ctx, "job reclaim temporarily unavailable")
				resetTimer(timer, w.options.PollInterval)
				continue
			}
			return err
		}
		if len(w.registeredTypes) == 0 {
			resetTimer(timer, w.options.PollInterval)
			continue
		}
		claimCtx, cancel := context.WithTimeout(ctx, w.options.DatabaseTimeout)
		lease, err := w.repository.Claim(claimCtx, w.registeredTypes)
		cancel()
		switch {
		case err == nil:
			if err := w.runLease(ctx, lease); err != nil {
				if errors.Is(err, job.ErrDatabaseUnavailable) {
					w.logger.WarnContext(ctx, "job execution database operation temporarily unavailable", slog.String("job_id", lease.ID))
				} else {
					return err
				}
			}
		case errors.Is(err, job.ErrNoWork):
		case errors.Is(err, job.ErrDatabaseUnavailable):
			w.logger.WarnContext(ctx, "job claim temporarily unavailable")
		default:
			return err
		}
		resetTimer(timer, w.options.PollInterval)
	}
}

func (w *Worker) reclaim(ctx context.Context) error {
	reclaimCtx, cancel := context.WithTimeout(ctx, w.options.DatabaseTimeout)
	defer cancel()
	count, err := w.repository.ReclaimExpired(reclaimCtx)
	if err == nil && count > 0 {
		w.logger.InfoContext(ctx, "reclaimed expired job leases", slog.Int("count", count))
	}
	return err
}

func (w *Worker) runLease(parent context.Context, lease job.Lease) error {
	if parent.Err() != nil {
		return w.releaseOnShutdown(lease)
	}
	executor := w.executors[lease.Type]
	if executor == nil {
		// A claim result outside the exact registration set is an invariant
		// failure, never a successful no-op.
		return job.ErrInvariant
	}
	executionCtx, cancelExecution := context.WithCancelCause(parent)
	defer cancelExecution(context.Canceled)
	result := make(chan error, 1)
	go func() { result <- executor.Execute(executionCtx, lease, w.options.ExecutionLimits) }()
	heartbeat := time.NewTicker(w.options.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case executionErr := <-result:
			cancelExecution(executionErr)
			if executionErr == nil {
				return nil
			}
			if parent.Err() != nil {
				return w.releaseOnShutdown(lease)
			}
			if errors.Is(executionErr, job.ErrLeaseLost) {
				return nil
			}
			if errors.Is(executionErr, job.ErrDatabaseUnavailable) {
				return executionErr
			}
			return w.finish(parent, lease, processFailureCode(executionErr))
		case <-heartbeat.C:
			if parent.Err() != nil {
				cancelExecution(parent.Err())
				<-result
				return w.releaseOnShutdown(lease)
			}
			heartbeatCtx, cancel := context.WithTimeout(parent, w.options.DatabaseTimeout)
			_, err := w.repository.Heartbeat(heartbeatCtx, lease.ID, lease.Token)
			cancel()
			if err != nil {
				cancelExecution(err)
				<-result
				if parent.Err() != nil {
					return w.releaseOnShutdown(lease)
				}
				if errors.Is(err, job.ErrLeaseLost) {
					return nil
				}
				return err
			}
		case <-parent.Done():
			cancelExecution(parent.Err())
			<-result
			return w.releaseOnShutdown(lease)
		}
	}
}

func (w *Worker) finish(parent context.Context, lease job.Lease, code job.FailureCode) error {
	finishCtx, cancel := context.WithTimeout(parent, w.options.DatabaseTimeout)
	defer cancel()
	err := w.repository.FinishAttempt(finishCtx, lease.ID, lease.Token, code)
	if parent.Err() != nil && errors.Is(err, job.ErrDatabaseUnavailable) {
		return w.releaseOnShutdown(lease)
	}
	if errors.Is(err, job.ErrLeaseLost) {
		return nil
	}
	return err
}

func (w *Worker) releaseOnShutdown(lease job.Lease) error {
	releaseCtx, cancel := context.WithTimeout(context.Background(), w.options.ShutdownReleaseTimeout)
	defer cancel()
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	for {
		err := w.repository.FinishAttempt(releaseCtx, lease.ID, lease.Token, job.FailureWorkerShutdown)
		if err == nil || errors.Is(err, job.ErrLeaseLost) {
			return nil
		}
		if !errors.Is(err, job.ErrDatabaseUnavailable) {
			return err
		}
		select {
		case <-retry.C:
		case <-releaseCtx.Done():
			return err
		}
	}
}

func processFailureCode(err error) job.FailureCode {
	switch {
	case errors.Is(err, processrunner.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return job.FailureProcessTimeout
	case errors.Is(err, processrunner.ErrOutputLimit):
		return job.FailureProcessOutputLimit
	default:
		return job.FailureProcessFailed
	}
}

func withDefaults(options Options) Options {
	if options.PollInterval == 0 {
		options.PollInterval = DefaultPollInterval
	}
	if options.HeartbeatInterval == 0 {
		options.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if options.DatabaseTimeout == 0 {
		options.DatabaseTimeout = DefaultDatabaseTimeout
	}
	if options.ShutdownReleaseTimeout == 0 {
		options.ShutdownReleaseTimeout = DefaultShutdownReleaseTimeout
	}
	if options.ExecutionLimits.Threads == 0 {
		options.ExecutionLimits.Threads = DefaultThreads
	}
	if options.ExecutionLimits.OutputBytesPerStream == 0 {
		options.ExecutionLimits.OutputBytesPerStream = DefaultOutputBytesPerStream
	}
	if options.ExecutionLimits.StillTimeoutCeiling == 0 {
		options.ExecutionLimits.StillTimeoutCeiling = DefaultStillTimeoutCeiling
	}
	if options.ExecutionLimits.AnimationTimeoutCeiling == 0 {
		options.ExecutionLimits.AnimationTimeoutCeiling = DefaultAnimationTimeoutCeiling
	}
	if options.ExecutionLimits.VideoTimeoutCeiling == 0 {
		options.ExecutionLimits.VideoTimeoutCeiling = DefaultVideoTimeoutCeiling
	}
	return options
}

func validOptions(options Options) bool {
	return options.PollInterval > 0 && options.HeartbeatInterval > 0 && options.HeartbeatInterval <= MaximumHeartbeatInterval && options.DatabaseTimeout > 0 &&
		options.ShutdownReleaseTimeout > 0 && options.ExecutionLimits.Threads > 0 && options.ExecutionLimits.Threads <= DefaultThreads &&
		options.ExecutionLimits.OutputBytesPerStream > 0 && options.ExecutionLimits.OutputBytesPerStream <= DefaultOutputBytesPerStream &&
		options.ExecutionLimits.StillTimeoutCeiling > 0 && options.ExecutionLimits.StillTimeoutCeiling <= DefaultStillTimeoutCeiling &&
		options.ExecutionLimits.AnimationTimeoutCeiling > 0 && options.ExecutionLimits.AnimationTimeoutCeiling <= DefaultAnimationTimeoutCeiling &&
		options.ExecutionLimits.VideoTimeoutCeiling > 0 && options.ExecutionLimits.VideoTimeoutCeiling <= DefaultVideoTimeoutCeiling
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
