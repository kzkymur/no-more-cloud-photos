//go:build linux

package upload

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

type notifyingFinalizeRepository struct {
	acceptanceRepository
	started chan struct{}
}

func TestServiceSurfacesRealStoreAbortDirectorySyncUncertaintyIntegration(t *testing.T) {
	fault := errors.New("injected abort directory sync failure")
	store, err := storage.Open(t.TempDir(), storage.Options{Faults: storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
		if event.Boundary == storage.BoundaryDeleteDirectorySync && event.Phase == storage.Before {
			return fault
		}
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	terminals := make([]string, 0, 1)
	repository := &fakeAcceptanceRepository{
		timezone: "UTC",
		finalize: func(context.Context, acceptance, func() (string, error)) (Outcome, error) {
			return Outcome{Status: 201, Replayed: true}, nil
		},
		complete: func(_ context.Context, _ string, terminal string) error {
			terminals = append(terminals, terminal)
			return nil
		},
	}
	service := testService(storageAdapter{store}, &fakeMetadataProber{result: validMetadata()}, repository)
	outcome, err := service.Accept(context.Background(), validRequest(bytes.NewBufferString("abort-uncertainty")))
	if outcome.Status != 201 || !outcome.Replayed || !errors.Is(err, fault) ||
		!errors.Is(err, storage.ErrOutcomeUncertain) || !errors.Is(err, storage.ErrDurability) {
		t.Fatalf("Accept() = %+v, %#v; want surfaced real-store abort uncertainty", outcome, err)
	}
	if len(terminals) != 1 || terminals[0] != "aborted" {
		t.Fatalf("terminal history = %v, want aborted", terminals)
	}
}

func (r notifyingFinalizeRepository) Finalize(ctx context.Context, input acceptance, publish func() (string, error)) (Outcome, error) {
	close(r.started)
	return r.acceptanceRepository.Finalize(ctx, input, publish)
}

func TestServiceFinalizationDoesNotSelfContendWithHeartbeatIntegration(t *testing.T) {
	pool, repository := uploadIntegrationRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := validRequest(bytes.NewBufferString("heartbeat-contention"))
	barrier, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Release()
	barrierKey := IdempotencyAdvisoryLock(IdempotencyScopeMediaUpload, request.IdempotencyKey)
	if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, barrierKey); err != nil {
		t.Fatalf("hold later idempotency lock: %v", err)
	}
	defer barrier.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey)
	started := make(chan struct{})
	temporary := &fakeStagedOriginal{
		publishKey: "originals/00/00000000-0000-4000-8000-000000000001/original.jpg",
	}
	service := testService(
		&fakeOriginalStore{temporary: temporary},
		&fakeMetadataProber{result: validMetadata()},
		notifyingFinalizeRepository{acceptanceRepository: repository, started: started},
	)
	service.heartbeatInterval = 10 * time.Millisecond
	service.heartbeatBudget = 25 * time.Millisecond
	service.dbBudget = 2 * time.Second

	type result struct {
		outcome Outcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := service.Accept(ctx, request)
		resultCh <- result{outcome: outcome, err: err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("final transaction did not start: %v", ctx.Err())
	}
	select {
	case got := <-resultCh:
		t.Fatalf("finalization escaped held later lock: outcome=%+v error=%v", got.outcome, got.err)
	case <-time.After(150 * time.Millisecond):
	}
	var unlocked bool
	if err := barrier.QueryRow(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release later idempotency lock: unlocked=%t error=%v", unlocked, err)
	}
	got := <-resultCh
	outcome, err := got.outcome, got.err
	if err != nil {
		t.Fatalf("Accept() after held later lock error = %v", err)
	}
	if outcome.Status != 201 || !temporary.published || temporary.aborted {
		t.Fatalf("outcome/temp = %+v, published=%t aborted=%t", outcome, temporary.published, temporary.aborted)
	}
}
