package medialifecycle

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestLifecycleRepositoryIntegrationDeferredCommitRejectionRollsBack(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID, originalID := integrationUUID(701), integrationUUID(702)
		insertMedia(t, pool, mediaID, originalID)
		jobID := integrationUUID(703)
		targetID := integrationUUID(704)
		var profileID string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, `INSERT INTO jobs
			(id,type,original_id,media_id_snapshot,status,max_attempts)
			VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		mediaBefore, jobBefore := lifecycleRows(t, pool, mediaID, jobID)
		installDeferredLifecycleEventRejection(t, pool)

		_, err = service.Delete(ctx, mediaID)
		assertCommitRolledBackUnavailable(t, err)
		assertLifecycleState(t, pool, mediaID, false, jobID, "queued", 0, 0)
		assertLifecycleRows(t, pool, mediaID, jobID, mediaBefore, jobBefore)
	})

	t.Run("restore", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID := integrationUUID(711)
		insertMedia(t, pool, mediaID, integrationUUID(712))
		if _, err := service.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := service.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}
		mediaBefore, jobBefore := lifecycleRows(t, pool, mediaID, enqueued.Job.ID)
		installDeferredLifecycleEventRejection(t, pool)

		_, err = service.Restore(ctx, mediaID)
		assertCommitRolledBackUnavailable(t, err)
		assertLifecycleState(t, pool, mediaID, true, enqueued.Job.ID, "queued", 1, 1)
		assertLifecycleRows(t, pool, mediaID, enqueued.Job.ID, mediaBefore, jobBefore)
	})
}

func TestLifecycleRepositoryIntegrationEventFailuresRollBackAllState(t *testing.T) {
	t.Run("event UUID generation", func(t *testing.T) {
		ctx := context.Background()
		pool := integrationPool(t)
		mediaID := integrationUUID(721)
		insertMedia(t, pool, mediaID, integrationUUID(722))
		fault := errors.New("event UUID unavailable")
		repository := &PostgresRepository{
			db: pool, fileBaseURL: "https://files.example/files/",
			newID: func() (string, error) { return "", fault },
		}

		if _, err := newService(repository).Delete(ctx, mediaID); !errors.Is(err, fault) {
			t.Fatalf("Delete error = %#v, want injected UUID failure", err)
		}
		assertLifecycleState(t, pool, mediaID, false, "", "", 0, 0)
	})

	t.Run("feed counter overflow", func(t *testing.T) {
		ctx := context.Background()
		pool, service := integrationService(t)
		mediaID := integrationUUID(731)
		insertMedia(t, pool, mediaID, integrationUUID(732))
		if _, err := pool.Exec(ctx, `UPDATE change_feed_state SET last_position=9223372036854775807 WHERE id=1`); err != nil {
			t.Fatal(err)
		}

		if _, err := service.Delete(ctx, mediaID); !IsKind(err, KindInvariant) {
			t.Fatalf("Delete overflow error = %#v, want invariant", err)
		}
		assertLifecycleState(t, pool, mediaID, false, "", "", 9223372036854775807, 0)
	})
}

func TestLifecycleRepositoryIntegrationChangeFeedLockOrderingAndRollbackReuse(t *testing.T) {
	t.Run("waiters commit without position inversion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := integrationPool(t)
		firstMedia, secondMedia := integrationUUID(741), integrationUUID(742)
		insertMedia(t, pool, firstMedia, integrationUUID(743))
		insertMedia(t, pool, secondMedia, integrationUUID(744))

		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT id FROM change_feed_state WHERE id=1 FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		firstConn, firstService, firstPID := connectionService(t, ctx, pool)
		defer firstConn.Release()
		secondConn, secondService, secondPID := connectionService(t, ctx, pool)
		defer secondConn.Release()
		firstResult := make(chan error, 1)
		go func() {
			_, err := firstService.Delete(ctx, firstMedia)
			firstResult <- err
		}()
		awaitLockWait(t, ctx, pool, firstPID)
		secondResult := make(chan error, 1)
		go func() {
			_, err := secondService.Delete(ctx, secondMedia)
			secondResult <- err
		}()
		awaitLockWait(t, ctx, pool, secondPID)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-firstResult; err != nil {
			t.Fatalf("first Delete: %v", err)
		}
		if err := <-secondResult; err != nil {
			t.Fatalf("second Delete: %v", err)
		}

		var firstPosition, secondPosition int64
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT position FROM change_events WHERE media_id=$1),
			(SELECT position FROM change_events WHERE media_id=$2)`, firstMedia, secondMedia).Scan(&firstPosition, &secondPosition); err != nil {
			t.Fatal(err)
		}
		if firstPosition != 1 || secondPosition != 2 {
			t.Fatalf("committed positions = %d/%d, want 1/2", firstPosition, secondPosition)
		}
	})

	t.Run("rolled back allocation is reused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := integrationPool(t)
		mediaID := integrationUUID(751)
		insertMedia(t, pool, mediaID, integrationUUID(752))
		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		connection, service, pid := connectionService(t, ctx, pool)
		defer connection.Release()
		result := make(chan error, 1)
		go func() {
			_, err := service.Delete(ctx, mediaID)
			result <- err
		}()
		awaitLockWait(t, ctx, pool, pid)
		if err := blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		var position, feedPosition int64
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT position FROM change_events WHERE media_id=$1),
			(SELECT last_position FROM change_feed_state WHERE id=1)`, mediaID).Scan(&position, &feedPosition); err != nil {
			t.Fatal(err)
		}
		if position != 1 || feedPosition != 1 {
			t.Fatalf("reused/feed positions = %d/%d, want 1/1", position, feedPosition)
		}
	})
}

func TestLifecycleRepositoryIntegrationPostCommitResponseLossConverges(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		ctx := context.Background()
		pool := integrationPool(t)
		mediaID := integrationUUID(761)
		insertMedia(t, pool, mediaID, integrationUUID(762))
		fault := errors.New("delete commit response lost")
		service := lifecycleServiceWithOnePostCommitFailure(pool, fault)

		_, err := service.Delete(ctx, mediaID)
		assertCommitOutcomeUnknown(t, err, fault)
		retry, err := service.Delete(ctx, mediaID)
		if err != nil || retry.Changed {
			t.Fatalf("Delete retry = %#v, %v", retry, err)
		}
		assertLifecycleState(t, pool, mediaID, true, "", "", 1, 1)
	})

	t.Run("enqueue", func(t *testing.T) {
		ctx := context.Background()
		pool, setupService := integrationService(t)
		mediaID := integrationUUID(771)
		insertMedia(t, pool, mediaID, integrationUUID(772))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		fault := errors.New("enqueue commit response lost")
		service := lifecycleServiceWithOnePostCommitFailure(pool, fault)

		_, err := service.EnqueuePurge(ctx, mediaID)
		assertCommitOutcomeUnknown(t, err, fault)
		retry, err := service.EnqueuePurge(ctx, mediaID)
		if err != nil || retry.Disposition != EnqueueExistingQueued {
			t.Fatalf("EnqueuePurge retry = %#v, %v", retry, err)
		}
		assertLifecycleState(t, pool, mediaID, true, retry.Job.ID, "queued", 1, 1)
		var jobs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge'`, mediaID).Scan(&jobs); err != nil || jobs != 1 {
			t.Fatalf("purge jobs = %d, %v", jobs, err)
		}
	})

	t.Run("purge reclaim", func(t *testing.T) {
		ctx := context.Background()
		pool, setupService := integrationService(t)
		mediaID := integrationUUID(773)
		insertMedia(t, pool, mediaID, integrationUUID(774))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := setupService.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}
		first, err := setupService.StartPurge(ctx, enqueued.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, first.JobID); err != nil {
			t.Fatal(err)
		}
		fault := errors.New("purge reclaim commit response lost")
		service := lifecycleServiceWithOnePostCommitFailure(pool, fault)
		_, err = service.StartNextPurge(ctx, 10)
		assertCommitOutcomeUnknown(t, err, fault)
		retry, err := service.StartNextPurge(ctx, 10)
		if err != nil || retry.JobID != first.JobID || retry.Attempts != 2 || !retry.StartedAt.Equal(first.StartedAt) || retry.Token == first.Token {
			t.Fatalf("StartNextPurge retry = %#v, %v", retry, err)
		}
	})

	t.Run("initial purge start", func(t *testing.T) {
		ctx := context.Background()
		pool, setupService := integrationService(t)
		mediaID := integrationUUID(775)
		insertMedia(t, pool, mediaID, integrationUUID(776))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := setupService.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}
		fault := errors.New("initial purge start response lost")
		faultyService := lifecycleServiceWithOnePostCommitFailure(pool, fault)
		_, err = faultyService.StartPurge(ctx, enqueued.Job.ID)
		assertCommitOutcomeUnknown(t, err, fault)
		var durableToken string
		var firstStartedAt time.Time
		var attempts int
		var status string
		if err := pool.QueryRow(ctx, `SELECT status,attempts,lease_token::text,started_at FROM jobs WHERE id=$1`, enqueued.Job.ID).Scan(
			&status, &attempts, &durableToken, &firstStartedAt); err != nil {
			t.Fatal(err)
		}
		if status != "running" || attempts != 1 || durableToken == "" || firstStartedAt.IsZero() {
			t.Fatalf("durable initial purge status=%s attempts=%d token=%q started=%s", status, attempts, durableToken, firstStartedAt)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, enqueued.Job.ID); err != nil {
			t.Fatal(err)
		}
		repository := setupService.repository.(*PostgresRepository)
		repository.jitter = func(time.Duration) time.Duration { return 0 }
		reclaimed, err := repository.ReclaimExpiredPurges(ctx, 10)
		if err != nil || len(reclaimed) != 1 || reclaimed[0] != enqueued.Job.ID {
			t.Fatalf("reclaim unknown-start purge = %#v, %v", reclaimed, err)
		}
		retry, err := setupService.StartNextPurge(ctx, 10)
		if err != nil || retry.JobID != enqueued.Job.ID || retry.Attempts != 2 || !retry.StartedAt.Equal(firstStartedAt) || retry.Token == durableToken {
			t.Fatalf("unknown-start retry = %#v, %v", retry, err)
		}
	})

	t.Run("purge file progress", func(t *testing.T) {
		ctx := context.Background()
		pool, setupService := integrationService(t)
		mediaID := integrationUUID(777)
		insertMedia(t, pool, mediaID, integrationUUID(778))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
			t.Fatal(err)
		}
		enqueued, err := setupService.EnqueuePurge(ctx, mediaID)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := setupService.StartPurge(ctx, enqueued.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		fault := errors.New("purge progress commit response lost")
		faultyService := lifecycleServiceWithOnePostCommitFailure(pool, fault)
		faultyRepository := faultyService.repository.(*PostgresRepository)
		var relativePath string
		if err := pool.QueryRow(ctx, `SELECT relative_path FROM purge_file_progress WHERE job_id=$1`, lease.JobID).Scan(&relativePath); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		writePurgeFixture(t, root, relativePath, []byte("0123456789"))
		store, err := storage.Open(root, storage.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		_, err = faultyRepository.RunPurgeFileStep(ctx, lease.JobID, lease.Token, StoragePurgeFileAction(store))
		assertCommitOutcomeUnknown(t, err, fault)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relativePath))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge file after lost commit response stat error = %v", err)
		}
		var disposition string
		if err := pool.QueryRow(ctx, `SELECT disposition FROM purge_file_progress WHERE job_id=$1`, lease.JobID).Scan(&disposition); err != nil {
			t.Fatal(err)
		}
		if disposition != "deleted" {
			t.Fatalf("durable purge progress disposition = %s", disposition)
		}
		retry, err := faultyRepository.RunPurgeFileStep(ctx, lease.JobID, lease.Token, func(context.Context, PurgeFile) (PurgeFileDisposition, error) {
			t.Fatal("terminal progress row was invoked after unknown commit")
			return "", nil
		})
		if err != nil || !retry.Done {
			t.Fatalf("purge progress retry = %#v, %v", retry, err)
		}
	})
}

func TestLifecycleRepositoryIntegrationDeleteVersusTransformPublication(t *testing.T) {
	tests := []struct {
		name             string
		publicationFirst bool
		wantRenditions   int
		wantPublishEvent int
	}{
		{name: "publication wins", publicationFirst: true, wantRenditions: 1, wantPublishEvent: 1},
		{name: "delete wins"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := integrationPool(t)
			publicationRepository, candidate := lifecyclePublicationFixture(t, pool, 780+index*20)
			service, err := NewService(pool, "https://files.example/files")
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, candidate.MediaID); err != nil {
				t.Fatal(err)
			}
			var blockerPID int32
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}

			publishResult := make(chan error, 1)
			deleteResult := make(chan error, 1)
			startPublish := func() {
				go func() { _, err := publicationRepository.PublishRendition(ctx, candidate); publishResult <- err }()
			}
			startDelete := func() { go func() { _, err := service.Delete(ctx, candidate.MediaID); deleteResult <- err }() }
			if test.publicationFirst {
				startPublish()
				awaitBlockedMediaLockCount(t, ctx, pool, blockerPID, 1)
				startDelete()
			} else {
				startDelete()
				awaitBlockedMediaLockCount(t, ctx, pool, blockerPID, 1)
				startPublish()
			}
			awaitBlockedMediaLockCount(t, ctx, pool, blockerPID, 2)
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			publicationErr, deleteErr := <-publishResult, <-deleteResult
			if deleteErr != nil {
				t.Fatalf("Delete error = %v", deleteErr)
			}
			if test.publicationFirst && publicationErr != nil {
				t.Fatalf("winning publication error = %v", publicationErr)
			}
			if !test.publicationFirst && !errors.Is(publicationErr, job.ErrConflict) {
				t.Fatalf("losing publication error = %#v, want conflict", publicationErr)
			}

			var deleted bool
			var renditions, current, publishEvents int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT deleted_at IS NOT NULL FROM media WHERE id=$1),
				(SELECT count(*) FROM renditions WHERE media_id=$1),
				(SELECT count(*) FROM renditions WHERE media_id=$1 AND is_current),
				(SELECT count(*) FROM change_events WHERE media_id=$1 AND reason='rendition_current')`, candidate.MediaID).Scan(
				&deleted, &renditions, &current, &publishEvents); err != nil {
				t.Fatal(err)
			}
			if !deleted || renditions != test.wantRenditions || current != test.wantRenditions || publishEvents != test.wantPublishEvent {
				t.Fatalf("final state deleted=%v renditions=%d current=%d publication_events=%d", deleted, renditions, current, publishEvents)
			}
		})
	}
}

func TestLifecycleRepositoryIntegrationLockTimeoutMapsUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := integrationPool(t)
	mediaID := integrationUUID(821)
	insertMedia(t, pool, mediaID, integrationUUID(822))
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	repository := &PostgresRepository{db: connection, fileBaseURL: "https://files.example/files/", newID: newUUIDv4}

	_, err = newService(repository).Delete(ctx, mediaID)
	var semantic *SemanticError
	if !errors.As(err, &semantic) || semantic.Kind() != KindDatabaseUnavailable || semantic.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("lock timeout error = %#v, want unavailable/503", err)
	}
	assertLifecycleState(t, pool, mediaID, false, "", "", 0, 0)
}

func installDeferredLifecycleEventRejection(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_lifecycle_event_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'deferred lifecycle rejection' USING ERRCODE='23514'; END $$;
		CREATE CONSTRAINT TRIGGER reject_lifecycle_event_commit AFTER INSERT ON change_events
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_lifecycle_event_commit()`); err != nil {
		t.Fatal(err)
	}
}

func assertCommitRolledBackUnavailable(t *testing.T, err error) {
	t.Helper()
	var rolledBack *CommitRolledBack
	var unknown *CommitOutcomeUnknown
	var semantic *SemanticError
	if !errors.As(err, &rolledBack) || errors.As(err, &unknown) || !errors.As(err, &semantic) ||
		semantic.Kind() != KindDatabaseUnavailable || semantic.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("commit error = %#v, want CommitRolledBack unavailable/503", err)
	}
}

func assertCommitOutcomeUnknown(t *testing.T, err, cause error) {
	t.Helper()
	var unknown *CommitOutcomeUnknown
	var semantic *SemanticError
	if !errors.As(err, &unknown) || !errors.Is(err, cause) || errors.As(err, &semantic) || unknown.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("commit error = %#v, want preserved CommitOutcomeUnknown/503", err)
	}
}

func assertLifecycleState(t *testing.T, pool *pgxpool.Pool, mediaID string, wantDeleted bool, jobID, wantJobStatus string, wantPosition int64, wantEvents int) {
	t.Helper()
	ctx := context.Background()
	var deleted bool
	var position int64
	var events int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT deleted_at IS NOT NULL FROM media WHERE id=$1),
		(SELECT last_position FROM change_feed_state WHERE id=1),
		(SELECT count(*) FROM change_events)`, mediaID).Scan(&deleted, &position, &events); err != nil {
		t.Fatal(err)
	}
	if deleted != wantDeleted || position != wantPosition || events != wantEvents {
		t.Fatalf("state deleted=%v position=%d events=%d, want %v/%d/%d", deleted, position, events, wantDeleted, wantPosition, wantEvents)
	}
	if jobID != "" {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != wantJobStatus {
			t.Fatalf("job %s status = %s, want %s", jobID, status, wantJobStatus)
		}
	}
}

func lifecycleRows(t *testing.T, pool *pgxpool.Pool, mediaID, jobID string) (string, string) {
	t.Helper()
	var mediaRow, jobRow string
	if err := pool.QueryRow(context.Background(), `SELECT row_to_json(m)::text FROM media m WHERE id=$1`, mediaID).Scan(&mediaRow); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT row_to_json(j)::text FROM jobs j WHERE id=$1`, jobID).Scan(&jobRow); err != nil {
		t.Fatal(err)
	}
	return mediaRow, jobRow
}

func assertLifecycleRows(t *testing.T, pool *pgxpool.Pool, mediaID, jobID, wantMedia, wantJob string) {
	t.Helper()
	mediaRow, jobRow := lifecycleRows(t, pool, mediaID, jobID)
	if mediaRow != wantMedia || jobRow != wantJob {
		t.Fatalf("rolled-back rows changed\nmedia: %s\nwant:  %s\njob:   %s\nwant:  %s", mediaRow, wantMedia, jobRow, wantJob)
	}
}

func lifecycleServiceWithOnePostCommitFailure(pool *pgxpool.Pool, fault error) *Service {
	var failed atomic.Bool
	repository := &PostgresRepository{
		db: pool, fileBaseURL: "https://files.example/files/", newID: newUUIDv4,
		jitter: func(time.Duration) time.Duration { return 0 },
		afterCommit: func(context.Context) error {
			if failed.CompareAndSwap(false, true) {
				return fault
			}
			return nil
		},
	}
	return newService(repository)
}

func lifecyclePublicationFixture(t *testing.T, pool *pgxpool.Pool, seed int) (*job.Repository, job.Rendition) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER;
		UPDATE profiles SET status='active',activated_at=clock_timestamp() WHERE status='draft';
		ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	mediaID, originalID := integrationUUID(seed), integrationUUID(seed+1)
	jobID, targetID := integrationUUID(seed+2), integrationUUID(seed+3)
	leaseToken, renditionID := integrationUUID(seed+4), integrationUUID(seed+5)
	insertMedia(t, pool, mediaID, originalID)
	var profileID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO jobs
		(id,type,original_id,media_id_snapshot,status,max_attempts)
		VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,
		lease_expires_at=clock_timestamp()+interval '1 hour',started_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1`, jobID, leaseToken); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := job.NewRepository(pool, job.Options{FileBaseURL: "https://files.example/files/"})
	if err != nil {
		t.Fatal(err)
	}
	width, height := 1, 1
	return repository, job.Rendition{
		ID: renditionID, JobID: jobID, LeaseToken: leaseToken, TargetID: targetID,
		OriginalID: originalID, MediaID: mediaID, ProfileID: profileID,
		RelativePath: "renditions/" + originalID[:2] + "/" + originalID + "/" + targetID + "/" + renditionID + ".avif",
		MIMEType:     "image/avif", Width: &width, Height: &height, SizeBytes: 1,
		SHA256: strings.Repeat("b", 64), ProcessorAudit: []byte(`{"schema_version":1,"family":"still","result":{"processor":"lifecycle-race"}}`),
	}
}

func awaitBlockedMediaLockCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blockerPID int32, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(ctx, `WITH RECURSIVE blocked(pid) AS (
			SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
			UNION
			SELECT activity.pid FROM pg_stat_activity activity JOIN blocked ON blocked.pid=ANY(pg_blocking_pids(activity.pid))
		)
		SELECT count(*) FROM blocked`, blockerPID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked media-lock operations = %d, want at least %d", count, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
