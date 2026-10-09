package medialifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestCleanupFailureIntegrationDeleteRenditionTerminalEvidence(t *testing.T) {
	t.Run("exact unlink and parent sync commit deleted", func(t *testing.T) {
		ctx := context.Background()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		fixture, root := cleanupFailureFixture(t, pool, 4000, true)

		var events []storage.FaultEvent
		store := openCleanupFailureStore(t, root, storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
			if event.Boundary == storage.BoundaryDelete || event.Boundary == storage.BoundaryDeleteDirectorySync {
				events = append(events, event)
			}
			return nil
		}))
		id, err := repository.CleanupNextRendition(ctx, "", nil, cleanupDeleteRendition(store))
		if err != nil || id != fixture.renditionID {
			t.Fatalf("CleanupNextRendition() = %q, %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(fixture.relativePath))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted path stat error = %v", err)
		}
		want := []storage.FaultEvent{
			{Boundary: storage.BoundaryDelete, Phase: storage.Before, Key: fixture.relativePath},
			{Boundary: storage.BoundaryDelete, Phase: storage.After, Key: fixture.relativePath},
			{Boundary: storage.BoundaryDeleteDirectorySync, Phase: storage.Before, Key: fixture.relativePath},
			{Boundary: storage.BoundaryDeleteDirectorySync, Phase: storage.After, Key: fixture.relativePath},
		}
		if len(events) != len(want) {
			t.Fatalf("delete durability events = %#v, want %#v", events, want)
		}
		for index := range want {
			if events[index] != want[index] {
				t.Fatalf("delete durability event %d = %#v, want %#v", index, events[index], want[index])
			}
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
	})

	t.Run("already missing commits missing", func(t *testing.T) {
		ctx := context.Background()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		fixture, root := cleanupFailureFixture(t, pool, 4020, false)
		store := openCleanupFailureStore(t, root, nil)

		id, err := repository.CleanupNextRendition(ctx, "", nil, cleanupDeleteRendition(store))
		if err != nil || id != fixture.renditionID {
			t.Fatalf("CleanupNextRendition() = %q, %v", id, err)
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "missing")
	})
}

func TestCleanupFailureIntegrationStorageFailuresRollbackAndConverge(t *testing.T) {
	tests := []struct {
		name          string
		boundary      storage.Boundary
		phase         storage.Phase
		unlinked      bool
		wantUncertain bool
		wantDurable   bool
	}{
		{name: "action failure before unlink", boundary: storage.BoundaryDelete, phase: storage.Before},
		{name: "response lost after unlink", boundary: storage.BoundaryDelete, phase: storage.After, unlinked: true, wantUncertain: true},
		{name: "failure before parent sync", boundary: storage.BoundaryDeleteDirectorySync, phase: storage.Before, unlinked: true, wantUncertain: true, wantDurable: true},
		{name: "response lost after parent sync", boundary: storage.BoundaryDeleteDirectorySync, phase: storage.After, unlinked: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool := cleanupIntegrationPool(t)
			repository := newCleanupTestRepository(t, pool)
			fixture, root := cleanupFailureFixture(t, pool, 4100+index*20, true)
			injected := errors.New("injected cleanup storage failure")
			faulty := openCleanupFailureStore(t, root, storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
				if event.Boundary == test.boundary && event.Phase == test.phase {
					return injected
				}
				return nil
			}))

			id, err := repository.CleanupNextRendition(ctx, "", nil, cleanupDeleteRendition(faulty))
			if id != fixture.renditionID || !errors.Is(err, injected) {
				t.Fatalf("faulted CleanupNextRendition() = %q, %#v", id, err)
			}
			if errors.Is(err, storage.ErrOutcomeUncertain) != test.wantUncertain || errors.Is(err, storage.ErrDurability) != test.wantDurable {
				t.Fatalf("fault classification = %#v, want uncertain=%t durability=%t", err, test.wantUncertain, test.wantDurable)
			}
			_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(fixture.relativePath)))
			if test.unlinked && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("post-unlink path stat error = %v", statErr)
			}
			if !test.unlinked && statErr != nil {
				t.Fatalf("pre-unlink failure changed path: %v", statErr)
			}
			assertCleanupFailureState(t, pool, fixture.renditionID, true, "")

			if !test.unlinked {
				return
			}
			clean := openCleanupFailureStore(t, root, nil)
			id, err = repository.CleanupNextRendition(ctx, fixture.renditionID, nil, cleanupDeleteRendition(clean))
			if err != nil || id != fixture.renditionID {
				t.Fatalf("converging CleanupNextRendition() = %q, %v", id, err)
			}
			assertCleanupFailureState(t, pool, fixture.renditionID, false, "missing")
		})
	}
}

func TestCleanupFailureIntegrationCommittedResponseConvergesWithoutSecondDelete(t *testing.T) {
	ctx := context.Background()
	pool := cleanupIntegrationPool(t)
	repository := newCleanupTestRepository(t, pool)
	fixture, root := cleanupFailureFixture(t, pool, 4200, true)
	store := openCleanupFailureStore(t, root, nil)
	lost := errors.New("cleanup commit response lost")
	var fail atomic.Bool
	fail.Store(true)
	repository.afterCommit = func(context.Context) error {
		if fail.Swap(false) {
			return lost
		}
		return nil
	}
	var calls atomic.Int32
	action := cleanupDeleteRendition(store)
	counted := func(ctx context.Context, id, path string, size int64) (bool, error) {
		calls.Add(1)
		return action(ctx, id, path, size)
	}

	id, err := repository.CleanupNextRendition(ctx, "", nil, counted)
	var unknown *CommitOutcomeUnknown
	if id != fixture.renditionID || !errors.As(err, &unknown) || !errors.Is(err, lost) {
		t.Fatalf("uncertain CleanupNextRendition() = %q, %#v", id, err)
	}
	assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
	id, err = repository.CleanupNextRendition(ctx, fixture.renditionID, nil, counted)
	if err != nil || id != fixture.renditionID || calls.Load() != 1 {
		t.Fatalf("converged CleanupNextRendition() = %q, %v; delete calls=%d", id, err, calls.Load())
	}
}

func TestCleanupFailureIntegrationRetainsLocksThroughStorageCallback(t *testing.T) {
	t.Run("maintenance waits for delete and database terminal state", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		fixture, root := cleanupFailureFixture(t, pool, 4300, true)
		store := openCleanupFailureStore(t, root, nil)
		storageDone := make(chan struct{})
		release := make(chan struct{})
		cleanupResult := make(chan error, 1)
		action := cleanupDeleteRendition(store)
		go func() {
			_, err := repository.CleanupNextRendition(ctx, "", nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
				missing, err := action(ctx, id, path, size)
				close(storageDone)
				if err == nil {
					select {
					case <-release:
					case <-ctx.Done():
						err = ctx.Err()
					}
				}
				return missing, err
			})
			cleanupResult <- err
		}()
		var cleanupErr error
		cleanupReturned := false
		select {
		case <-storageDone:
		case cleanupErr = <-cleanupResult:
			cleanupReturned = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if cleanupReturned {
			t.Fatalf("cleanup returned before storage callback: %v", cleanupErr)
		}

		maintenance, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer maintenance.Release()
		var pid int32
		if err := maintenance.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		maintenanceResult := make(chan error, 1)
		go func() {
			_, err := maintenance.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='cleanup callback drain',owner='test',entered_at=clock_timestamp() WHERE id=1`)
			maintenanceResult <- err
		}()
		awaitLockWait(t, ctx, pool, pid)
		close(release)
		if !cleanupReturned {
			select {
			case cleanupErr = <-cleanupResult:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		select {
		case err := <-maintenanceResult:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
	})

	t.Run("StartPurge waits and observes cleanup commit", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		service, err := NewService(pool, "https://files.example/files")
		if err != nil {
			t.Fatal(err)
		}
		fixture, root := cleanupFailureFixture(t, pool, 4320, true)
		store := openCleanupFailureStore(t, root, nil)
		if _, err := service.Delete(ctx, fixture.mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := service.EnqueuePurge(ctx, fixture.mediaID)
		if err != nil {
			t.Fatal(err)
		}

		storageDone := make(chan struct{})
		release := make(chan struct{})
		cleanupResult := make(chan error, 1)
		action := cleanupDeleteRendition(store)
		go func() {
			_, err := repository.CleanupNextRendition(ctx, "", nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
				missing, err := action(ctx, id, path, size)
				close(storageDone)
				if err == nil {
					select {
					case <-release:
					case <-ctx.Done():
						err = ctx.Err()
					}
				}
				return missing, err
			})
			cleanupResult <- err
		}()
		var cleanupErr error
		cleanupReturned := false
		select {
		case <-storageDone:
		case cleanupErr = <-cleanupResult:
			cleanupReturned = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if cleanupReturned {
			t.Fatalf("cleanup returned before storage callback: %v", cleanupErr)
		}

		connection, startService, pid := connectionService(t, ctx, pool)
		defer connection.Release()
		type startOutcome struct {
			lease PurgeLease
			err   error
		}
		startResult := make(chan startOutcome, 1)
		go func() {
			lease, err := startService.StartPurge(ctx, enqueued.Job.ID)
			startResult <- startOutcome{lease: lease, err: err}
		}()
		awaitLockWait(t, ctx, pool, pid)
		close(release)
		if !cleanupReturned {
			select {
			case cleanupErr = <-cleanupResult:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		var started startOutcome
		select {
		case started = <-startResult:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if started.err != nil || started.lease.JobID != enqueued.Job.ID {
			t.Fatalf("StartPurge() = %#v, %v", started.lease, started.err)
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
		var manifestRows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM purge_file_progress WHERE job_id=$1 AND object_id=$2`, enqueued.Job.ID, fixture.renditionID).Scan(&manifestRows); err != nil {
			t.Fatal(err)
		}
		if manifestRows != 0 {
			t.Fatalf("started purge retained %d manifest rows for cleaned Rendition", manifestRows)
		}
	})
}

func TestCleanupFailureIntegrationStartPurgeQueuesFirstAndOwnsExactRendition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := cleanupIntegrationPool(t)
	setupService, err := NewService(pool, "https://files.example/files")
	if err != nil {
		t.Fatal(err)
	}
	fixture, _ := cleanupFailureFixture(t, pool, 4330, false)
	if _, err := setupService.Delete(ctx, fixture.mediaID); err != nil {
		t.Fatal(err)
	}
	enqueued, err := setupService.EnqueuePurge(ctx, fixture.mediaID)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, fixture.mediaID); err != nil {
		t.Fatal(err)
	}
	var blockerPID int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	startConnection, startService, startPID := connectionService(t, ctx, pool)
	defer startConnection.Release()
	type startOutcome struct {
		lease PurgeLease
		err   error
	}
	startResult := make(chan startOutcome, 1)
	go func() {
		lease, err := startService.StartPurge(ctx, enqueued.Job.ID)
		startResult <- startOutcome{lease: lease, err: err}
	}()
	awaitLockWait(t, ctx, pool, startPID)

	cleanupConnection, cleanupRepository, cleanupPID := cleanupConnectionRepository(t, ctx, pool)
	defer cleanupConnection.Release()
	var calls atomic.Int32
	cleanupResult := make(chan error, 1)
	go func() {
		_, err := cleanupRepository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			calls.Add(1)
			return false, nil
		})
		cleanupResult <- err
	}()
	awaitLockWait(t, ctx, pool, cleanupPID)
	awaitBlockedMediaLockCount(t, ctx, pool, blockerPID, 2)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var started startOutcome
	select {
	case started = <-startResult:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if started.err != nil || started.lease.JobID != enqueued.Job.ID {
		t.Fatalf("StartPurge() = %#v, %v", started.lease, started.err)
	}
	select {
	case err := <-cleanupResult:
		if err != nil {
			t.Fatalf("cleanup after purge takeover: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 0 {
		t.Fatalf("cleanup callback calls=%d, want 0 after purge ownership", calls.Load())
	}
	var path string
	var size int64
	var disposition string
	if err := pool.QueryRow(ctx, `SELECT relative_path,size_bytes,disposition FROM purge_file_progress
		WHERE job_id=$1 AND object_kind='rendition' AND object_id=$2`, enqueued.Job.ID, fixture.renditionID).Scan(&path, &size, &disposition); err != nil {
		t.Fatal(err)
	}
	if path != fixture.relativePath || size != fixture.sizeBytes || disposition != "pending" {
		t.Fatalf("purge manifest path=%q size=%d disposition=%q", path, size, disposition)
	}
	var cleanupRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rendition_cleanup_progress WHERE rendition_id=$1`, fixture.renditionID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("cleanup progress rows=%d after purge takeover", cleanupRows)
	}
}

func TestCleanupFailureIntegrationRestoreOrdering(t *testing.T) {
	t.Run("cleanup holds Media through storage and Restore waits", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := cleanupIntegrationPool(t)
		cleanupService, err := NewService(pool, "https://files.example/files")
		if err != nil {
			t.Fatal(err)
		}
		fixture, root := cleanupFailureFixture(t, pool, 4340, true)
		if _, err := cleanupService.Delete(ctx, fixture.mediaID); err != nil {
			t.Fatal(err)
		}

		storageStarted := make(chan struct{})
		releaseStorage := make(chan struct{})
		var blocked atomic.Bool
		store := openCleanupFailureStore(t, root, storage.FaultInjectorFunc(func(ctx context.Context, event storage.FaultEvent) error {
			if event.Boundary == storage.BoundaryDelete && event.Phase == storage.Before && blocked.CompareAndSwap(false, true) {
				close(storageStarted)
				select {
				case <-releaseStorage:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}))
		cleanupResult := make(chan error, 1)
		go func() {
			_, err := cleanupService.CleanupNextRendition(ctx, "", nil, cleanupDeleteRendition(store))
			cleanupResult <- err
		}()
		var cleanupErr error
		cleanupReturned := false
		select {
		case <-storageStarted:
		case cleanupErr = <-cleanupResult:
			cleanupReturned = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if cleanupReturned {
			t.Fatalf("cleanup returned before storage callback: %v", cleanupErr)
		}

		restoreConnection, restoreService, restorePID := connectionService(t, ctx, pool)
		defer restoreConnection.Release()
		restoreResult := make(chan error, 1)
		go func() {
			_, err := restoreService.Restore(ctx, fixture.mediaID)
			restoreResult <- err
		}()
		awaitLockWait(t, ctx, pool, restorePID)
		close(releaseStorage)
		if !cleanupReturned {
			select {
			case cleanupErr = <-cleanupResult:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if cleanupErr != nil {
			t.Fatalf("CleanupNextRendition() error = %v", cleanupErr)
		}
		select {
		case err := <-restoreResult:
			if err != nil {
				t.Fatalf("Restore() error = %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
		assertCleanupMediaRestored(t, pool, fixture.mediaID)
	})

	t.Run("Restore wins before locked cleanup recheck", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := cleanupIntegrationPool(t)
		setupService, err := NewService(pool, "https://files.example/files")
		if err != nil {
			t.Fatal(err)
		}
		fixture, root := cleanupFailureFixture(t, pool, 4360, true)
		if _, err := setupService.Delete(ctx, fixture.mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := setupService.EnqueuePurge(ctx, fixture.mediaID)
		if err != nil {
			t.Fatal(err)
		}

		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, fixture.mediaID); err != nil {
			t.Fatal(err)
		}
		restoreConnection, restoreService, restorePID := connectionService(t, ctx, pool)
		defer restoreConnection.Release()
		cleanupConnection, cleanupRepository, cleanupPID := cleanupConnectionRepository(t, ctx, pool)
		defer cleanupConnection.Release()

		restoreResult := make(chan error, 1)
		go func() {
			_, err := restoreService.Restore(ctx, fixture.mediaID)
			restoreResult <- err
		}()
		awaitLockWait(t, ctx, pool, restorePID)
		store := openCleanupFailureStore(t, root, nil)
		var calls atomic.Int32
		cleanupResult := make(chan error, 1)
		go func() {
			_, err := cleanupRepository.CleanupNextRendition(ctx, "", nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
				calls.Add(1)
				return cleanupDeleteRendition(store)(ctx, id, path, size)
			})
			cleanupResult <- err
		}()
		awaitLockWait(t, ctx, pool, cleanupPID)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-restoreResult:
			if err != nil {
				t.Fatalf("winning Restore() error = %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case err := <-cleanupResult:
			if err != nil {
				t.Fatalf("CleanupNextRendition() after Restore = %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if calls.Load() != 1 {
			t.Fatalf("cleanup callback calls = %d, want 1", calls.Load())
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
		assertCleanupMediaRestored(t, pool, fixture.mediaID)
		assertJobState(t, pool, enqueued.Job.ID, "cancelled", 0, false)
	})
}

func TestCleanupFailureIntegrationServiceCancellationBoundary(t *testing.T) {
	t.Run("deadline before unlink never invokes callback", func(t *testing.T) {
		pool := cleanupIntegrationPool(t)
		service, err := NewService(pool, "https://files.example/files")
		if err != nil {
			t.Fatal(err)
		}
		fixture, _ := cleanupFailureFixture(t, pool, 4380, false)
		blocker, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(context.Background(), `SELECT id FROM media WHERE id=$1 FOR UPDATE`, fixture.mediaID); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var calls atomic.Int32
		id, err := service.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			calls.Add(1)
			return false, nil
		})
		if id != fixture.renditionID || !IsKind(err, KindDatabaseUnavailable) || calls.Load() != 0 {
			t.Fatalf("CleanupNextRendition() = %q, %#v; callback calls=%d", id, err, calls.Load())
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, true, "")
	})

	t.Run("external cancellation after unlink drains terminal database state", func(t *testing.T) {
		pool := cleanupIntegrationPool(t)
		service, err := NewService(pool, "https://files.example/files")
		if err != nil {
			t.Fatal(err)
		}
		fixture, root := cleanupFailureFixture(t, pool, 4400, true)
		ctx, cancel := context.WithCancel(context.Background())
		var cancelled atomic.Bool
		store := openCleanupFailureStore(t, root, storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
			if event.Boundary == storage.BoundaryDelete && event.Phase == storage.After && cancelled.CompareAndSwap(false, true) {
				cancel()
			}
			return nil
		}))
		id, err := service.CleanupNextRendition(ctx, "", nil, cleanupDeleteRendition(store))
		if err != nil || id != fixture.renditionID || !cancelled.Load() {
			t.Fatalf("CleanupNextRendition() = %q, %v; cancelled=%t", id, err, cancelled.Load())
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(fixture.relativePath))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted path stat error = %v", err)
		}
		assertCleanupFailureState(t, pool, fixture.renditionID, false, "deleted")
	})
}

func cleanupFailureFixture(t *testing.T, pool *pgxpool.Pool, seed int, publish bool) (cleanupRenditionFixture, string) {
	t.Helper()
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fixture := insertCleanupRenditions(t, pool, seed, &deadline, 1, 1, false)
	ctx := context.Background()
	var originalID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM originals WHERE media_id=$1`, fixture.mediaID).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	original, err := storage.ParseOriginalID(originalID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := storage.ParseJobTargetID(fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	rendition, err := storage.ParseRenditionID(fixture.renditionID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewRenditionKey(original, target, rendition, storage.RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	fixture.relativePath = key.String()
	if _, err := pool.Exec(ctx, `ALTER TABLE renditions DISABLE TRIGGER renditions_provenance`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE renditions ENABLE TRIGGER renditions_provenance`)
	})
	if _, err := pool.Exec(ctx, `UPDATE renditions SET relative_path=$2 WHERE id=$1`, fixture.renditionID, fixture.relativePath); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE renditions ENABLE TRIGGER renditions_provenance`); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if !publish {
		return fixture, root
	}
	store := openCleanupFailureStore(t, root, nil)
	temporary, err := store.BeginRendition(ctx, key, cleanupFailureAttemptID(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, fixture.sizeBytes)
	for index := range payload {
		payload[index] = byte(index)
	}
	if _, err := temporary.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(ctx, storage.Validation{ExpectedSize: fixture.sizeBytes}); err != nil {
		t.Fatal(err)
	}
	return fixture, root
}

func cleanupFailureAttemptID(t *testing.T, seed int) storage.AttemptID {
	t.Helper()
	attempt, err := storage.ParseAttemptID(integrationUUID(50000 + seed))
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func openCleanupFailureStore(t *testing.T, root string, faults storage.FaultInjector) *storage.Store {
	t.Helper()
	store, err := storage.Open(root, storage.Options{Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func cleanupDeleteRendition(store *storage.Store) func(context.Context, string, string, int64) (bool, error) {
	return func(ctx context.Context, renditionID, relativePath string, sizeBytes int64) (bool, error) {
		key, err := storage.ParseRenditionKey(relativePath)
		if err != nil || key.RenditionID().String() != renditionID || sizeBytes < 0 {
			return false, storage.ErrValidation
		}
		result, err := store.DeleteRendition(ctx, key, storage.DeleteExpectation{ExpectedSize: sizeBytes})
		return result.Missing, err
	}
}

func assertCleanupFailureState(t *testing.T, pool *pgxpool.Pool, renditionID string, renditionExists bool, disposition string) {
	t.Helper()
	var exists bool
	var count int
	var got *string
	if err := pool.QueryRow(context.Background(), `SELECT
		EXISTS(SELECT 1 FROM renditions WHERE id=$1),
		count(*),max(disposition) FROM rendition_cleanup_progress WHERE rendition_id=$1`, renditionID).Scan(&exists, &count, &got); err != nil {
		t.Fatal(err)
	}
	if exists != renditionExists {
		t.Fatalf("Rendition exists = %t, want %t", exists, renditionExists)
	}
	if disposition == "" {
		if count != 0 || got != nil {
			t.Fatalf("cleanup progress = count %d disposition %v, want absent", count, got)
		}
		return
	}
	if count != 1 || got == nil || *got != disposition {
		t.Fatalf("cleanup progress = count %d disposition %v, want one %q", count, got, disposition)
	}
}

func assertCleanupMediaRestored(t *testing.T, pool *pgxpool.Pool, mediaID string) {
	t.Helper()
	var restored bool
	if err := pool.QueryRow(context.Background(), `SELECT deleted_at IS NULL AND purge_after IS NULL FROM media WHERE id=$1`, mediaID).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("Media was not restored")
	}
}
