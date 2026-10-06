//go:build linux

package transformexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestExecutorRealPostgreSQLCommitBoundariesIntegration(t *testing.T) {
	t.Run("deferred commit rejection leaves orphan and rolls back publication", func(t *testing.T) {
		beforeCommit := false
		pool, repository, store, root := executorIntegrationDependencies(t, func(store *storage.Store) jobCheckpoint {
			return func(ctx context.Context, boundary storage.Boundary, key string) error {
				if boundary == storage.BoundaryBeforeDBCommit {
					beforeCommit = true
				}
				return store.DatabaseCheckpoint(ctx, boundary, key)
			}
		})
		if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_executor_publication_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'deferred executor publication rejection' USING ERRCODE='23514'; END $$;
		CREATE CONSTRAINT TRIGGER reject_executor_publication_commit AFTER INSERT ON renditions
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_executor_publication_commit()`); err != nil {
			t.Fatal(err)
		}
		jobID, targets := seedExecutorTransform(t, pool, store, 1)
		lease, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		executor := integrationExecutor(t, repository, store, &fakeProcessors{}, []string{renderOne})
		err = executor.Execute(context.Background(), lease, executionLimits())
		var rolledBack *job.CommitRolledBack
		var unknown *job.CommitOutcomeUnknown
		if !errors.As(err, &rolledBack) || errors.As(err, &unknown) {
			t.Fatalf("Execute() error = %#v, want CommitRolledBack", err)
		}
		if !beforeCommit {
			t.Fatal("repository did not cross BoundaryBeforeDBCommit")
		}
		assertIntegrationRendition(t, root, lease.Original.ID, targets[0], renderOne, "still")
		assertExecutorPublicationState(t, pool, jobID, targets[0], 0, 0, "running", "pending")
	})

	t.Run("post-commit checkpoint leaves committed row and file", func(t *testing.T) {
		fault := errors.New("commit response lost")
		pool, repository, store, root := executorIntegrationDependencies(t, func(store *storage.Store) jobCheckpoint {
			return func(ctx context.Context, boundary storage.Boundary, key string) error {
				if err := store.DatabaseCheckpoint(ctx, boundary, key); err != nil {
					return err
				}
				if boundary == storage.BoundaryAfterDBCommit {
					return fault
				}
				return nil
			}
		})
		jobID, targets := seedExecutorTransform(t, pool, store, 1)
		lease, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		executor := integrationExecutor(t, repository, store, &fakeProcessors{}, []string{renderOne})
		err = executor.Execute(context.Background(), lease, executionLimits())
		var unknown *job.CommitOutcomeUnknown
		if !errors.As(err, &unknown) || !errors.Is(err, fault) {
			t.Fatalf("Execute() error = %#v, want CommitOutcomeUnknown", err)
		}
		assertIntegrationRendition(t, root, lease.Original.ID, targets[0], renderOne, "still")
		assertExecutorPublicationState(t, pool, jobID, targets[0], 1, 1, "succeeded", "succeeded")
	})
}

func TestExecutorPartialRetryOnlyRunsFailedTargetIntegration(t *testing.T) {
	pool, repository, store, root := executorIntegrationDependencies(t, nil)
	jobID, targets := seedExecutorTransform(t, pool, store, 2)
	lease, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	processorFault := errors.New("second target processor failure")
	transformCalls := 0
	processors := &fakeProcessors{stillHook: func(context.Context) error {
		transformCalls++
		if transformCalls == 2 {
			return processorFault
		}
		return nil
	}}
	executor := integrationExecutor(t, repository, store, processors, []string{renderOne, renderTwo, renderThree})
	if err := executor.Execute(context.Background(), lease, executionLimits()); !errors.Is(err, processorFault) {
		t.Fatalf("first Execute() error = %v", err)
	}
	assertIntegrationRendition(t, root, lease.Original.ID, targets[0], renderOne, "still")
	if err := repository.FinishAttempt(context.Background(), jobID, lease.Token, job.FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	retry, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Targets) != 1 || retry.Targets[0].ID != targets[1] {
		t.Fatalf("retry targets = %+v, want only %s", retry.Targets, targets[1])
	}
	if err := executor.Execute(context.Background(), retry, executionLimits()); err != nil {
		t.Fatal(err)
	}
	assertIntegrationRendition(t, root, lease.Original.ID, targets[1], renderThree, "still")
	if transformCalls != 3 {
		t.Fatalf("processor calls = %d, want first target once and second target twice", transformCalls)
	}
	var firstStatus, secondStatus string
	var firstAttempts, secondAttempts, renditions int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT status FROM job_targets WHERE id=$1),(SELECT attempts FROM job_targets WHERE id=$1),
		(SELECT status FROM job_targets WHERE id=$2),(SELECT attempts FROM job_targets WHERE id=$2),
		(SELECT count(*) FROM renditions WHERE job_target_id=ANY($3::uuid[]))`, targets[0], targets[1], targets).Scan(
		&firstStatus, &firstAttempts, &secondStatus, &secondAttempts, &renditions); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "succeeded" || firstAttempts != 1 || secondStatus != "succeeded" || secondAttempts != 2 || renditions != 2 {
		t.Fatalf("target retry state: first=%s/%d second=%s/%d renditions=%d", firstStatus, firstAttempts, secondStatus, secondAttempts, renditions)
	}
	failedFinal := filepath.Join(root, "renditions", lease.Original.ID[:2], lease.Original.ID, targets[1], renderTwo+".avif")
	failedTemp := filepath.Join(filepath.Dir(failedFinal), "."+filepath.Base(failedFinal)+"."+lease.Token+".tmp")
	if _, err := os.Stat(failedFinal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed target final exists: %v", err)
	}
	if _, err := os.Stat(failedTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed target temporary exists: %v", err)
	}
}

type jobCheckpoint func(context.Context, storage.Boundary, string) error

func executorIntegrationDependencies(t *testing.T, checkpoint func(*storage.Store) jobCheckpoint) (*pgxpool.Pool, *job.Repository, *storage.Store, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
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
	schema := "executor_" + strings.ReplaceAll(id, "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop executor integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
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
	if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER;
		UPDATE profiles SET status='active',activated_at=clock_timestamp() WHERE status='draft';
		ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := openExecutorStore(t, root, nil)
	options := job.Options{
		FileBaseURL: "https://files.example.test/files/",
		Jitter:      func(time.Duration) time.Duration { return 0 },
	}
	if checkpoint == nil {
		options.Checkpoint = store.DatabaseCheckpoint
	} else {
		options.Checkpoint = checkpoint(store)
	}
	repository, err := job.NewRepository(pool, options)
	if err != nil {
		t.Fatal(err)
	}
	return pool, repository, store, root
}

func seedExecutorTransform(t *testing.T, pool *pgxpool.Pool, store *storage.Store, targetCount int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	mediaID := integrationUUID(t)
	originalID := integrationUUID(t)
	jobID := integrationUUID(t)
	original, err := storage.ParseOriginalID(originalID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewOriginalKey(original, storage.OriginalJPEG)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := storage.ParseAttemptID(integrationUUID(t))
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := store.BeginOriginal(ctx, key, attempt)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("integration original")
	digest := sha256.Sum256(payload)
	if _, err := temporary.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(ctx, storage.Validation{ExpectedSize: int64(len(payload)), ExpectedSHA256: &digest}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',$5,1,1)`, originalID, mediaID, hex.EncodeToString(digest[:]), key.String(), len(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts)
		VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
		t.Fatal(err)
	}
	profileIDs := []string{profile.StandardV1ID, profile.ThumbnailV1ID}
	targets := make([]string, targetCount)
	for index := range targetCount {
		targets[index] = integrationUUID(t)
		if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targets[index], jobID, profileIDs[index]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID, targets
}

func integrationExecutor(t *testing.T, repository Repository, store *storage.Store, processors *fakeProcessors, ids []string) *Executor {
	t.Helper()
	index := 0
	executor, err := New(repository, StoreAdapter{Store: store}, processors, animationAdapter{processors}, videoAdapter{processors}, Options{
		UUID: func() (string, error) {
			id := ids[index]
			index++
			return id, nil
		},
		AbortTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func assertIntegrationRendition(t *testing.T, root, originalID, targetID, renditionID, want string) {
	t.Helper()
	path := filepath.Join(root, "renditions", originalID[:2], originalID, targetID, renditionID+".avif")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendition %s: %v", renditionID, err)
	}
	if string(contents) != want {
		t.Fatalf("rendition %s bytes = %q, want %q", renditionID, contents, want)
	}
}

func assertExecutorPublicationState(t *testing.T, pool *pgxpool.Pool, jobID, targetID string, wantRenditions, wantEvents int, wantJob, wantTarget string) {
	t.Helper()
	var renditions, events int
	var position int64
	var jobStatus, targetStatus string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM renditions),(SELECT count(*) FROM change_events),
		(SELECT last_position FROM change_feed_state WHERE id=1),
		(SELECT status FROM jobs WHERE id=$1),(SELECT status FROM job_targets WHERE id=$2)`, jobID, targetID).Scan(
		&renditions, &events, &position, &jobStatus, &targetStatus); err != nil {
		t.Fatal(err)
	}
	if renditions != wantRenditions || events != wantEvents || position != int64(wantEvents) || jobStatus != wantJob || targetStatus != wantTarget {
		t.Fatalf("publication state: renditions=%d events=%d position=%d job=%s target=%s", renditions, events, position, jobStatus, targetStatus)
	}
}

func integrationUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
