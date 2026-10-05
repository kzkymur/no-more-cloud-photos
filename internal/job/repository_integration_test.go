package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
)

func TestClaimIntegrationOrderingTypesAvailabilityAndHydration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	future, _ := insertTransformJobAt(t, pool, 3, 1, `clock_timestamp()+interval '1 hour'`)
	first, _ := insertTransformJob(t, pool, 3, 1)
	purge := insertPurgeJob(t, pool, 3, `clock_timestamp()`)

	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != first || lease.Type != TypeTransform || lease.Token == "" || lease.Attempts != 1 || !lease.StartedAt.Before(lease.LeaseExpiresAt) {
		t.Fatalf("first claim = %+v, %v", lease, err)
	}
	if len(lease.Targets) != 1 {
		t.Fatalf("transform targets = %+v", lease.Targets)
	}
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("future/unsupported claim error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, future); err != nil {
		t.Fatal(err)
	}
	if lease, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil || lease.ID != future {
		t.Fatalf("exact-boundary claim = %+v, %v", lease, err)
	}
	if _, err := repository.Claim(ctx, []Type{TypePurge}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("purge claim boundary error = %v", err)
	}
	var purgeStatus Status
	var purgeStarted *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,started_at FROM jobs WHERE id=$1`, purge).Scan(&purgeStatus, &purgeStarted); err != nil || purgeStatus != StatusQueued || purgeStarted != nil {
		t.Fatalf("purge changed by generic claim: status=%s started=%v err=%v", purgeStatus, purgeStarted, err)
	}
	if _, err := repository.Claim(ctx, []Type{"unsupported"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered API type error = %v", err)
	}
}

func TestClaimIntegrationExclusivitySkipLockedAndReturnsCommitted(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	lockedID, _ := insertTransformJob(t, pool, 3, 1)
	nextID, _ := insertTransformJobAt(t, pool, 3, 1, `clock_timestamp()+interval '1 microsecond'`)
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(ctx)
	if _, err := locker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, lockedID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lease, err := repository.Claim(claimCtx, []Type{TypeTransform})
	if err != nil || lease.ID != nextID {
		t.Fatalf("skip-locked claim = %+v, %v", lease, err)
	}
	if _, err := repository.Claim(claimCtx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("all candidates locked/running error = %v", err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	lockCtx, lockCancel := context.WithTimeout(ctx, time.Second)
	defer lockCancel()
	committed, err := pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer committed.Rollback(ctx)
	if _, err := committed.Exec(lockCtx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE NOWAIT`, nextID); err != nil {
		t.Fatalf("Claim returned before commit/lock release: %v", err)
	}

	if lease, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil || lease.ID != lockedID {
		t.Fatalf("released candidate claim = %+v, %v", lease, err)
	}

	onlyID, _ := insertTransformJob(t, pool, 3, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			lease, err := repository.Claim(ctx, []Type{TypeTransform})
			if err == nil && lease.ID != onlyID {
				err = fmt.Errorf("claimed %s, want %s", lease.ID, onlyID)
			}
			results <- err
		}()
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) || !(errors.Is(firstErr, ErrNoWork) || errors.Is(secondErr, ErrNoWork)) {
		t.Fatalf("two-worker single-job results = %v, %v", firstErr, secondErr)
	}
}

func TestHeartbeatIntegrationCASClockAndExactExpiry(t *testing.T) {
	pool, repository := integrationRepository(t, Options{LeaseDuration: 30 * time.Second, Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 4, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, newTestUUID(t)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong token heartbeat = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	expiry, err := repository.Heartbeat(ctx, jobID, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	var deltaSeconds float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(epoch FROM lease_expires_at-clock_timestamp()) FROM jobs WHERE id=$1`, jobID).Scan(&deltaSeconds); err != nil {
		t.Fatal(err)
	}
	if deltaSeconds < 28 || deltaSeconds > 31 || expiry.IsZero() {
		t.Fatalf("heartbeat extension from DB clock = %f seconds, expiry %s", deltaSeconds, expiry)
	}
	var beforeDisconnect time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM jobs WHERE id=$1`, jobID).Scan(&beforeDisconnect); err != nil {
		t.Fatal(err)
	}
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID); err != nil {
		t.Fatal(err)
	}
	heartbeatResult := make(chan error, 1)
	go func() {
		_, err := repository.Heartbeat(ctx, jobID, lease.Token)
		heartbeatResult <- err
	}()
	var heartbeatPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE pid<>pg_backend_pid() AND state='active' AND query LIKE 'UPDATE jobs SET lease_expires_at=clock_timestamp()+%'
			ORDER BY query_start DESC LIMIT 1`).Scan(&heartbeatPID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if heartbeatPID == 0 {
		t.Fatal("blocked heartbeat backend was not observed")
	}
	var terminated bool
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, heartbeatPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate heartbeat backend = %v, %v", terminated, err)
	}
	if err := <-heartbeatResult; !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("disconnected heartbeat error = %v", err)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var afterDisconnect time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM jobs WHERE id=$1`, jobID).Scan(&afterDisconnect); err != nil {
		t.Fatal(err)
	}
	if !afterDisconnect.Equal(beforeDisconnect) {
		t.Fatalf("disconnected heartbeat changed expiry: before=%s after=%s", beforeDisconnect, afterDisconnect)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, lease.Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("exact-expiry heartbeat = %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired owner mutation = %v", err)
	}
	if count, err := repository.ReclaimExpired(ctx); err != nil || count != 1 {
		t.Fatalf("reclaim exact-expiry job = %d, %v", count, err)
	}
	reclaimed, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, lease.Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old-token heartbeat = %v", err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, reclaimed.Token); err != nil {
		t.Fatalf("new-token heartbeat = %v", err)
	}
}

func TestTransformTargetsRetryAndCompletionIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != jobID || len(lease.Targets) != 2 {
		t.Fatalf("transform claim = %+v, %v", lease, err)
	}
	for _, target := range lease.Targets {
		if target.Profile.ID == "" || target.Profile.Key == "" || target.Profile.Version <= 0 || target.Profile.Processor == "" ||
			target.Profile.ParametersSchemaVersion <= 0 || len(target.Profile.InputMIMETypes) == 0 || len(target.Profile.Parameters) == 0 {
			t.Fatalf("incomplete pinned profile hydration: %+v", target.Profile)
		}
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate begin = %v", err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM job_targets WHERE id=$1`, targets[0]).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("target begin attempts = %d, %v", attempts, err)
	}
	publishTargetFixture(t, pool, targets[0])
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[1], FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteSucceeded(ctx, jobID, lease.Token); !errors.Is(err, ErrConflict) {
		t.Fatalf("partial completion error = %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	retry, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || len(retry.Targets) != 1 || retry.Targets[0].ID != targets[1] || retry.Targets[0].Status != TargetPending || retry.Targets[0].ErrorCode != nil {
		t.Fatalf("retry hydration = %+v, %v", retry, err)
	}
	if err := repository.BeginTarget(ctx, jobID, retry.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	publishTargetFixture(t, pool, targets[1])
	if err := repository.CompleteSucceeded(ctx, jobID, retry.Token); err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil || status != StatusSucceeded {
		t.Fatalf("completed status = %s, %v", status, err)
	}
}

func TestFinishAttemptCeilingAndSafeErrorsIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 1, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureCode("stderr /secret/file")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe failure accepted: %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessOutputLimit); err != nil {
		t.Fatal(err)
	}
	var status Status
	var code, message string
	var finished *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,error_code,error_message,finished_at FROM jobs WHERE id=$1`, jobID).Scan(&status, &code, &message, &finished); err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed || code != string(FailureProcessOutputLimit) || message != safeFailureMessages[FailureProcessOutputLimit] || finished == nil || strings.Contains(message, "secret") {
		t.Fatalf("durable failure = %s %q %q %v", status, code, message, finished)
	}
}

func TestReclaimExpiredIntegrationBudgetBatchConcurrencyAndRace(t *testing.T) {
	pool, repository := integrationRepository(t, Options{ReclaimBatch: 2, Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	remaining, _ := insertTransformJob(t, pool, 2, 1)
	ceiling, _ := insertTransformJob(t, pool, 1, 1)
	notExpired, _ := insertTransformJob(t, pool, 2, 1)
	leases := make(map[string]Lease)
	for range 3 {
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		leases[lease.ID] = lease
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=ANY($1::uuid[])`, []string{remaining, ceiling}); err != nil {
		t.Fatal(err)
	}
	count, err := repository.ReclaimExpired(ctx)
	if err != nil || count != 2 {
		t.Fatalf("reclaim = %d, %v", count, err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text,status,error_code FROM jobs WHERE error_code IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string][2]string{}
	for rows.Next() {
		var id, status, code string
		if err := rows.Scan(&id, &status, &code); err != nil {
			t.Fatal(err)
		}
		states[id] = [2]string{status, code}
	}
	if states[remaining] != [2]string{"queued", "lease_expired"} || states[ceiling] != [2]string{"failed", "lease_expired"} {
		t.Fatalf("reclaimed states = %#v", states)
	}
	if err := repository.CompleteSucceeded(ctx, remaining, leases[remaining].Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("reclaimed stale owner = %v", err)
	}
	if _, err := repository.Heartbeat(ctx, notExpired, leases[notExpired].Token); err != nil {
		t.Fatalf("live owner lost reclaim race: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, remaining); err != nil {
		t.Fatal(err)
	}

	for range 4 {
		id, _ := insertTransformJob(t, pool, 2, 1)
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if lease.ID != id {
			t.Fatalf("claim ID = %s, want %s", lease.ID, id)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	var total atomic.Int32
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count, err := repository.ReclaimExpired(ctx)
			if err == nil {
				total.Add(int32(count))
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if total.Load() != 4 {
		t.Fatalf("two-reclaimer total = %d, want 4", total.Load())
	}
}

func TestAdminRetryIntegrationCeilingAuditAndConcurrency(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	publishTargetFixture(t, pool, targets[0])
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[1], FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	audits := []AdminAudit{
		{ID: newTestUUID(t), Actor: "admin-a", Host: "host-a", SanitizedArguments: []byte(`{"additional_attempts":3}`)},
		{ID: newTestUUID(t), Actor: "admin-b", Host: "host-b", SanitizedArguments: []byte(`{"additional_attempts":5}`)},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index := range audits {
		go func(index int) {
			<-start
			results <- repository.AdminRetryFailed(ctx, jobID, 3+index*2, audits[index])
		}(index)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || !(errors.Is(first, ErrConflict) || errors.Is(second, ErrConflict)) {
		t.Fatalf("concurrent retry errors = %v, %v", first, second)
	}
	var status Status
	var attempts, maximum, auditsCount int
	var immediatelyAvailable bool
	if err := pool.QueryRow(ctx, `SELECT status,attempts,max_attempts,available_at<=clock_timestamp(),(SELECT count(*) FROM admin_audit) FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts, &maximum, &immediatelyAvailable, &auditsCount); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 3 || maximum < 6 || !immediatelyAvailable || auditsCount != 2 {
		t.Fatalf("retry state = %s attempts=%d max=%d available=%v audits=%d", status, attempts, maximum, immediatelyAvailable, auditsCount)
	}
	var succeeded, pending string
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM job_targets WHERE id=$1),(SELECT status FROM job_targets WHERE id=$2)`, targets[0], targets[1]).Scan(&succeeded, &pending); err != nil {
		t.Fatal(err)
	}
	if succeeded != "succeeded" || pending != "pending" {
		t.Fatalf("target states after retry = %s, %s", succeeded, pending)
	}
	var succeededAudits, failedAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome='succeeded'),count(*) FILTER (WHERE outcome='failed' AND error_code='conflict' AND error_message='job is not failed') FROM admin_audit`).Scan(&succeededAudits, &failedAudits); err != nil {
		t.Fatal(err)
	}
	if succeededAudits != 1 || failedAudits != 1 {
		t.Fatalf("admin retry audit outcomes = succeeded:%d failed:%d", succeededAudits, failedAudits)
	}
	if _, err := pool.Exec(ctx, `UPDATE admin_audit SET actor='tampered'`); err == nil {
		t.Fatal("admin audit was mutable")
	}
	lease, err = repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != jobID {
		t.Fatalf("admin-retried immediate claim = %+v, %v", lease, err)
	}
}

func TestQueuedAttemptInvariantCounterexamplesIntegration(t *testing.T) {
	pool, _ := integrationRepository(t, Options{})
	ctx := context.Background()
	jobID := insertPurgeJob(t, pool, 1, `clock_timestamp()`)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET attempts=max_attempts WHERE id=$1`, jobID); err == nil {
		t.Fatal("queued attempts=max_attempts accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET max_attempts=attempts WHERE id=$1`, jobID); err == nil {
		t.Fatal("queued max_attempts=attempts accepted")
	}
}

func integrationRepository(t *testing.T, options Options) (*pgxpool.Pool, *Repository) {
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
	id := strings.ReplaceAll(newTestUUID(t), "-", "")
	schema := "job_" + id
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop job integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxConns < 6 {
		config.MaxConns = 6
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
	repository, err := NewRepository(pool, options)
	if err != nil {
		t.Fatal(err)
	}
	return pool, repository
}

func insertPurgeJob(t *testing.T, pool *pgxpool.Pool, maxAttempts int, availableExpression string) string {
	t.Helper()
	id := newTestUUID(t)
	mediaID := newTestUUID(t)
	query := fmt.Sprintf(`INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,available_at) VALUES ($1,'purge',$2,'queued',$3,%s)`, availableExpression)
	if _, err := pool.Exec(context.Background(), query, id, mediaID, maxAttempts); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertTransformJob(t *testing.T, pool *pgxpool.Pool, maxAttempts, targetCount int) (string, []string) {
	return insertTransformJobAt(t, pool, maxAttempts, targetCount, `clock_timestamp()`)
}

func insertTransformJobAt(t *testing.T, pool *pgxpool.Pool, maxAttempts, targetCount int, availableExpression string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	mediaID, originalID, jobID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height) VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`,
		originalID, mediaID, strings.Repeat(strings.ReplaceAll(mediaID, "-", ""), 2), "originals/aa/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM profiles ORDER BY key,version LIMIT $1`, targetCount)
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, id)
	}
	rows.Close()
	if len(profiles) != targetCount {
		t.Fatalf("profile fixtures = %d, want %d", len(profiles), targetCount)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	query := fmt.Sprintf(`INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at) VALUES ($1,'transform',$2,$3,'queued',$4,%s)`, availableExpression)
	if _, err := tx.Exec(ctx, query, jobID, originalID, mediaID, maxAttempts); err != nil {
		t.Fatal(err)
	}
	targets := make([]string, 0, targetCount)
	for _, profileID := range profiles {
		targetID := newTestUUID(t)
		if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, targetID)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID, targets
}

// publishTargetFixture establishes the cross-table invariant owned by the
// later publication issue without exposing a repository operation that could
// mark a target succeeded before its rendition is durable and referenced.
func publishTargetFixture(t *testing.T, pool *pgxpool.Pool, targetID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	renditionID := newTestUUID(t)
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',error_code=NULL,error_message=NULL,updated_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO renditions
		(id,media_id,job_target_id,profile_key,is_current,purge_after,relative_path,mime_type,width,height,size_bytes,sha256)
		SELECT $1,j.media_id_snapshot,jt.id,'fixture',false,clock_timestamp()+interval '1 day',$2,'image/avif',1,1,1,$3
		FROM job_targets jt JOIN jobs j ON j.id=jt.job_id WHERE jt.id=$4`,
		renditionID, "renditions/fixture/"+renditionID+".avif", strings.Repeat("a", 64), targetID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
