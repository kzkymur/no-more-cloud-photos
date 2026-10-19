//go:build linux

package upload

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

type uploadServiceResult struct {
	outcome Outcome
	err     error
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

func TestServiceFinalizationDoesNotSelfContendWithHeartbeatIntegration(t *testing.T) {
	pool, repository := uploadIntegrationRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	var blockerPID int32
	if err := barrier.QueryRow(ctx, `SELECT pg_catalog.pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("read advisory blocker backend: %v", err)
	}
	temporary := &fakeStagedOriginal{
		publishKey: "originals/00/00000000-0000-4000-8000-000000000001/original.jpg",
	}
	service := testService(
		&fakeOriginalStore{temporary: temporary},
		&fakeMetadataProber{result: validMetadata()},
		repository,
	)
	service.heartbeatInterval = 100 * time.Millisecond
	service.heartbeatBudget = 2 * time.Second
	service.dbBudget = 5 * time.Second

	resultCh := make(chan uploadServiceResult, 1)
	go func() {
		outcome, err := service.Accept(ctx, request)
		resultCh <- uploadServiceResult{outcome: outcome, err: err}
	}()
	awaitUploadAdvisoryWait(t, ctx, pool, blockerPID, resultCh)
	var unlocked bool
	if err := barrier.QueryRow(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release later idempotency lock: unlocked=%t error=%v", unlocked, err)
	}
	var got uploadServiceResult
	select {
	case got = <-resultCh:
	case <-ctx.Done():
		t.Fatalf("await acceptance after advisory unlock: %v", ctx.Err())
	}
	outcome, err := got.outcome, got.err
	if err != nil {
		t.Fatalf("Accept() after held later lock error = %v", err)
	}
	if outcome.Status != 201 || !temporary.published || temporary.aborted {
		t.Fatalf("outcome/temp = %+v, published=%t aborted=%t", outcome, temporary.published, temporary.aborted)
	}
}

func awaitUploadAdvisoryWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blockerPID int32, result <-chan uploadServiceResult) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiterPID int32
		var waiterCount int
		err := pool.QueryRow(ctx, `SELECT COALESCE(min(activity.pid),0),count(*)
			FROM pg_catalog.pg_stat_activity AS activity
			WHERE $1=ANY(pg_catalog.pg_blocking_pids(activity.pid))
			  AND activity.wait_event_type='Lock'
			  AND activity.query LIKE 'SELECT pg_catalog.pg_advisory_xact_lock($1)%'`, blockerPID).Scan(&waiterPID, &waiterCount)
		if err != nil {
			t.Fatalf("observe finalization advisory waiter: %v", err)
		}
		if waiterCount > 1 {
			t.Fatalf("advisory blocker backend %d has %d matching finalization waiters", blockerPID, waiterCount)
		}
		if waiterCount == 1 {
			if waiterPID == blockerPID {
				t.Fatalf("finalization waiter reused blocker backend %d", blockerPID)
			}
			return
		}
		select {
		case got := <-result:
			t.Fatalf("Accept returned before advisory wait: outcome=%+v error=%v", got.outcome, got.err)
		case <-ctx.Done():
			t.Fatalf("final transaction did not reach advisory wait: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
