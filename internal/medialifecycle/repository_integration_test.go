package medialifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

func TestLifecycleRepositoryIntegrationDeleteRetentionAndRestore(t *testing.T) {
	tests := []struct {
		name      string
		retention *int
	}{
		{name: "null"},
		{name: "zero", retention: intPointer(0)},
		{name: "positive", retention: intPointer(7)},
		{name: "maximum", retention: intPointer(MaxDeletedMediaRetentionDays)},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool, service := integrationService(t)
			mediaID := integrationUUID(index + 1)
			insertMedia(t, pool, mediaID, integrationUUID(index+101))
			if _, err := pool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=$1 WHERE id=1`, test.retention); err != nil {
				t.Fatal(err)
			}

			first, err := service.Delete(ctx, mediaID)
			if err != nil {
				t.Fatal(err)
			}
			if !first.Changed || first.Media.DeletedAt == nil || first.Media.CurrentRenditions == nil || first.Media.Jobs == nil {
				t.Fatalf("first delete = %#v", first)
			}
			if test.retention == nil && first.Media.PurgeAfter != nil {
				t.Fatalf("NULL retention deadline = %v", first.Media.PurgeAfter)
			}
			if test.retention != nil {
				want := first.Media.DeletedAt.Add(time.Duration(*test.retention) * 24 * time.Hour)
				if first.Media.PurgeAfter == nil || !first.Media.PurgeAfter.Equal(want) {
					t.Fatalf("deadline = %v, want %v", first.Media.PurgeAfter, want)
				}
			}

			if _, err := pool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=30 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			repeated, err := service.Delete(ctx, mediaID)
			if err != nil || repeated.Changed || !sameTime(repeated.Media.DeletedAt, first.Media.DeletedAt) || !sameTime(repeated.Media.PurgeAfter, first.Media.PurgeAfter) {
				t.Fatalf("repeat delete = %#v, %v", repeated, err)
			}
			assertEventCounts(t, pool, 1, 0)

			restored, err := service.Restore(ctx, mediaID)
			if err != nil || restored.Media.DeletedAt != nil || restored.Media.PurgeAfter != nil {
				t.Fatalf("restore = %#v, %v", restored, err)
			}
			assertEventCounts(t, pool, 1, 1)
			var payload map[string]any
			if err := pool.QueryRow(ctx, `SELECT payload FROM change_events WHERE reason='restore'`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if _, hasJobs := payload["jobs"]; hasJobs || payload["deleted_at"] != nil {
				t.Fatalf("restore payload = %#v", payload)
			}

			redeleted, err := service.Delete(ctx, mediaID)
			if err != nil || !redeleted.Changed || redeleted.Media.PurgeAfter == nil || !redeleted.Media.PurgeAfter.Equal(redeleted.Media.DeletedAt.Add(30*24*time.Hour)) {
				t.Fatalf("re-delete = %#v, %v", redeleted, err)
			}
		})
	}
}

func TestLifecycleRepositoryIntegrationPurgeSemanticsAndCancellation(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(20)
	insertMedia(t, pool, mediaID, integrationUUID(120))
	if _, err := service.EnqueuePurge(ctx, mediaID); !errorCodeIs(err, CodeMediaNotDeleted) {
		t.Fatalf("active enqueue error = %#v", err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}

	created, err := service.EnqueuePurge(ctx, mediaID)
	if err != nil || created.Disposition != EnqueueCreated || created.Job.Status != readapi.JobQueued || created.Job.MaxAttempts != InitialPurgeMaxAttempts || created.Job.Targets == nil || len(created.Job.Targets) != 0 {
		t.Fatalf("created enqueue = %#v, %v", created, err)
	}
	repeated, err := service.EnqueuePurge(ctx, mediaID)
	if err != nil || repeated.Disposition != EnqueueExistingQueued || repeated.Job.ID != created.Job.ID {
		t.Fatalf("repeated enqueue = %#v, %v", repeated, err)
	}

	restored, err := service.Restore(ctx, mediaID)
	if err != nil || len(restored.Media.Jobs) != 1 || restored.Media.Jobs[0].Status != readapi.JobCancelled || restored.Media.Jobs[0].CancelledAt == nil {
		t.Fatalf("restore cancellation = %#v, %v", restored, err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	second, err := service.EnqueuePurge(ctx, mediaID)
	if err != nil || second.Disposition != EnqueueCreated || second.Job.ID == created.Job.ID {
		t.Fatalf("enqueue after cancelled history = %#v, %v", second, err)
	}
}

func TestLifecycleRepositoryIntegrationStartedAndFailedConflicts(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "running", status: "running"},
		{name: "queued retry", status: "queued"},
		{name: "failed", status: "failed"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool, service := integrationService(t)
			mediaID, originalID := integrationUUID(30+index), integrationUUID(130+index)
			insertMedia(t, pool, mediaID, originalID)
			if _, err := service.Delete(ctx, mediaID); err != nil {
				t.Fatal(err)
			}
			enqueued, err := service.EnqueuePurge(ctx, mediaID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,started_at=$2::timestamptz,lease_token=$3,lease_expires_at=$2::timestamptz+interval '1 hour',updated_at=$2::timestamptz WHERE id=$1`, enqueued.Job.ID, now, integrationUUID(900+index)); err != nil {
				t.Fatal(err)
			}
			switch test.status {
			case "queued":
				if _, err := pool.Exec(ctx, `UPDATE jobs SET status='queued',lease_token=NULL,lease_expires_at=NULL,available_at=$2,updated_at=$2 WHERE id=$1`, enqueued.Job.ID, now); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed',lease_token=NULL,lease_expires_at=NULL,finished_at=$2,error_code='failed',error_message='failed',updated_at=$2 WHERE id=$1`, enqueued.Job.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			if test.status != "failed" {
				repeated, err := service.EnqueuePurge(ctx, mediaID)
				wantDisposition := EnqueueExistingRunning
				if test.status == "queued" {
					wantDisposition = EnqueueExistingQueued
				}
				if err != nil || repeated.Job.ID != enqueued.Job.ID || repeated.Disposition != wantDisposition {
					t.Fatalf("started duplicate enqueue = %#v, %v", repeated, err)
				}
			}

			if _, err := service.Restore(ctx, mediaID); !errorWithJob(err, CodePurgeAlreadyStarted, enqueued.Job.ID) {
				t.Fatalf("restore error = %#v", err)
			}
			if test.status == "failed" {
				if _, err := service.EnqueuePurge(ctx, mediaID); !errorWithJob(err, CodePurgeFailedUseRetry, enqueued.Job.ID) {
					t.Fatalf("failed enqueue error = %#v", err)
				}
			}
		})
	}
}

func TestLifecycleRepositoryIntegrationStartPurgeLeaseAndEligibility(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(40)
	insertMedia(t, pool, mediaID, integrationUUID(140))
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	enqueued, err := service.EnqueuePurge(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()+interval '1 hour',updated_at=clock_timestamp() WHERE id=$1`, enqueued.Job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPurge(ctx, enqueued.Job.ID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("future StartPurge error = %#v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()-interval '1 second',updated_at=clock_timestamp() WHERE id=$1`, enqueued.Job.ID); err != nil {
		t.Fatal(err)
	}

	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	first, err := service.StartPurge(ctx, enqueued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if first.JobID != enqueued.Job.ID || first.MediaID != mediaID || first.Attempts != 1 ||
		first.MaxAttempts != InitialPurgeMaxAttempts || !readapi.IsUUIDv4(first.Token) ||
		first.LeaseExpiresAt.Before(before.Add(DefaultPurgeLeaseDuration)) ||
		first.LeaseExpiresAt.After(after.Add(DefaultPurgeLeaseDuration)) {
		t.Fatalf("first purge lease = %#v, clock bounds %v..%v", first, before, after)
	}
	if _, err := service.StartPurge(ctx, enqueued.Job.ID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("running StartPurge error = %#v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status='queued',lease_token=NULL,lease_expires_at=NULL,
			available_at=clock_timestamp()-interval '1 second',updated_at=clock_timestamp()
		WHERE id=$1`, enqueued.Job.ID); err != nil {
		t.Fatal(err)
	}
	second, err := service.StartPurge(ctx, enqueued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempts != 2 || second.Token == first.Token || !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("retry purge lease = %#v, first = %#v", second, first)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status='queued',lease_token=NULL,lease_expires_at=NULL,
			available_at=clock_timestamp()-interval '1 second',updated_at=clock_timestamp()
		WHERE id=$1`, enqueued.Job.ID); err != nil {
		t.Fatal(err)
	}
	third, err := service.StartPurge(ctx, enqueued.Job.ID)
	if err != nil || third.Attempts != InitialPurgeMaxAttempts || !third.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("final-budget purge lease = %#v, %v", third, err)
	}

	missingMediaID := integrationUUID(41)
	insertMedia(t, pool, missingMediaID, integrationUUID(141))
	if _, err := service.Delete(ctx, missingMediaID); err != nil {
		t.Fatal(err)
	}
	missing, err := service.EnqueuePurge(ctx, missingMediaID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, missingMediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPurge(ctx, missing.Job.ID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("missing Media StartPurge error = %#v", err)
	}
	if _, err := service.StartPurge(ctx, integrationUUID(999)); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("missing Job StartPurge error = %#v", err)
	}
}

func TestLifecycleRepositoryIntegrationStartPurgeRestoreRaces(t *testing.T) {
	t.Run("restore wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, service := integrationService(t)
		mediaID := integrationUUID(42)
		insertMedia(t, pool, mediaID, integrationUUID(142))
		if _, err := service.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := service.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}

		restoreTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer restoreTx.Rollback(context.Background())
		if _, err := restoreTx.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
			t.Fatal(err)
		}
		startConn, startService, startPID := connectionService(t, ctx, pool)
		defer startConn.Release()
		result := make(chan error, 1)
		go func() {
			_, err := startService.StartPurge(ctx, enqueued.Job.ID)
			result <- err
		}()
		awaitLockWait(t, ctx, pool, startPID)

		now := time.Now().UTC()
		if _, err := restoreTx.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, enqueued.Job.ID); err != nil {
			t.Fatalf("StartPurge locked Job before Media: %v", err)
		}
		if _, err := restoreTx.Exec(ctx, `UPDATE jobs SET status='cancelled',finished_at=$2,cancelled_at=$2,cancel_reason='media_restored',updated_at=$2 WHERE id=$1`, enqueued.Job.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := restoreTx.Exec(ctx, `UPDATE media SET deleted_at=NULL,purge_after=NULL WHERE id=$1`, mediaID); err != nil {
			t.Fatal(err)
		}
		if err := restoreTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, ErrNoPurgeWork) {
			t.Fatalf("StartPurge after restore = %#v", err)
		}
		assertJobState(t, pool, enqueued.Job.ID, "cancelled", 0, false)
	})

	t.Run("start wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, setupService := integrationService(t)
		mediaID := integrationUUID(43)
		insertMedia(t, pool, mediaID, integrationUUID(143))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := setupService.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}

		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
			t.Fatal(err)
		}
		startConn, startService, startPID := connectionService(t, ctx, pool)
		defer startConn.Release()
		restoreConn, restoreService, restorePID := connectionService(t, ctx, pool)
		defer restoreConn.Release()
		startResult := make(chan error, 1)
		go func() {
			_, err := startService.StartPurge(ctx, enqueued.Job.ID)
			startResult <- err
		}()
		awaitLockWait(t, ctx, pool, startPID)
		restoreResult := make(chan error, 1)
		go func() {
			_, err := restoreService.Restore(ctx, mediaID)
			restoreResult <- err
		}()
		awaitLockWait(t, ctx, pool, restorePID)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-startResult; err != nil {
			t.Fatalf("winning StartPurge error = %v", err)
		}
		if err := <-restoreResult; !errorWithJob(err, CodePurgeAlreadyStarted, enqueued.Job.ID) {
			t.Fatalf("losing Restore error = %#v", err)
		}
		assertJobState(t, pool, enqueued.Job.ID, "running", 1, true)
	})
}

func TestLifecycleRepositoryIntegrationStartPurgeGuards(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(44)
	insertMedia(t, pool, mediaID, integrationUUID(144))
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	enqueued, err := service.EnqueuePurge(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPurge(ctx, enqueued.Job.ID); !IsKind(err, KindDatabaseUnavailable) {
		t.Fatalf("maintenance StartPurge error = %#v", err)
	}
	assertJobState(t, pool, enqueued.Job.ID, "queued", 0, false)
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPurge(ctx, enqueued.Job.ID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("cancelled StartPurge error = %#v", err)
	}
}

func TestLifecycleRepositoryIntegrationConcurrentDeleteAndEnqueue(t *testing.T) {
	t.Run("concurrent delete emits one event", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID := integrationUUID(50)
		insertMedia(t, pool, mediaID, integrationUUID(150))
		results := make(chan DeleteResult, 2)
		errorsChannel := make(chan error, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		for range 2 {
			go func() {
				ready.Done()
				<-start
				result, err := service.Delete(ctx, mediaID)
				results <- result
				errorsChannel <- err
			}()
		}
		ready.Wait()
		close(start)
		first, second := <-results, <-results
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
		if first.Changed == second.Changed || !sameTime(first.Media.DeletedAt, second.Media.DeletedAt) {
			t.Fatalf("delete results = %#v / %#v", first, second)
		}
		assertEventCounts(t, pool, 1, 0)
	})

	t.Run("concurrent enqueue returns one job", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID := integrationUUID(51)
		insertMedia(t, pool, mediaID, integrationUUID(151))
		if _, err := service.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		results := make(chan EnqueueResult, 2)
		errorsChannel := make(chan error, 2)
		start := make(chan struct{})
		for range 2 {
			go func() {
				<-start
				result, err := service.EnqueuePurge(ctx, mediaID)
				results <- result
				errorsChannel <- err
			}()
		}
		close(start)
		first, second := <-results, <-results
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
		if first.Job.ID != second.Job.ID || first.Disposition == second.Disposition {
			t.Fatalf("enqueue results = %#v / %#v", first, second)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='purge'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("purge count = %d, %v", count, err)
		}
	})

	t.Run("restore and enqueue serialize", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID := integrationUUID(52)
		insertMedia(t, pool, mediaID, integrationUUID(152))
		if _, err := service.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		restoreErrors := make(chan error, 1)
		enqueueErrors := make(chan error, 1)
		go func() {
			<-start
			_, err := service.Restore(ctx, mediaID)
			restoreErrors <- err
		}()
		go func() {
			<-start
			_, err := service.EnqueuePurge(ctx, mediaID)
			enqueueErrors <- err
		}()
		close(start)
		if err := <-restoreErrors; err != nil {
			t.Fatalf("restore error = %v", err)
		}
		if err := <-enqueueErrors; err != nil && !errorCodeIs(err, CodeMediaNotDeleted) {
			t.Fatalf("enqueue error = %#v", err)
		}
		var deleted bool
		var blockingJobs int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT deleted_at IS NOT NULL FROM media WHERE id=$1),
			(SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge' AND status IN ('queued','running','failed'))`, mediaID).Scan(&deleted, &blockingJobs); err != nil {
			t.Fatal(err)
		}
		if deleted || blockingJobs != 0 {
			t.Fatalf("final deleted/blocking = %v/%d", deleted, blockingJobs)
		}
	})

	t.Run("direct purge insert waits for restore and rejects active media", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, service := integrationService(t)
		mediaID := integrationUUID(53)
		insertMedia(t, pool, mediaID, integrationUUID(153))
		if _, err := service.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}

		restoreTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer restoreTx.Rollback(context.Background())
		if _, err := restoreTx.Exec(ctx, `UPDATE media SET deleted_at=NULL,purge_after=NULL WHERE id=$1`, mediaID); err != nil {
			t.Fatal(err)
		}

		inserter, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer inserter.Release()
		var inserterPID int32
		if err := inserter.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&inserterPID); err != nil {
			t.Fatal(err)
		}
		insertResult := make(chan error, 1)
		go func() {
			_, err := inserter.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, integrationUUID(953), mediaID)
			insertResult <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, inserterPID).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("direct purge insert did not wait for the Media row lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := restoreTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-insertResult; err == nil {
			t.Fatal("direct purge insert succeeded after concurrent restore")
		}
		var purgeJobs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge'`, mediaID).Scan(&purgeJobs); err != nil || purgeJobs != 0 {
			t.Fatalf("purge jobs after restore race = %d, %v", purgeJobs, err)
		}
	})
}

func TestLifecycleRepositoryIntegrationMaintenanceAndEventRollback(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(60)
	insertMedia(t, pool, mediaID, integrationUUID(160))
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='test',entered_at=statement_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); !IsKind(err, KindDatabaseUnavailable) {
		t.Fatalf("maintenance error = %#v", err)
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1`, mediaID).Scan(&deletedAt); err != nil || deletedAt != nil {
		t.Fatalf("maintenance mutation = %v, %v", deletedAt, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE change_feed_state SET last_position=9223372036854775807 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); !IsKind(err, KindInvariant) {
		t.Fatalf("feed overflow error = %#v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1`, mediaID).Scan(&deletedAt); err != nil || deletedAt != nil {
		t.Fatalf("overflow rollback = %v, %v", deletedAt, err)
	}
}

func integrationService(t *testing.T) (*pgxpool.Pool, *Service) {
	t.Helper()
	pool := integrationPool(t)
	service, err := NewService(pool, "https://files.example/files")
	if err != nil {
		t.Fatal(err)
	}
	return pool, service
}

func connectionService(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, *Service, int32) {
	t.Helper()
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := connection.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		connection.Release()
		t.Fatal(err)
	}
	repository := &PostgresRepository{db: connection, fileBaseURL: "https://files.example/files/", newID: newUUIDv4}
	return connection, newService(repository), pid
}

func awaitLockWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d did not wait for a lock", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertJobState(t *testing.T, pool *pgxpool.Pool, jobID, wantStatus string, wantAttempts int, wantStarted bool) {
	t.Helper()
	var status string
	var attempts int
	var startedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT status,attempts,started_at FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts, &startedAt); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts || (startedAt != nil) != wantStarted {
		t.Fatalf("job state = %s attempts=%d started_at=%v, want %s/%d started=%v", status, attempts, startedAt, wantStatus, wantAttempts, wantStarted)
	}
}

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("NMCP_TEST_DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	schema := "lifecycle_" + strings.ReplaceAll(id, "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop lifecycle integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxConns < 4 {
		config.MaxConns = 4
	}
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,false)`, schema+",pg_catalog,pg_temp")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := dbmigration.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

func insertMedia(t *testing.T, pool *pgxpool.Pool, mediaID, originalID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($2,$1,$3,$4,'image/jpeg',10,1,1)`, mediaID, originalID,
		strings.Repeat("0", 63)+originalID[len(originalID)-1:], "originals/"+originalID[:2]+"/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
}

func integrationUUID(value int) string {
	return fmt.Sprintf("10000000-0000-4000-8000-%012x", value)
}

func intPointer(value int) *int { return &value }

func sameTime(first, second *time.Time) bool {
	return first == nil && second == nil || first != nil && second != nil && first.Equal(*second)
}

func assertEventCounts(t *testing.T, pool *pgxpool.Pool, deleted, restored int) {
	t.Helper()
	var gotDeleted, gotRestored int
	if err := pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE reason='logical_delete'),count(*) FILTER (WHERE reason='restore') FROM change_events`).Scan(&gotDeleted, &gotRestored); err != nil {
		t.Fatal(err)
	}
	if gotDeleted != deleted || gotRestored != restored {
		t.Fatalf("event counts = %d/%d, want %d/%d", gotDeleted, gotRestored, deleted, restored)
	}
}

func errorCodeIs(err error, code ErrorCode) bool {
	var semantic *SemanticError
	return errors.As(err, &semantic) && semantic.Code() == code
}

func errorWithJob(err error, code ErrorCode, jobID string) bool {
	var semantic *SemanticError
	return errors.As(err, &semantic) && semantic.Code() == code && semantic.Details()["job_id"] == jobID
}
