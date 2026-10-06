package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
)

type executorFunc func(context.Context, job.Lease, ExecutionLimits) error

func (f executorFunc) Execute(ctx context.Context, lease job.Lease, limits ExecutionLimits) error {
	return f(ctx, lease, limits)
}

type fakeRepository struct {
	mu             sync.Mutex
	heartbeatErr   error
	heartbeatFn    func(context.Context) error
	finishErr      error
	finishErrors   []error
	finishCodes    []job.FailureCode
	finishContexts []error
	reclaimCalls   int
	claimCalls     int
}

func (r *fakeRepository) Claim(context.Context, []job.Type) (job.Lease, error) {
	r.mu.Lock()
	r.claimCalls++
	r.mu.Unlock()
	return job.Lease{}, job.ErrNoWork
}

func (r *fakeRepository) Heartbeat(ctx context.Context, _ string, _ string) (time.Time, error) {
	if r.heartbeatFn != nil {
		return time.Time{}, r.heartbeatFn(ctx)
	}
	return time.Time{}, r.heartbeatErr
}

func (r *fakeRepository) FinishAttempt(ctx context.Context, _ string, _ string, code job.FailureCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishCodes = append(r.finishCodes, code)
	r.finishContexts = append(r.finishContexts, ctx.Err())
	if len(r.finishErrors) > 0 {
		err := r.finishErrors[0]
		r.finishErrors = r.finishErrors[1:]
		return err
	}
	return r.finishErr
}

func (r *fakeRepository) ReclaimExpired(context.Context) (int, error) {
	r.mu.Lock()
	r.reclaimCalls++
	r.mu.Unlock()
	return 0, nil
}

func TestNewDefaultsLimitsAndRejectsPurgeRegistration(t *testing.T) {
	repository := &fakeRepository{}
	worker, err := New(repository, nil, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	limits := worker.options.ExecutionLimits
	if worker.options.PollInterval != time.Second || worker.options.HeartbeatInterval != 30*time.Second ||
		worker.options.DatabaseTimeout != 10*time.Second || worker.options.ShutdownReleaseTimeout != 10*time.Second ||
		limits.Threads != 1 || limits.OutputBytesPerStream != 1<<20 || limits.StillTimeoutCeiling != 30*time.Minute ||
		limits.AnimationTimeoutCeiling != 2*time.Hour || limits.VideoTimeoutCeiling != 2*time.Hour {
		t.Fatalf("default options = %+v", worker.options)
	}
	if ceiling, ok := limits.TimeoutCeiling(FamilyVideo); !ok || ceiling != 2*time.Hour {
		t.Fatalf("video ceiling = %s, %v", ceiling, ok)
	}
	if _, ok := limits.TimeoutCeiling("unknown"); ok {
		t.Fatal("unknown family accepted")
	}
	if _, err := New(repository, map[job.Type]Executor{job.TypePurge: executorFunc(func(context.Context, job.Lease, ExecutionLimits) error { return nil })}, Options{}, nil); !errors.Is(err, job.ErrInvalid) {
		t.Fatalf("purge registration error = %v", err)
	}
	if _, err := New(repository, map[job.Type]Executor{job.TypeTransform: nil}, Options{}, nil); !errors.Is(err, job.ErrInvalid) {
		t.Fatalf("nil executor error = %v", err)
	}
	if _, err := New(repository, nil, Options{HeartbeatInterval: MaximumHeartbeatInterval + time.Nanosecond}, nil); !errors.Is(err, job.ErrInvalid) {
		t.Fatalf("unsafe heartbeat interval error = %v", err)
	}
	for _, limits := range []ExecutionLimits{
		{Threads: DefaultThreads + 1},
		{OutputBytesPerStream: DefaultOutputBytesPerStream + 1},
		{StillTimeoutCeiling: DefaultStillTimeoutCeiling + time.Nanosecond},
		{AnimationTimeoutCeiling: DefaultAnimationTimeoutCeiling + time.Nanosecond},
		{VideoTimeoutCeiling: DefaultVideoTimeoutCeiling + time.Nanosecond},
	} {
		if _, err := New(repository, nil, Options{ExecutionLimits: limits}, nil); !errors.Is(err, job.ErrInvalid) {
			t.Errorf("unsafe execution limits %+v error = %v", limits, err)
		}
	}
}

func TestRunLeaseMapsProcessFailuresToDurableCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code job.FailureCode
	}{
		{name: "timeout", err: processrunner.ErrTimeout, code: job.FailureProcessTimeout},
		{name: "deadline", err: context.DeadlineExceeded, code: job.FailureProcessTimeout},
		{name: "output", err: processrunner.ErrOutputLimit, code: job.FailureProcessOutputLimit},
		{name: "failure", err: errors.New("stderr /private/secret"), code: job.FailureProcessFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{}
			worker := testWorker(t, repository, executorFunc(func(context.Context, job.Lease, ExecutionLimits) error { return test.err }))
			if err := worker.runLease(context.Background(), testLease()); err != nil {
				t.Fatal(err)
			}
			if len(repository.finishCodes) != 1 || repository.finishCodes[0] != test.code {
				t.Fatalf("finish codes = %v", repository.finishCodes)
			}
		})
	}
}

func TestRunLeaseHeartbeatLossCancelsExecutionWithoutMutation(t *testing.T) {
	repository := &fakeRepository{heartbeatErr: job.ErrLeaseLost}
	cancelled := make(chan struct{})
	executor := executorFunc(func(ctx context.Context, _ job.Lease, limits ExecutionLimits) error {
		if limits.Threads != 1 {
			t.Errorf("threads = %d", limits.Threads)
		}
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	worker := testWorker(t, repository, executor)
	if err := worker.runLease(context.Background(), testLease()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("executor was not cancelled after lease loss")
	}
	if len(repository.finishCodes) != 0 {
		t.Fatalf("stale owner persisted finish codes %v", repository.finishCodes)
	}
}

func TestRunLeaseExecutorDatabaseAndLeaseErrorsDoNotPersistProcessFailure(t *testing.T) {
	for _, executorErr := range []error{job.ErrLeaseLost, job.ErrDatabaseUnavailable} {
		repository := &fakeRepository{}
		worker := testWorker(t, repository, executorFunc(func(context.Context, job.Lease, ExecutionLimits) error { return executorErr }))
		err := worker.runLease(context.Background(), testLease())
		if errors.Is(executorErr, job.ErrLeaseLost) && err != nil {
			t.Fatalf("lease loss result = %v", err)
		}
		if errors.Is(executorErr, job.ErrDatabaseUnavailable) && !errors.Is(err, job.ErrDatabaseUnavailable) {
			t.Fatalf("database result = %v", err)
		}
		if len(repository.finishCodes) != 0 {
			t.Fatalf("infrastructure error %v persisted as %v", executorErr, repository.finishCodes)
		}
	}
}

func TestRunLeaseShutdownCancelsAndRequeuesWithIndependentBudget(t *testing.T) {
	repository := &fakeRepository{finishErrors: []error{job.ErrDatabaseUnavailable, nil}}
	started := make(chan struct{})
	executor := executorFunc(func(ctx context.Context, _ job.Lease, _ ExecutionLimits) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	worker := testWorker(t, repository, executor)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.runLease(ctx, testLease()) }()
	<-started
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if len(repository.finishCodes) != 2 || repository.finishCodes[0] != job.FailureWorkerShutdown || repository.finishCodes[1] != job.FailureWorkerShutdown {
		t.Fatalf("shutdown finish codes = %v", repository.finishCodes)
	}
	if repository.finishContexts[0] != nil {
		t.Fatalf("shutdown release inherited cancelled context: %v", repository.finishContexts[0])
	}
}

func TestAlreadyCancelledLeaseIsReleasedWithoutStartingExecutor(t *testing.T) {
	repository := &fakeRepository{}
	var calls int
	worker := testWorker(t, repository, executorFunc(func(context.Context, job.Lease, ExecutionLimits) error {
		calls++
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.runLease(ctx, testLease()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("executor calls after cancellation = %d", calls)
	}
	if len(repository.finishCodes) != 1 || repository.finishCodes[0] != job.FailureWorkerShutdown || repository.finishContexts[0] != nil {
		t.Fatalf("cancelled claim release = codes:%v contexts:%v", repository.finishCodes, repository.finishContexts)
	}
}

func TestShutdownDuringHeartbeatStillUsesIndependentRelease(t *testing.T) {
	heartbeatStarted := make(chan struct{})
	repository := &fakeRepository{heartbeatFn: func(ctx context.Context) error {
		close(heartbeatStarted)
		<-ctx.Done()
		return job.ErrDatabaseUnavailable
	}}
	executor := executorFunc(func(ctx context.Context, _ job.Lease, _ ExecutionLimits) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := testWorker(t, repository, executor)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.runLease(ctx, testLease()) }()
	<-heartbeatStarted
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if len(repository.finishCodes) != 1 || repository.finishCodes[0] != job.FailureWorkerShutdown || repository.finishContexts[0] != nil {
		t.Fatalf("heartbeat/shutdown release = codes:%v contexts:%v", repository.finishCodes, repository.finishContexts)
	}
}

func TestRunLeaseSuccessRequiresExecutorOwnedCommitAndDoesNotFinishAgain(t *testing.T) {
	repository := &fakeRepository{}
	worker := testWorker(t, repository, executorFunc(func(context.Context, job.Lease, ExecutionLimits) error { return nil }))
	if err := worker.runLease(context.Background(), testLease()); err != nil {
		t.Fatal(err)
	}
	if len(repository.finishCodes) != 0 {
		t.Fatalf("successful executor was overwritten: %v", repository.finishCodes)
	}
}

func TestRunWithoutRegisteredExecutorNeverClaimsOrSucceedsWork(t *testing.T) {
	repository := &fakeRepository{}
	worker, err := New(repository, nil, Options{PollInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.reclaimCalls == 0 || repository.claimCalls != 0 || len(repository.finishCodes) != 0 {
		t.Fatalf("empty registry calls: reclaim=%d claim=%d finish=%v", repository.reclaimCalls, repository.claimCalls, repository.finishCodes)
	}
}

func testWorker(t *testing.T, repository *fakeRepository, executor Executor) *Worker {
	t.Helper()
	worker, err := New(repository, map[job.Type]Executor{job.TypeTransform: executor}, Options{
		HeartbeatInterval: time.Millisecond,
		DatabaseTimeout:   time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func testLease() job.Lease {
	return job.Lease{ID: "018f86a0-9d9b-4d1b-8d0b-40ec1eb8f115", Type: job.TypeTransform, Token: "018f86a0-9d9b-4d1b-8d0b-40ec1eb8f116"}
}
