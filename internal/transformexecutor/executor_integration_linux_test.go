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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformcapability"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
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

func TestExecutorPublicationMaintenanceBarrierIntegration(t *testing.T) {
	t.Run("publication share lock drains before maintenance", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		enteredRename := make(chan struct{})
		releaseRename := make(chan struct{})
		var armed atomic.Bool
		faults := storage.FaultInjectorFunc(func(ctx context.Context, event storage.FaultEvent) error {
			if armed.Load() && event.Boundary == storage.BoundaryRename && event.Phase == storage.Before {
				select {
				case <-enteredRename:
				default:
					close(enteredRename)
				}
				select {
				case <-releaseRename:
					return nil
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			return nil
		})
		pool, repository, store, root := executorIntegrationDependenciesWithFault(t, nil, faults)
		jobID, targets := seedExecutorTransform(t, pool, store, 1)
		lease, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		executor := integrationExecutor(t, repository, store, &fakeProcessors{}, []string{renderOne})
		repair := prepareRenditionRepair(t, pool, lease, targets[0], renderOne, []byte("still"))
		armed.Store(true)
		publication := make(chan error, 1)
		go func() { publication <- executor.Execute(ctx, lease, executionLimits()) }()
		select {
		case <-enteredRename:
		case err := <-publication:
			t.Fatalf("publication returned before filesystem barrier: %v", err)
		case <-ctx.Done():
			t.Fatalf("publication did not reach filesystem callback: %v", ctx.Err())
		}
		maintenanceConnection, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer maintenanceConnection.Release()
		var maintenancePID int32
		if err := maintenanceConnection.QueryRow(ctx, `SELECT pg_catalog.pg_backend_pid()`).Scan(&maintenancePID); err != nil {
			t.Fatal(err)
		}
		maintenance := make(chan error, 1)
		go func() {
			_, err := maintenanceConnection.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='publication drain',owner='test',entered_at=clock_timestamp() WHERE id=1`)
			maintenance <- err
		}()
		awaitExecutorMaintenanceWait(t, ctx, pool, maintenancePID, publication, maintenance)
		close(releaseRename)
		if err := awaitExecutorRaceResult(t, ctx, "publication after filesystem release", publication); err != nil {
			t.Fatalf("publication error = %v", err)
		}
		if err := awaitExecutorRaceResult(t, ctx, "maintenance after publication commit", maintenance); err != nil {
			t.Fatalf("enter maintenance: %v", err)
		}
		assertIntegrationRendition(t, root, lease.Original.ID, targets[0], renderOne, "still")
		assertExecutorPublicationState(t, pool, jobID, targets[0], 1, 1, "succeeded", "succeeded")
		beginRepairAttempt(t, pool, repair)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, applyErr := tx.Exec(context.Background(), `SELECT nmcp_append_repair_event($1,$2,'applying','applying',NULL,NULL,NULL)`, integrationUUID(t), repair.attemptID)
		_ = tx.Rollback(context.Background())
		if applyErr == nil {
			t.Fatal("repair applying ignored the newly committed rendition reference")
		}
	})

	t.Run("maintenance winner rejects before filesystem rename", func(t *testing.T) {
		renameCalled := make(chan struct{}, 1)
		var armed atomic.Bool
		faults := storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
			if armed.Load() && event.Boundary == storage.BoundaryRename && event.Phase == storage.Before {
				renameCalled <- struct{}{}
			}
			return nil
		})
		pool, repository, store, root := executorIntegrationDependenciesWithFault(t, nil, faults)
		jobID, targets := seedExecutorTransform(t, pool, store, 1)
		lease, err := repository.Claim(context.Background(), []job.Type{job.TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		orphanKey := renditionIntegrationKey(t, lease.Original.ID, targets[0], renderOne)
		orphanPayload := []byte("orphan")
		orphanTemp, err := store.BeginRendition(context.Background(), orphanKey, mustIntegrationAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := orphanTemp.Write(orphanPayload); err != nil {
			t.Fatal(err)
		}
		orphanDigest := sha256.Sum256(orphanPayload)
		if _, err := orphanTemp.Publish(context.Background(), storage.Validation{ExpectedSize: int64(len(orphanPayload)), ExpectedSHA256: &orphanDigest}); err != nil {
			t.Fatal(err)
		}
		repair := prepareRenditionRepair(t, pool, lease, targets[0], renderOne, orphanPayload)
		if _, err := pool.Exec(context.Background(), `UPDATE maintenance_state SET mode='maintenance',reason='repair wins',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		armed.Store(true)
		executor := integrationExecutor(t, repository, store, &fakeProcessors{}, []string{renderOne})
		if err := executor.Execute(context.Background(), lease, executionLimits()); err == nil {
			t.Fatal("publication succeeded while maintenance held the barrier")
		}
		select {
		case <-renameCalled:
			t.Fatal("maintenance winner allowed filesystem rename")
		default:
		}
		assertExecutorPublicationState(t, pool, jobID, targets[0], 0, 0, "running", "pending")
		beginRepairAttempt(t, pool, repair)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		appendRepairEvent(t, tx, repair.attemptID, "applying", "applying", nil, nil)
		size := int64(len(orphanPayload))
		shaText := hex.EncodeToString(orphanDigest[:])
		appendRepairEvent(t, tx, repair.attemptID, "revalidated", "matched", &size, &shaText)
		quarantine, err := storage.ParseQuarantineKey(".quarantine/" + repair.quarantineID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.QuarantineRendition(context.Background(), orphanKey, storage.DeleteExpectation{ExpectedSize: size}, quarantine); err != nil {
			t.Fatal(err)
		}
		appendRepairEvent(t, tx, repair.attemptID, "rename", "renamed", nil, nil)
		appendRepairEvent(t, tx, repair.attemptID, "source_directory_fsync", "durable", nil, nil)
		appendRepairEvent(t, tx, repair.attemptID, "destination_directory_fsync", "durable", nil, nil)
		appendRepairEvent(t, tx, repair.attemptID, "completed", "quarantined", &size, &shaText)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "renditions", lease.Original.ID[:2], lease.Original.ID, targets[0], renderOne+".avif")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("repair source still exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".quarantine", repair.quarantineID)); err != nil {
			t.Fatalf("read quarantined orphan: %v", err)
		}
	})
}

type observingTransformWorkerRepository struct {
	*job.TransformClaimer
	maintenanceNoWork chan struct{}
	claims            chan job.Lease
}

func (repository *observingTransformWorkerRepository) Claim(ctx context.Context, types []job.Type) (job.Lease, error) {
	lease, err := repository.TransformClaimer.Claim(ctx, types)
	if errors.Is(err, job.ErrNoWork) {
		select {
		case repository.maintenanceNoWork <- struct{}{}:
		default:
		}
	}
	if err == nil {
		repository.claims <- lease
	}
	return lease, err
}

func TestWorkerTransformMaintenanceRetryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, repository, store, root := executorIntegrationDependencies(t, nil)
	jobID, targets := seedExecutorTransform(t, pool, store, 1)
	// Consume the first two allowed attempts without touching the Target so the
	// Worker below claims the final configured attempt. Maintenance must grant
	// exactly one compensating retry rather than require AdminRetryFailed.
	for attempt := 1; attempt < 3; attempt++ {
		lease, err := repository.Claim(ctx, []job.Type{job.TypeTransform})
		if err != nil {
			t.Fatalf("seed prior attempt %d Claim: %v", attempt, err)
		}
		if err := repository.FinishAttempt(ctx, jobID, lease.Token, job.FailureProcessFailed); err != nil {
			t.Fatalf("seed prior attempt %d FinishAttempt: %v", attempt, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='claim fence',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	before := transformMaintenanceSnapshot(t, pool, jobID)
	if _, err := repository.Claim(ctx, []job.Type{job.TypeTransform}); !errors.Is(err, job.ErrNoWork) {
		t.Fatalf("Claim() during maintenance error = %v, want ErrNoWork", err)
	}
	if after := transformMaintenanceSnapshot(t, pool, jobID); after != before {
		t.Fatalf("maintenance Claim mutated Job/Target/attempt history\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	enteredProcessor := make(chan struct{})
	releaseProcessor := make(chan struct{})
	var processorCalls atomic.Int32
	processors := &fakeProcessors{stillHook: func(processCtx context.Context) error {
		if processorCalls.Add(1) != 1 {
			return nil
		}
		close(enteredProcessor)
		select {
		case <-releaseProcessor:
			return nil
		case <-processCtx.Done():
			return context.Cause(processCtx)
		}
	}}
	executor := integrationExecutor(t, repository, store, processors, []string{renderOne, renderTwo})
	observedRepository := &observingTransformWorkerRepository{
		TransformClaimer: repository, maintenanceNoWork: make(chan struct{}, 8), claims: make(chan job.Lease, 2),
	}
	transformWorker, err := worker.New(observedRepository, map[job.Type]worker.Executor{job.TypeTransform: executor}, worker.Options{
		PollInterval: 10 * time.Millisecond, HeartbeatInterval: time.Second, DatabaseTimeout: 5 * time.Second,
		ShutdownReleaseTimeout: 5 * time.Second, ExecutionLimits: executionLimits(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerResult := make(chan error, 1)
	go func() { workerResult <- transformWorker.Run(workerCtx) }()
	select {
	case <-enteredProcessor:
	case err := <-workerResult:
		t.Fatalf("Worker returned before first processor barrier: %v", err)
	case <-ctx.Done():
		t.Fatalf("Worker did not start transform: %v", ctx.Err())
	}
	var maintenanceLease job.Lease
	select {
	case maintenanceLease = <-observedRepository.claims:
	case <-ctx.Done():
		t.Fatalf("observe final configured lease: %v", ctx.Err())
	}
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='publication winner',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	close(releaseProcessor)
	for count := 0; count < 3; count++ {
		select {
		case <-observedRepository.maintenanceNoWork:
		case err := <-workerResult:
			t.Fatalf("Worker returned while maintenance blocked retry: %v", err)
		case <-ctx.Done():
			t.Fatalf("Worker did not observe maintenance-blocked Claim %d: %v", count+1, ctx.Err())
		}
	}
	assertTransformRetryState(t, pool, jobID, targets[0], "queued", "pending", 3, 4, 1)
	assertTransformAttemptHistory(t, pool, jobID, 3, 3, 0)
	assertTransformMaintenanceError(t, pool, jobID)
	assertTransformAttemptSequence(t, pool, maintenanceLease.Token, []string{"claimed", "released"}, "queued")
	paused := transformMaintenanceSnapshot(t, pool, jobID)
	if err := repository.FinishAttempt(ctx, jobID, integrationUUID(t), job.FailureMaintenancePaused); !errors.Is(err, job.ErrLeaseLost) {
		t.Fatalf("stale maintenance pause error = %v, want ErrLeaseLost", err)
	}
	if after := transformMaintenanceSnapshot(t, pool, jobID); after != paused {
		t.Fatalf("stale maintenance pause mutated state\nbefore: %s\nafter:  %s", paused, after)
	}
	if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	awaitTransformRetrySuccess(t, ctx, pool, jobID, workerResult)
	var resumedLease job.Lease
	select {
	case resumedLease = <-observedRepository.claims:
	case <-ctx.Done():
		t.Fatalf("observe resumed lease: %v", ctx.Err())
	}
	stopWorker()
	if err := awaitExecutorRaceResult(t, ctx, "Worker shutdown after maintenance retry", workerResult); err != nil {
		t.Fatalf("Worker error = %v", err)
	}
	assertTransformRetryState(t, pool, jobID, targets[0], "succeeded", "succeeded", 4, 4, 2)
	assertTransformAttemptHistory(t, pool, jobID, 4, 3, 1)
	assertTransformAttemptSequence(t, pool, resumedLease.Token, []string{"claimed", "published"}, "succeeded")
	var originalID string
	if err := pool.QueryRow(ctx, `SELECT original_id::text FROM jobs WHERE id=$1`, jobID).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	assertIntegrationRendition(t, root, originalID, targets[0], renderTwo, "still")
}

func assertTransformMaintenanceError(t *testing.T, pool *pgxpool.Pool, jobID string) {
	t.Helper()
	var code, message string
	if err := pool.QueryRow(context.Background(), `SELECT error_code,error_message FROM jobs WHERE id=$1`, jobID).Scan(&code, &message); err != nil {
		t.Fatal(err)
	}
	if code != string(job.FailureMaintenancePaused) || message != "job publication paused by maintenance" {
		t.Fatalf("maintenance error = %q/%q", code, message)
	}
}

func assertTransformAttemptSequence(t *testing.T, pool *pgxpool.Pool, attemptID string, wantEvents []string, wantStatus string) {
	t.Helper()
	var events []string
	var terminalStatus string
	if err := pool.QueryRow(context.Background(), `SELECT array_agg(event_type ORDER BY sequence),
		(array_agg(job_status ORDER BY sequence DESC))[1]
		FROM storage_attempt_events WHERE attempt_id=$1`, attemptID).Scan(&events, &terminalStatus); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") || terminalStatus != wantStatus {
		t.Fatalf("attempt %s events/status=%v/%s, want %v/%s", attemptID, events, terminalStatus, wantEvents, wantStatus)
	}
}

func assertTransformAttemptHistory(t *testing.T, pool *pgxpool.Pool, jobID string, wantAttempts, wantReleased, wantPublished int) {
	t.Helper()
	var attempts, released, published int
	err := pool.QueryRow(context.Background(), `SELECT count(*),
		count(*) FILTER (WHERE latest.event_type='released'),
		count(*) FILTER (WHERE latest.event_type='published')
		FROM storage_attempts AS attempt
		JOIN LATERAL (SELECT event_type FROM storage_attempt_events
			WHERE attempt_id=attempt.id ORDER BY sequence DESC LIMIT 1) AS latest ON true
		WHERE attempt.job_id=$1`, jobID).Scan(&attempts, &released, &published)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != wantAttempts || released != wantReleased || published != wantPublished {
		t.Fatalf("storage attempt history total/released/published=%d/%d/%d, want %d/%d/%d",
			attempts, released, published, wantAttempts, wantReleased, wantPublished)
	}
}

func transformMaintenanceSnapshot(t *testing.T, pool *pgxpool.Pool, jobID string) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
		'job',to_jsonb(j),
		'targets',(SELECT COALESCE(jsonb_agg(to_jsonb(jt) ORDER BY jt.id),'[]'::jsonb) FROM job_targets AS jt WHERE jt.job_id=j.id),
		'attempts',(SELECT COALESCE(jsonb_agg(to_jsonb(sa) ORDER BY sa.id),'[]'::jsonb) FROM storage_attempts AS sa WHERE sa.job_id=j.id),
		'attempt_events',(SELECT COALESCE(jsonb_agg(to_jsonb(se) ORDER BY se.id),'[]'::jsonb) FROM storage_attempt_events AS se JOIN storage_attempts AS sa ON sa.id=se.attempt_id WHERE sa.job_id=j.id)
	)::text FROM jobs AS j WHERE j.id=$1`, jobID).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertTransformRetryState(t *testing.T, pool *pgxpool.Pool, jobID, targetID, wantJob, wantTarget string, wantJobAttempts, wantMaxAttempts, wantTargetAttempts int) {
	t.Helper()
	var jobStatus, targetStatus string
	var jobAttempts, maxAttempts, targetAttempts int
	if err := pool.QueryRow(context.Background(), `SELECT j.status,j.attempts,j.max_attempts,jt.status,jt.attempts
		FROM jobs AS j JOIN job_targets AS jt ON jt.job_id=j.id WHERE j.id=$1 AND jt.id=$2`, jobID, targetID).Scan(
		&jobStatus, &jobAttempts, &maxAttempts, &targetStatus, &targetAttempts); err != nil {
		t.Fatal(err)
	}
	if jobStatus != wantJob || targetStatus != wantTarget || jobAttempts != wantJobAttempts || maxAttempts != wantMaxAttempts || targetAttempts != wantTargetAttempts {
		t.Fatalf("retry state job=%s/%d/%d target=%s/%d, want job=%s/%d/%d target=%s/%d",
			jobStatus, jobAttempts, maxAttempts, targetStatus, targetAttempts, wantJob, wantJobAttempts, wantMaxAttempts, wantTarget, wantTargetAttempts)
	}
}

func awaitTransformRetrySuccess(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string, workerResult <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			return
		}
		if status == "failed" {
			t.Fatal("maintenance retry exhausted transform permanently")
		}
		select {
		case err := <-workerResult:
			t.Fatalf("Worker returned before maintenance retry succeeded: %v", err)
		case <-ctx.Done():
			t.Fatalf("maintenance retry did not succeed: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func awaitExecutorMaintenanceWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, maintenancePID int32, publication, maintenance <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT COALESCE(
			wait_event_type='Lock'
			AND cardinality(pg_catalog.pg_blocking_pids(pid))>0
			AND position('UPDATE maintenance_state SET mode=''maintenance''' in query)>0,
			false)
			FROM pg_catalog.pg_stat_activity WHERE pid=$1`, maintenancePID).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe maintenance publication waiter: %v", err)
		}
		if waiting {
			return
		}
		select {
		case err := <-publication:
			t.Fatalf("publication returned before maintenance lock wait: %v", err)
		case err := <-maintenance:
			t.Fatalf("maintenance returned before publication released: %v", err)
		case <-ctx.Done():
			t.Fatalf("maintenance backend %d did not wait for publication fence: %v", maintenancePID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func awaitExecutorRaceResult(t *testing.T, ctx context.Context, operation string, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatalf("%s: %v", operation, ctx.Err())
		return ctx.Err()
	}
}

type integrationRepair struct {
	runID, itemID, attemptID, quarantineID string
}

func prepareRenditionRepair(t *testing.T, pool *pgxpool.Pool, lease job.Lease, targetID, renditionID string, payload []byte) integrationRepair {
	t.Helper()
	repair := integrationRepair{runID: integrationUUID(t), itemID: integrationUUID(t), attemptID: integrationUUID(t), quarantineID: integrationUUID(t)}
	reportID, findingID := integrationUUID(t), integrationUUID(t)
	path := "renditions/" + lease.Original.ID[:2] + "/" + lease.Original.ID + "/" + targetID + "/" + renditionID + ".avif"
	digest := sha256.Sum256(payload)
	shaText := hex.EncodeToString(digest[:])
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_check_reports(
		id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
		db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at)
		SELECT $1,1,1,'all',version,name,checksum,statement_timestamp()-interval '5 seconds',statement_timestamp()-interval '4 seconds',
		statement_timestamp()-interval '3 seconds',statement_timestamp()-interval '2 seconds',statement_timestamp()-interval '48 hours 4 seconds'
		FROM schema_migrations ORDER BY version DESC LIMIT 1`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_check_source_results(report_id,source,result)
		SELECT $1,source,'complete' FROM unnest(ARRAY['database_references','attempt_owners','storage_scan']) AS source`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_check_findings(
		id,report_id,ordinal,kind,reason_code,actionability,subject_type,subject_id,media_id,job_id,job_target_id,
		relative_key,expected_state,observed_type,observed_size_bytes,observed_sha256,observed_mtime,observed_ctime,observed_at)
		VALUES($1,$2,1,'final_orphan','publication_race','repairable','rendition',$3,$4,$5,$6,$7,'unreferenced','regular',$8,$9,
		clock_timestamp()-interval '3 seconds',clock_timestamp()-interval '3 seconds',clock_timestamp()-interval '1 second')`,
		findingID, reportID, renditionID, lease.MediaID, lease.ID, targetID, path, len(payload), shaText); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_check_report_seals(report_id,finding_count,fs_scan_ended_at)
		VALUES($1,1,clock_timestamp()-interval '1 second')`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT nmcp_begin_repair_run($1,$2,$3,'integration-test')`, repair.runID, reportID, integrationUUID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT nmcp_prepare_repair_manifest_item($1,$2,$3,$4)`, repair.itemID, repair.runID, findingID, repair.quarantineID); err != nil {
		t.Fatal(err)
	}
	return repair
}

func beginRepairAttempt(t *testing.T, pool *pgxpool.Pool, repair integrationRepair) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `SELECT nmcp_begin_repair_attempt($1,$2,$3)`, repair.attemptID, repair.itemID, integrationUUID(t)); err != nil {
		t.Fatal(err)
	}
}

func appendRepairEvent(t *testing.T, tx pgx.Tx, attemptID, eventType, outcome string, size *int64, sha *string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `SELECT nmcp_append_repair_event($1,$2,$3,$4,NULL,$5,$6)`, integrationUUID(t), attemptID, eventType, outcome, size, sha); err != nil {
		t.Fatal(err)
	}
}

func renditionIntegrationKey(t *testing.T, originalID, targetID, renditionID string) storage.RenditionKey {
	t.Helper()
	original, err := storage.ParseOriginalID(originalID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := storage.ParseJobTargetID(targetID)
	if err != nil {
		t.Fatal(err)
	}
	rendition, err := storage.ParseRenditionID(renditionID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewRenditionKey(original, target, rendition, storage.RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustIntegrationAttempt(t *testing.T) storage.AttemptID {
	t.Helper()
	attempt, err := storage.ParseAttemptID(integrationUUID(t))
	if err != nil {
		t.Fatal(err)
	}
	return attempt
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

func executorIntegrationDependencies(t *testing.T, checkpoint func(*storage.Store) jobCheckpoint) (*pgxpool.Pool, *job.TransformClaimer, *storage.Store, string) {
	return executorIntegrationDependenciesWithFault(t, checkpoint, nil)
}

func executorIntegrationDependenciesWithFault(t *testing.T, checkpoint func(*storage.Store) jobCheckpoint, faults storage.FaultInjector) (*pgxpool.Pool, *job.TransformClaimer, *storage.Store, string) {
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
	store := openExecutorStore(t, root, faults)
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
	definitions, err := repository.ClaimableTransformProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	still, animation, video := executorIntegrationCapabilities()
	envelope, err := transformcapability.ValidateEnvelope(definitions, still, animation, video)
	if err != nil {
		t.Fatal(err)
	}
	claimer, err := repository.BindTransformClaims(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return pool, claimer, store, root
}

func executorIntegrationCapabilities() (stillprocessor.Capabilities, animationprocessor.Capabilities, videoprocessor.Capabilities) {
	icc := strings.Repeat("a", 64)
	still := stillprocessor.Capabilities{
		ProtocolVersion: stillprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"libvips": "test"},
		DecoderMIMETypes: []string{"image/bmp", "image/dng", "image/heic", "image/heif", "image/jpeg", "image/png", "image/webp", "image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef", "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw"},
		AVIFEncoder:      "aom", ICCSHA256: icc, Threads: stillprocessor.RequiredThreads,
	}
	animation := animationprocessor.Capabilities{
		ProtocolVersion: animationprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"libwebp": "test"},
		DecoderMIMETypes: []string{"image/gif", "image/webp"}, Encoders: []string{"animated-webp", "avif"},
		ICCSHA256: icc, Threads: animationprocessor.RequiredThreads, BuildManifest: animationprocessor.BuildManifest,
	}
	video := videoprocessor.Capabilities{
		ProtocolVersion: videoprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"ffmpeg": "test"},
		DecoderMIMETypes: []string{"video/mp4", "video/quicktime"}, OutputKinds: []string{"first-frame-avif", "mp4-av1"},
		VideoEncoder: "libsvtav1", AudioEncoder: "aac-lc", VideoMuxer: "mp4", ToneMap: "zscale+hable",
		ICCSHA256: icc, Threads: videoprocessor.RequiredThreads, BuildManifest: videoprocessor.BuildManifest,
	}
	return still, animation, video
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
