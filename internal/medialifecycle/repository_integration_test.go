package medialifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
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

func TestLifecycleRepositoryIntegrationDuePurgeScanRechecksCurrentDeadline(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(23)
	insertMedia(t, pool, mediaID, integrationUUID(123))
	if _, err := pool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	dueThrough, err := service.DatabaseNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := service.ScanDuePurges(ctx, dueThrough, nil, 10)
	if err != nil || len(candidates) != 1 || candidates[0].MediaID != mediaID {
		t.Fatalf("due candidates = %#v, %v", candidates, err)
	}

	if _, err := service.Restore(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=30 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueDuePurge(ctx, candidates[0].MediaID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("stale automatic enqueue error = %#v", err)
	}
	var purgeJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge'`, mediaID).Scan(&purgeJobs); err != nil {
		t.Fatal(err)
	}
	if purgeJobs != 0 {
		t.Fatalf("purge job count after stale scan = %d, want 0", purgeJobs)
	}
}

func TestLifecycleRepositoryIntegrationExplicitAndAutomaticEnqueueConverge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, service := integrationService(t)
	mediaID := integrationUUID(24)
	insertMedia(t, pool, mediaID, integrationUUID(124))
	if _, err := pool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result EnqueueResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	go func() {
		<-start
		result, err := service.EnqueuePurge(ctx, mediaID)
		outcomes <- outcome{result: result, err: err}
	}()
	go func() {
		<-start
		result, err := service.EnqueueDuePurge(ctx, mediaID)
		outcomes <- outcome{result: result, err: err}
	}()
	close(start)
	first, second := <-outcomes, <-outcomes
	if first.err != nil || second.err != nil || first.result.Job.ID != second.result.Job.ID {
		t.Fatalf("enqueue outcomes = %#v / %#v", first, second)
	}
	created := 0
	for _, result := range []EnqueueResult{first.result, second.result} {
		if result.Disposition == EnqueueCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created dispositions = %d, want 1", created)
	}
	var purgeJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge'`, mediaID).Scan(&purgeJobs); err != nil {
		t.Fatal(err)
	}
	if purgeJobs != 1 {
		t.Fatalf("purge job count = %d, want 1", purgeJobs)
	}
}

func TestLifecycleRepositoryIntegrationDuePurgeScanBoundariesAndKeyset(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	ids := []string{
		integrationUUID(25),
		integrationUUID(26),
		integrationUUID(27),
		integrationUUID(28),
		integrationUUID(29),
		integrationUUID(30),
	}
	for index, mediaID := range ids {
		insertMedia(t, pool, mediaID, integrationUUID(125+index))
	}
	boundary := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=$1,purge_after=NULL WHERE id=$2`, boundary, ids[1]); err != nil {
		t.Fatal(err)
	}
	for _, mediaID := range ids[2:5] {
		if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=$1,purge_after=$1 WHERE id=$2`, boundary, mediaID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=$1,purge_after=$2 WHERE id=$3`, boundary, boundary.Add(time.Nanosecond), ids[5]); err != nil {
		t.Fatal(err)
	}

	first, err := service.ScanDuePurges(ctx, boundary, nil, 2)
	if err != nil || len(first) != 2 || first[0].MediaID != ids[2] || first[1].MediaID != ids[3] {
		t.Fatalf("first due page = %#v, %v", first, err)
	}
	cursor := &DuePurgeCursor{PurgeAfter: first[1].PurgeAfter, MediaID: first[1].MediaID}
	second, err := service.ScanDuePurges(ctx, boundary, cursor, 2)
	if err != nil || len(second) != 1 || second[0].MediaID != ids[4] {
		t.Fatalf("second due page = %#v, %v", second, err)
	}
}

func TestLifecycleRepositoryIntegrationProjectionParityWithReadAPI(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID, originalID := integrationUUID(21), integrationUUID(121)
	transformJobID, targetID := integrationUUID(221), integrationUUID(321)
	renditionID, leaseID := integrationUUID(421), integrationUUID(521)
	insertMedia(t, pool, mediaID, originalID)
	var profileID, profileKey string
	if err := pool.QueryRow(ctx, `SELECT id::text,key FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID, &profileKey); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	renderPath := "renditions/" + originalID[:2] + "/" + originalID + "/" + targetID + "/" + renditionID + ".avif"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at) VALUES ($1,'transform',$2,$3,'queued',3,$4,$4,$4)`, []any{transformJobID, originalID, mediaID, now}},
		{`INSERT INTO job_targets (id,job_id,profile_id,status,updated_at) VALUES ($1,$2,$3,'pending',$4)`, []any{targetID, transformJobID, profileID, now}},
		{`UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=$3,started_at=$4,updated_at=$4 WHERE id=$1`, []any{transformJobID, leaseID, now.Add(time.Minute), now}},
		{`UPDATE job_targets SET status='succeeded',attempts=1,updated_at=$2 WHERE id=$1`, []any{targetID, now}},
		{`INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,width,height,sha256,created_at,processor_audit) VALUES ($1,$2,$3,$4,true,$5,'image/avif',21,1,1,$6,$7,'{"fixture":"lifecycle-parity"}')`, []any{renditionID, mediaID, targetID, profileKey, renderPath, strings.Repeat("a", 64), now}},
		{`UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=$2,updated_at=$2 WHERE id=$1`, []any{transformJobID, now}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueuePurge(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	restored, err := service.Restore(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}

	codec, err := readapi.NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	readService, err := readapi.NewService(pool, codec, "https://files.example/files")
	if err != nil {
		t.Fatal(err)
	}
	readDetail, err := readService.GetMedia(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Media.CurrentRenditions) != 1 || len(restored.Media.Jobs) != 2 {
		t.Fatalf("representative lifecycle projection = %#v", restored.Media)
	}
	assertSemanticJSONEqual(t, restored.Media, readDetail)
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
	missingLease, err := service.StartPurge(ctx, missing.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifestTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manifestTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, missingLease.Token); err == nil {
		_, err = manifestTx.Exec(ctx, `INSERT INTO purge_file_progress
			(job_id,media_id_snapshot,object_kind,object_id,relative_path,size_bytes)
			SELECT $1::nmcp_uuid_v4,$2::nmcp_uuid_v4,'original',id,relative_path,size_bytes FROM originals WHERE media_id=$2::nmcp_uuid_v4`, missing.Job.ID, missingMediaID)
	}
	if err != nil {
		_ = manifestTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := manifestTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	progressTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = progressTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, missingLease.Token); err == nil {
		_, err = progressTx.Exec(ctx, `UPDATE purge_file_progress SET disposition='missing' WHERE job_id=$1`, missing.Job.ID)
	}
	if err != nil {
		_ = progressTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := progressTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	finalTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finalTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_job_id',$1,true)`, missing.Job.ID); err == nil {
		_, err = finalTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, missingLease.Token)
	}
	if err == nil {
		_, err = finalTx.Exec(ctx, `DELETE FROM media WHERE id=$1`, missingMediaID)
	}
	if err == nil {
		_, err = finalTx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, missing.Job.ID)
	}
	if err != nil {
		_ = finalTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := finalTx.Commit(ctx); err != nil {
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

func TestLifecycleRepositoryIntegrationStartPurgeCancelledGuard(t *testing.T) {
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
	if _, err := service.Restore(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPurge(ctx, enqueued.Job.ID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("cancelled StartPurge error = %#v", err)
	}
}

func TestLifecycleRepositoryIntegrationOrderedLifecycleRaces(t *testing.T) {
	pool, setupService := integrationService(t)
	tests := []struct {
		name               string
		mediaValue         int
		initiallyDeleted   bool
		first              lifecycleOperation
		second             lifecycleOperation
		firstConflict      bool
		secondConflict     bool
		firstDeleteChange  *bool
		secondDeleteChange *bool
		wantDeleted        bool
		wantJobs           int
		wantJobStatus      string
		wantDeleteEvents   int
		wantRestoreEvents  int
	}{
		{name: "delete replay precedes restore", mediaValue: 50, initiallyDeleted: true, first: operationDelete, second: operationRestore, firstDeleteChange: boolPointer(false), wantDeleteEvents: 1, wantRestoreEvents: 1},
		{name: "restore precedes fresh delete", mediaValue: 51, initiallyDeleted: true, first: operationRestore, second: operationDelete, secondDeleteChange: boolPointer(true), wantDeleted: true, wantDeleteEvents: 2, wantRestoreEvents: 1},
		{name: "delete wins over enqueue", mediaValue: 52, first: operationDelete, second: operationEnqueue, firstDeleteChange: boolPointer(true), wantDeleted: true, wantJobs: 1, wantJobStatus: "queued", wantDeleteEvents: 1},
		{name: "enqueue loses before delete", mediaValue: 53, first: operationEnqueue, second: operationDelete, firstConflict: true, secondDeleteChange: boolPointer(true), wantDeleted: true, wantDeleteEvents: 1},
		{name: "restore wins over enqueue", mediaValue: 54, initiallyDeleted: true, first: operationRestore, second: operationEnqueue, secondConflict: true, wantDeleteEvents: 1, wantRestoreEvents: 1},
		{name: "enqueue wins over restore", mediaValue: 55, initiallyDeleted: true, first: operationEnqueue, second: operationRestore, wantJobs: 1, wantJobStatus: "cancelled", wantDeleteEvents: 1, wantRestoreEvents: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mediaID := integrationUUID(test.mediaValue)
			insertMedia(t, pool, mediaID, integrationUUID(test.mediaValue+100))
			if test.initiallyDeleted {
				result, err := setupService.Delete(ctx, mediaID)
				if err != nil || !result.Changed {
					t.Fatalf("setup Delete() = %#v, %v", result, err)
				}
			}

			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
				t.Fatal(err)
			}

			firstConn, firstService, firstPID := connectionService(t, ctx, pool)
			defer firstConn.Release()
			secondConn, secondService, secondPID := connectionService(t, ctx, pool)
			defer secondConn.Release()
			firstResult := runLifecycleOperation(ctx, firstService, mediaID, test.first)
			awaitLockWait(t, ctx, pool, firstPID)
			secondResult := runLifecycleOperation(ctx, secondService, mediaID, test.second)
			awaitLockWait(t, ctx, pool, secondPID)
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			assertLifecycleOperationResult(t, test.first, <-firstResult, mediaID, test.firstConflict, test.firstDeleteChange)
			assertLifecycleOperationResult(t, test.second, <-secondResult, mediaID, test.secondConflict, test.secondDeleteChange)
			assertLifecycleFinalState(t, pool, mediaID, test.wantDeleted, test.wantJobs, test.wantJobStatus, test.wantDeleteEvents, test.wantRestoreEvents)
		})
	}

	t.Run("ordered duplicate delete emits one event", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mediaID := integrationUUID(56)
		insertMedia(t, pool, mediaID, integrationUUID(156))

		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
			t.Fatal(err)
		}
		firstConn, firstService, firstPID := connectionService(t, ctx, pool)
		defer firstConn.Release()
		secondConn, secondService, secondPID := connectionService(t, ctx, pool)
		defer secondConn.Release()
		firstResult := runLifecycleOperation(ctx, firstService, mediaID, operationDelete)
		awaitLockWait(t, ctx, pool, firstPID)
		secondResult := runLifecycleOperation(ctx, secondService, mediaID, operationDelete)
		awaitLockWait(t, ctx, pool, secondPID)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		first, second := <-firstResult, <-secondResult
		if first.err != nil || !first.deleted.Changed || second.err != nil || second.deleted.Changed ||
			!sameTime(first.deleted.Media.DeletedAt, second.deleted.Media.DeletedAt) {
			t.Fatalf("ordered Delete() results = %#v / %#v", first, second)
		}
		assertLifecycleFinalState(t, pool, mediaID, true, 0, "", 1, 0)
	})

	t.Run("ordered duplicate enqueue returns one job", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mediaID := integrationUUID(57)
		insertMedia(t, pool, mediaID, integrationUUID(157))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
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
		firstConn, firstService, firstPID := connectionService(t, ctx, pool)
		defer firstConn.Release()
		secondConn, secondService, secondPID := connectionService(t, ctx, pool)
		defer secondConn.Release()
		firstResult := runLifecycleOperation(ctx, firstService, mediaID, operationEnqueue)
		awaitLockWait(t, ctx, pool, firstPID)
		secondResult := runLifecycleOperation(ctx, secondService, mediaID, operationEnqueue)
		awaitLockWait(t, ctx, pool, secondPID)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		first, second := <-firstResult, <-secondResult
		if first.err != nil || first.enqueued.Disposition != EnqueueCreated || second.err != nil ||
			second.enqueued.Disposition != EnqueueExistingQueued || first.enqueued.Job.ID != second.enqueued.Job.ID {
			t.Fatalf("ordered EnqueuePurge() results = %#v / %#v", first, second)
		}
		assertLifecycleFinalState(t, pool, mediaID, true, 1, "queued", 1, 0)
	})

	t.Run("direct purge insert waits for restore and rejects active media", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mediaID := integrationUUID(58)
		insertMedia(t, pool, mediaID, integrationUUID(158))
		if _, err := setupService.Delete(ctx, mediaID); err != nil {
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
			_, err := inserter.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, integrationUUID(958), mediaID)
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

func TestLifecycleRepositoryIntegrationMaintenanceRejectsBeforeMutation(t *testing.T) {
	tests := []struct {
		name       string
		mediaValue int
		setup      lifecycleOperation
		operation  lifecycleOperation
	}{
		{name: "delete", mediaValue: 60, operation: operationDelete},
		{name: "restore", mediaValue: 61, setup: operationEnqueue, operation: operationRestore},
		{name: "enqueue purge", mediaValue: 62, setup: operationDelete, operation: operationEnqueue},
		{name: "start purge", mediaValue: 63, setup: operationEnqueue, operation: operationStart},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool, service := integrationService(t)
			mediaID := integrationUUID(test.mediaValue)
			insertMedia(t, pool, mediaID, integrationUUID(test.mediaValue+100))

			var jobID string
			switch test.setup {
			case operationDelete:
				if _, err := service.Delete(ctx, mediaID); err != nil {
					t.Fatal(err)
				}
			case operationEnqueue:
				if _, err := service.Delete(ctx, mediaID); err != nil {
					t.Fatal(err)
				}
				enqueued, err := service.EnqueuePurge(ctx, mediaID)
				if err != nil {
					t.Fatal(err)
				}
				jobID = enqueued.Job.ID
			}

			before := lifecycleSnapshot(t, pool, mediaID)
			if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='test',entered_at=statement_timestamp() WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			result := <-runLifecycleOperation(ctx, service, operationID(test.operation, mediaID, jobID), test.operation)
			assertUnavailableError(t, result.err)
			after := lifecycleSnapshot(t, pool, mediaID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("state mutated in maintenance mode\nbefore: %#v\nafter:  %#v", before, after)
			}
		})
	}
}

func TestLifecycleRepositoryIntegrationEventRollback(t *testing.T) {
	ctx := context.Background()
	pool, service := integrationService(t)
	mediaID := integrationUUID(64)
	insertMedia(t, pool, mediaID, integrationUUID(164))
	if _, err := pool.Exec(ctx, `UPDATE change_feed_state SET last_position=9223372036854775807 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(ctx, mediaID); !IsKind(err, KindInvariant) {
		t.Fatalf("feed overflow error = %#v", err)
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1`, mediaID).Scan(&deletedAt); err != nil || deletedAt != nil {
		t.Fatalf("overflow rollback = %v, %v", deletedAt, err)
	}
}

type lifecycleOperation string

const (
	operationDelete  lifecycleOperation = "delete"
	operationRestore lifecycleOperation = "restore"
	operationEnqueue lifecycleOperation = "enqueue"
	operationStart   lifecycleOperation = "start"
)

type lifecycleOperationResult struct {
	deleted  DeleteResult
	restored RestoreResult
	enqueued EnqueueResult
	started  PurgeLease
	err      error
}

func runLifecycleOperation(ctx context.Context, service *Service, id string, operation lifecycleOperation) <-chan lifecycleOperationResult {
	result := make(chan lifecycleOperationResult, 1)
	go func() {
		var value lifecycleOperationResult
		switch operation {
		case operationDelete:
			value.deleted, value.err = service.Delete(ctx, id)
		case operationRestore:
			value.restored, value.err = service.Restore(ctx, id)
		case operationEnqueue:
			value.enqueued, value.err = service.EnqueuePurge(ctx, id)
		case operationStart:
			value.started, value.err = service.StartPurge(ctx, id)
		default:
			value.err = fmt.Errorf("unknown lifecycle operation %q", operation)
		}
		result <- value
	}()
	return result
}

func assertLifecycleOperationResult(t *testing.T, operation lifecycleOperation, result lifecycleOperationResult, mediaID string, wantConflict bool, wantDeleteChange *bool) {
	t.Helper()
	if wantConflict {
		assertMediaNotDeletedError(t, result.err)
		return
	}
	if result.err != nil {
		t.Fatalf("%s error = %#v", operation, result.err)
	}
	switch operation {
	case operationDelete:
		if wantDeleteChange == nil || result.deleted.Changed != *wantDeleteChange || result.deleted.Media.ID != mediaID || result.deleted.Media.DeletedAt == nil {
			t.Fatalf("Delete() = %#v", result.deleted)
		}
	case operationRestore:
		if result.restored.Media.ID != mediaID || result.restored.Media.DeletedAt != nil || result.restored.Media.PurgeAfter != nil {
			t.Fatalf("Restore() = %#v", result.restored)
		}
	case operationEnqueue:
		job := result.enqueued.Job
		if result.enqueued.Disposition != EnqueueCreated || !readapi.IsUUIDv4(job.ID) || job.MediaID != mediaID ||
			job.Type != readapi.JobPurge || job.Status != readapi.JobQueued || job.Attempts != 0 || job.MaxAttempts != InitialPurgeMaxAttempts {
			t.Fatalf("EnqueuePurge() = %#v", result.enqueued)
		}
	}
}

func assertLifecycleFinalState(t *testing.T, pool *pgxpool.Pool, mediaID string, wantDeleted bool, wantJobs int, wantJobStatus string, wantDeleteEvents, wantRestoreEvents int) {
	t.Helper()
	var deleted bool
	var jobs, queued, cancelled, deleteEvents, restoreEvents int
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT deleted_at IS NOT NULL FROM media WHERE id=$1),
		(SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge'),
		(SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge' AND status='queued'),
		(SELECT count(*) FROM jobs WHERE media_id_snapshot=$1 AND type='purge' AND status='cancelled'),
		(SELECT count(*) FROM change_events WHERE media_id=$1 AND reason='logical_delete'),
		(SELECT count(*) FROM change_events WHERE media_id=$1 AND reason='restore')`, mediaID).Scan(
		&deleted, &jobs, &queued, &cancelled, &deleteEvents, &restoreEvents,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantQueued, wantCancelled := 0, 0
	switch wantJobStatus {
	case "queued":
		wantQueued = wantJobs
	case "cancelled":
		wantCancelled = wantJobs
	case "":
	default:
		t.Fatalf("unsupported expected job status %q", wantJobStatus)
	}
	if deleted != wantDeleted || jobs != wantJobs || queued != wantQueued || cancelled != wantCancelled ||
		deleteEvents != wantDeleteEvents || restoreEvents != wantRestoreEvents {
		t.Fatalf("final state deleted=%v jobs=%d queued=%d cancelled=%d events=%d/%d, want %v/%d/%d/%d/%d/%d",
			deleted, jobs, queued, cancelled, deleteEvents, restoreEvents,
			wantDeleted, wantJobs, wantQueued, wantCancelled, wantDeleteEvents, wantRestoreEvents)
	}
}

func assertMediaNotDeletedError(t *testing.T, err error) {
	t.Helper()
	var semantic *SemanticError
	if !errors.As(err, &semantic) || semantic.Kind() != KindConflict || semantic.HTTPStatus() != 409 ||
		semantic.Code() != CodeMediaNotDeleted || semantic.Message() != "media is not deleted" || len(semantic.Details()) != 0 {
		t.Fatalf("media-not-deleted error = %#v", err)
	}
}

func assertUnavailableError(t *testing.T, err error) {
	t.Helper()
	var semantic *SemanticError
	if !errors.As(err, &semantic) || semantic.Kind() != KindDatabaseUnavailable || semantic.HTTPStatus() != 503 ||
		semantic.Code() != CodeUnavailable || semantic.Message() != "service is temporarily unavailable" || len(semantic.Details()) != 0 {
		t.Fatalf("maintenance error = %#v", err)
	}
}

func operationID(operation lifecycleOperation, mediaID, jobID string) string {
	if operation == operationStart {
		return jobID
	}
	return mediaID
}

type lifecycleDatabaseSnapshot struct {
	media        string
	jobs         string
	events       string
	feedPosition int64
}

func lifecycleSnapshot(t *testing.T, pool *pgxpool.Pool, mediaID string) lifecycleDatabaseSnapshot {
	t.Helper()
	var snapshot lifecycleDatabaseSnapshot
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT to_jsonb(media)::text FROM media WHERE id=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(jobs) ORDER BY id)::text,'[]') FROM jobs WHERE media_id_snapshot=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(change_events) ORDER BY position)::text,'[]') FROM change_events WHERE media_id=$1),
		(SELECT last_position FROM change_feed_state WHERE id=1)`, mediaID).Scan(
		&snapshot.media, &snapshot.jobs, &snapshot.events, &snapshot.feedPosition,
	)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
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

func intPointer(value int) *int    { return &value }
func boolPointer(value bool) *bool { return &value }

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

func assertSemanticJSONEqual(t *testing.T, first, second any) {
	t.Helper()
	decode := func(value any) any {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	firstJSON, secondJSON := decode(first), decode(second)
	if !reflect.DeepEqual(firstJSON, secondJSON) {
		t.Fatalf("semantic JSON differs:\nlifecycle: %#v\nread API: %#v", firstJSON, secondJSON)
	}
}
