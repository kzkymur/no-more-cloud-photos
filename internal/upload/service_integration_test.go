//go:build linux

package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestServiceIntegrationDatabaseCheckpoints(t *testing.T) {
	t.Run("before commit rolls back and retry accepts without deleting orphan", func(t *testing.T) {
		pool := uploadIntegrationPool(t, nil)
		fault := errors.New("before commit fault")
		service, root, setBoundary := integrationService(t, pool, storage.BoundaryBeforeDBCommit, fault)

		_, err := service.Accept(context.Background(), integrationRequest("before-key", "before-request", "before-content"))
		var unknown *OutcomeUnknown
		if err == nil || errors.As(err, &unknown) {
			t.Fatalf("first Accept() error = %#v, want known pre-commit failure", err)
		}
		assertUploadRowCounts(t, pool, 0, 0, 0, 0, 0)
		assertFinalFileCount(t, root, 1)

		setBoundary("")
		outcome, err := service.Accept(context.Background(), integrationRequest("before-key", "before-retry", "before-content"))
		if err != nil || outcome.Status != 201 || outcome.Replayed {
			t.Fatalf("retry outcome=%+v error=%v", outcome, err)
		}
		assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
		assertFinalFileCount(t, root, 2)
	})

	t.Run("after commit loses response and same key replays", func(t *testing.T) {
		pool := uploadIntegrationPool(t, nil)
		fault := errors.New("after commit fault")
		service, root, setBoundary := integrationService(t, pool, storage.BoundaryAfterDBCommit, fault)

		_, err := service.Accept(context.Background(), integrationRequest("after-key", "after-request", "after-content"))
		var unknown *OutcomeUnknown
		if !errors.As(err, &unknown) || !errors.Is(err, fault) {
			t.Fatalf("first Accept() error = %#v, want OutcomeUnknown", err)
		}
		assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
		assertFinalFileCount(t, root, 1)

		setBoundary("")
		outcome, err := service.Accept(context.Background(), integrationRequest("after-key", "after-retry", "after-content"))
		if err != nil || outcome.Status != 201 || !outcome.Replayed {
			t.Fatalf("retry outcome=%+v error=%v", outcome, err)
		}
		assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
		assertFinalFileCount(t, root, 1)
	})
}

func TestServiceIntegrationCommitRolledBack(t *testing.T) {
	pool := uploadIntegrationPool(t, nil)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION test_abort_upload_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'test deferred commit failure' USING ERRCODE = '23514';
		END $$;
		CREATE CONSTRAINT TRIGGER test_abort_upload_commit
		AFTER INSERT ON media DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION test_abort_upload_commit()`); err != nil {
		t.Fatal(err)
	}
	service, root, _ := integrationService(t, pool, "", nil)

	_, err := service.Accept(ctx, integrationRequest("rollback-key", "rollback-request", "rollback-content"))
	var rolledBack *CommitRolledBack
	var unknown *OutcomeUnknown
	if !errors.As(err, &rolledBack) || errors.As(err, &unknown) {
		t.Fatalf("Accept() error = %#v, want CommitRolledBack", err)
	}
	assertUploadRowCounts(t, pool, 0, 0, 0, 0, 0)
	assertFinalFileCount(t, root, 1)

	if _, err := pool.Exec(ctx, `DROP TRIGGER test_abort_upload_commit ON media; DROP FUNCTION test_abort_upload_commit()`); err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Accept(ctx, integrationRequest("rollback-key", "rollback-retry", "rollback-content"))
	if err != nil || outcome.Status != 201 || outcome.Replayed {
		t.Fatalf("retry outcome=%+v error=%v", outcome, err)
	}
	assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
	assertFinalFileCount(t, root, 2)
}

func TestServiceIntegrationCommitDisconnectIsOutcomeUnknown(t *testing.T) {
	tracer := &commitDisconnectTracer{disabled: true, result: make(chan commitTerminationResult, 1)}
	pool := uploadIntegrationPool(t, func(config *pgxpool.Config, admin *pgxpool.Pool) {
		tracer.admin = admin
		config.MaxConns = 1
		config.ConnConfig.Tracer = tracer
	})
	service, root, _ := integrationService(t, pool, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const barrierKey = int64(0x4e4d435055504c44) // "NMCPUPLD"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION test_block_upload_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_catalog.pg_advisory_xact_lock(%d);
			RETURN NEW;
		END $$;
		CREATE CONSTRAINT TRIGGER test_block_upload_commit
		AFTER INSERT ON media DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION test_block_upload_commit()`, barrierKey)); err != nil {
		t.Fatal(err)
	}
	barrier, err := tracer.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire commit barrier connection: %v", err)
	}
	if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, barrierKey); err != nil {
		barrier.Release()
		t.Fatalf("acquire commit barrier: %v", err)
	}
	releaseBarrier := func() error {
		if barrier == nil {
			return nil
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		var unlocked bool
		err := barrier.QueryRow(releaseCtx, `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey).Scan(&unlocked)
		barrier.Release()
		barrier = nil
		if err != nil {
			return fmt.Errorf("release commit barrier: %w", err)
		}
		if !unlocked {
			return errors.New("commit barrier was not held")
		}
		return nil
	}
	t.Cleanup(func() {
		if err := releaseBarrier(); err != nil {
			t.Errorf("commit barrier cleanup: %v", err)
		}
	})
	tracer.enable()

	_, err = service.Accept(ctx, integrationRequest("disconnect-key", "disconnect-request", "disconnect-content"))
	var unknown *OutcomeUnknown
	if !errors.As(err, &unknown) {
		t.Fatalf("Accept() error = %#v, want OutcomeUnknown", err)
	}
	termination := awaitCommitTermination(t, ctx, tracer.result)
	if termination.err != nil || !termination.terminated {
		t.Fatalf("terminate blocked COMMIT: terminated=%t error=%v", termination.terminated, termination.err)
	}
	assertFinalFileCount(t, root, 1)
	assertUploadRowCounts(t, pool, 0, 0, 0, 0, 0)

	tracer.disable()
	if err := releaseBarrier(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER test_block_upload_commit ON media; DROP FUNCTION test_block_upload_commit()`); err != nil {
		t.Fatal(err)
	}
	var idempotencyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_requests WHERE scope=$1 AND key=$2`,
		IdempotencyScopeMediaUpload, "disconnect-key").Scan(&idempotencyCount); err != nil {
		t.Fatal(err)
	}
	if idempotencyCount != 0 {
		t.Fatalf("idempotency rows after terminated COMMIT = %d, want 0", idempotencyCount)
	}
	outcome, retryErr := service.Accept(ctx, integrationRequest("disconnect-key", "disconnect-retry", "disconnect-content"))
	if retryErr != nil || outcome.Status != 201 || outcome.Replayed {
		t.Fatalf("resolved count=%d retry outcome=%+v error=%v", idempotencyCount, outcome, retryErr)
	}
	assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
	assertFinalFileCount(t, root, 2)
}

type commitDisconnectTracer struct {
	admin    *pgxpool.Pool
	mu       sync.Mutex
	disabled bool
	started  bool
	result   chan commitTerminationResult
}

type commitTerminationResult struct {
	terminated bool
	err        error
}

func (t *commitDisconnectTracer) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	disabled := t.disabled
	if !disabled && !t.started && strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		t.started = true
	} else {
		disabled = true
	}
	t.mu.Unlock()
	if disabled {
		return ctx
	}
	go t.terminateWhenCommitWaits(connection.PgConn().PID())
	return ctx
}

func (*commitDisconnectTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *commitDisconnectTracer) disable() {
	t.mu.Lock()
	t.disabled = true
	t.mu.Unlock()
}

func (t *commitDisconnectTracer) enable() {
	t.mu.Lock()
	t.disabled = false
	t.started = false
	t.mu.Unlock()
}

func (t *commitDisconnectTracer) terminateWhenCommitWaits(pid uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := t.admin.QueryRow(ctx, `
			SELECT COALESCE(wait_event_type = 'Lock', false)
			FROM pg_catalog.pg_stat_activity
			WHERE pid = $1`, int32(pid)).Scan(&waiting)
		if err != nil {
			t.result <- commitTerminationResult{err: fmt.Errorf("poll COMMIT backend %d: %w", pid, err)}
			return
		}
		if waiting {
			var terminated bool
			err := t.admin.QueryRow(ctx, `SELECT pg_catalog.pg_terminate_backend($1)`, int32(pid)).Scan(&terminated)
			if err != nil {
				err = fmt.Errorf("terminate COMMIT backend %d: %w", pid, err)
			} else if !terminated {
				err = fmt.Errorf("terminate COMMIT backend %d returned false", pid)
			}
			t.result <- commitTerminationResult{terminated: terminated, err: err}
			return
		}
		select {
		case <-ctx.Done():
			t.result <- commitTerminationResult{err: fmt.Errorf("wait for COMMIT backend %d lock: %w", pid, ctx.Err())}
			return
		case <-ticker.C:
		}
	}
}

func awaitCommitTermination(t *testing.T, ctx context.Context, result <-chan commitTerminationResult) commitTerminationResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatalf("wait for COMMIT termination result: %v", ctx.Err())
		return commitTerminationResult{err: ctx.Err()}
	}
}

func integrationService(t *testing.T, pool *pgxpool.Pool, boundary storage.Boundary, fault error) (*Service, string, func(storage.Boundary)) {
	t.Helper()
	root := t.TempDir()
	var mu sync.Mutex
	failing := boundary
	store, err := storage.Open(root, storage.Options{Faults: storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if event.Boundary == failing {
			return fault
		}
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := newService(storageAdapter{store}, &fakeMetadataProber{result: validMetadata()}, newPGRepository(pool, store.DatabaseCheckpoint))
	next := 1
	service.newID = func() (string, error) {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", next)
		next++
		return id, nil
	}
	setBoundary := func(value storage.Boundary) {
		mu.Lock()
		failing = value
		mu.Unlock()
	}
	return service, root, setBoundary
}

func integrationRequest(key, requestID, content string) Request {
	filename := "photo.jpg"
	return Request{Body: bytes.NewBufferString(content), Filename: &filename, IdempotencyKey: key, RequestID: requestID}
}

func assertFinalFileCount(t *testing.T, root string, want int) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("durable final file count = %d, want %d", count, want)
	}
}
