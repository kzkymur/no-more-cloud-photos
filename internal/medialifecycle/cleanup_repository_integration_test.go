package medialifecycle

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCleanupRepositoryIntegrationRollsBackBeforeCommitAndConvergesUnknownCommit(t *testing.T) {
	t.Run("action failure leaves due Rendition without pending history", func(t *testing.T) {
		ctx := context.Background()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		fixture := insertCleanupRenditions(t, pool, 2250, &deadline, 1, 1, false)
		want := errors.New("unlink failed before mutation")
		id, err := repository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			return false, want
		})
		if id != fixture.renditionID || !errors.Is(err, want) {
			t.Fatalf("CleanupNextRendition() = %q, %v", id, err)
		}
		var renditionExists bool
		var progress int
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM renditions WHERE id=$1),(SELECT count(*) FROM rendition_cleanup_progress WHERE rendition_id=$1)`, fixture.renditionID).Scan(&renditionExists, &progress); err != nil {
			t.Fatal(err)
		}
		if !renditionExists || progress != 0 {
			t.Fatalf("rolled-back cleanup rendition=%t progress=%d", renditionExists, progress)
		}
	})

	t.Run("lost committed response converges without second unlink", func(t *testing.T) {
		ctx := context.Background()
		pool := cleanupIntegrationPool(t)
		repository := newCleanupTestRepository(t, pool)
		deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		fixture := insertCleanupRenditions(t, pool, 2270, &deadline, 1, 1, false)
		lost := errors.New("commit response lost")
		var fail atomic.Bool
		fail.Store(true)
		repository.afterCommit = func(context.Context) error {
			if fail.Swap(false) {
				return lost
			}
			return nil
		}
		calls := 0
		action := func(context.Context, string, string, int64) (bool, error) {
			calls++
			return true, nil
		}
		id, err := repository.CleanupNextRendition(ctx, "", nil, action)
		var unknown *CommitOutcomeUnknown
		if id != fixture.renditionID || !errors.As(err, &unknown) {
			t.Fatalf("uncertain CleanupNextRendition() = %q, %v", id, err)
		}
		id, err = repository.CleanupNextRendition(ctx, fixture.renditionID, nil, action)
		if err != nil || id != fixture.renditionID || calls != 1 {
			t.Fatalf("converged CleanupNextRendition() = %q, %v; calls=%d", id, err, calls)
		}
		var disposition string
		if err := pool.QueryRow(ctx, `SELECT disposition FROM rendition_cleanup_progress WHERE rendition_id=$1`, fixture.renditionID).Scan(&disposition); err != nil {
			t.Fatal(err)
		}
		if disposition != "missing" {
			t.Fatalf("disposition=%q, want missing", disposition)
		}
	})
}

func TestCleanupRepositoryIntegrationCompletesExactHistoricalSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := cleanupIntegrationPool(t)
	repository := newCleanupTestRepository(t, pool)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fixture := insertCleanupRenditions(t, pool, 1000, &deadline, 1, 1, false)

	var calls int
	gotID, err := repository.CleanupNextRendition(ctx, "", nil, func(_ context.Context, id, path string, size int64) (bool, error) {
		calls++
		if id != fixture.renditionID || path != fixture.relativePath || size != fixture.sizeBytes {
			t.Fatalf("cleanup callback = %s/%s/%d, want %s/%s/%d", id, path, size, fixture.renditionID, fixture.relativePath, fixture.sizeBytes)
		}
		return false, nil
	})
	if err != nil || gotID != fixture.renditionID || calls != 1 {
		t.Fatalf("CleanupNextRendition() = %q, %v; callback calls = %d", gotID, err, calls)
	}

	var mediaID, renditionID, targetID, relativePath, disposition string
	var sizeBytes int64
	var purgeAfter time.Time
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT media_id_snapshot::text,rendition_id::text,job_target_id::text,
		relative_path,size_bytes,purge_after,disposition,completed_at
		FROM rendition_cleanup_progress WHERE rendition_id=$1`, fixture.renditionID).Scan(
		&mediaID, &renditionID, &targetID, &relativePath, &sizeBytes, &purgeAfter, &disposition, &completedAt,
	); err != nil {
		t.Fatal(err)
	}
	if mediaID != fixture.mediaID || renditionID != fixture.renditionID || targetID != fixture.targetID ||
		relativePath != fixture.relativePath || sizeBytes != fixture.sizeBytes || !purgeAfter.Equal(deadline) ||
		disposition != "deleted" || completedAt == nil {
		t.Fatalf("cleanup progress = %s/%s/%s/%s/%d/%s/%s/%v", mediaID, renditionID, targetID, relativePath, sizeBytes, purgeAfter, disposition, completedAt)
	}
	var renditionExists bool
	var shaColumns int
	if err := pool.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM renditions WHERE id=$1),
		(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='rendition_cleanup_progress' AND column_name='sha256')`, fixture.renditionID).Scan(&renditionExists, &shaColumns); err != nil {
		t.Fatal(err)
	}
	if renditionExists || shaColumns != 0 {
		t.Fatalf("terminal cleanup rendition_exists=%t progress_sha_columns=%d", renditionExists, shaColumns)
	}
}

func TestCleanupRepositoryIntegrationRejectsIneligibleRenditionsBeforeCallback(t *testing.T) {
	tests := []struct {
		name                             string
		deadline                         func() *time.Time
		candidateVersion, currentVersion int
		candidateCurrent                 bool
	}{
		{name: "current", deadline: func() *time.Time { return nil }, candidateVersion: 1, currentVersion: 1, candidateCurrent: true},
		{name: "null deadline", deadline: func() *time.Time { return nil }, candidateVersion: 1, currentVersion: 1},
		{name: "future deadline", deadline: func() *time.Time { value := time.Now().UTC().Add(time.Hour); return &value }, candidateVersion: 1, currentVersion: 1},
		{name: "stale higher version", deadline: func() *time.Time { value := time.Now().UTC().Add(-time.Hour); return &value }, candidateVersion: 2, currentVersion: 1},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool := cleanupIntegrationPool(t)
			repository := newCleanupTestRepository(t, pool)
			insertCleanupRenditions(t, pool, 1100+index*20, test.deadline(), test.candidateVersion, test.currentVersion, test.candidateCurrent)
			calls := 0
			id, err := repository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
				calls++
				return false, nil
			})
			if err != nil || id != "" || calls != 0 {
				t.Fatalf("CleanupNextRendition() = %q, %v; callback calls = %d", id, err, calls)
			}
			var progress int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM rendition_cleanup_progress`).Scan(&progress); err != nil {
				t.Fatal(err)
			}
			if progress != 0 {
				t.Fatalf("cleanup progress rows = %d, want 0", progress)
			}
		})
	}
}

func TestCleanupRepositoryIntegrationPurgeHistoryOwnsFilesOnlyAfterStart(t *testing.T) {
	tests := []struct {
		name        string
		started     bool
		finalStatus string
		wantCalls   int
	}{
		{name: "never-started queued purge allows cleanup", wantCalls: 1},
		{name: "running purge skips cleanup", started: true, finalStatus: "running"},
		{name: "queued retry history skips cleanup", started: true, finalStatus: "queued"},
		{name: "failed history skips cleanup", started: true, finalStatus: "failed"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			pool := cleanupIntegrationPool(t)
			repository := newCleanupTestRepository(t, pool)
			service, err := NewService(pool, "https://files.example/files")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			fixture := insertCleanupRenditions(t, pool, 1200+index*20, &deadline, 1, 1, false)
			if _, err := service.Delete(ctx, fixture.mediaID); err != nil {
				t.Fatal(err)
			}
			enqueued, err := service.EnqueuePurge(ctx, fixture.mediaID)
			if err != nil {
				t.Fatal(err)
			}
			if test.started {
				if _, err := service.StartPurge(ctx, enqueued.Job.ID); err != nil {
					t.Fatal(err)
				}
				switch test.finalStatus {
				case "queued":
					_, err = pool.Exec(ctx, `UPDATE jobs SET status='queued',lease_token=NULL,lease_expires_at=NULL,available_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, enqueued.Job.ID)
				case "failed":
					_, err = pool.Exec(ctx, `UPDATE jobs SET status='failed',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp(),error_code='test',error_message='test',updated_at=clock_timestamp() WHERE id=$1`, enqueued.Job.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			calls := 0
			id, err := repository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
				calls++
				return false, nil
			})
			if err != nil || calls != test.wantCalls {
				t.Fatalf("CleanupNextRendition() = %q, %v; callback calls = %d, want %d", id, err, calls, test.wantCalls)
			}
			if test.wantCalls == 1 && id != fixture.renditionID {
				t.Fatalf("cleaned rendition = %q, want %q", id, fixture.renditionID)
			}
			if test.wantCalls == 0 && id != "" {
				t.Fatalf("skipped cleanup returned rendition %q", id)
			}
		})
	}
}

func TestCleanupRepositoryIntegrationFrozenBatchHonorsLimitAndPublicationBoundary(t *testing.T) {
	ctx := context.Background()
	pool := cleanupIntegrationPool(t)
	repository := newCleanupTestRepository(t, pool)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	const batchSize = 50
	for index := 0; index < batchSize+1; index++ {
		insertCleanupRenditions(t, pool, 1400+index*10, &deadline, 1, 1, false)
	}
	publicationRepository, publicationCandidate := lifecyclePublicationFixture(t, pool, 3000)
	if _, err := pool.Exec(ctx, `UPDATE system_config SET superseded_rendition_retention_days=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	cleaned := make(map[string]struct{})
	action := func(_ context.Context, id, _ string, _ int64) (bool, error) {
		cleaned[id] = struct{}{}
		return false, nil
	}
	if id, err := repository.CleanupNextRendition(ctx, "", nil, action); err != nil || id == "" {
		t.Fatalf("first batch cleanup = %q, %v", id, err)
	}
	if _, err := publicationRepository.PublishRendition(ctx, publicationCandidate); err != nil {
		t.Fatal(err)
	}
	secondPublication := publicationCandidate
	secondPublication.JobID = integrationUUID(3010)
	secondPublication.TargetID = integrationUUID(3011)
	secondPublication.LeaseToken = integrationUUID(3012)
	secondPublication.ID = integrationUUID(3013)
	secondPublication.RelativePath = "renditions/" + secondPublication.OriginalID[:2] + "/" + secondPublication.OriginalID + "/" + secondPublication.TargetID + "/" + secondPublication.ID + ".avif"
	secondPublication.SHA256 = strings.Repeat("c", 64)
	publicationTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publicationTx.Rollback(context.Background())
	if _, err := publicationTx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts)
		VALUES ($1,'transform',$2,$3,'queued',3)`, secondPublication.JobID, secondPublication.OriginalID, secondPublication.MediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := publicationTx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, secondPublication.TargetID, secondPublication.JobID, secondPublication.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := publicationTx.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 hour',started_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1`, secondPublication.JobID, secondPublication.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := publicationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := publicationRepository.PublishRendition(ctx, secondPublication); err != nil {
		t.Fatal(err)
	}
	newlyPublishedID := publicationCandidate.ID
	for index := 1; index < batchSize; index++ {
		if id, err := repository.CleanupNextRendition(ctx, "", nil, action); err != nil || id == "" {
			t.Fatalf("cached batch cleanup %d = %q, %v", index, id, err)
		}
	}
	if id, err := repository.CleanupNextRendition(ctx, "", nil, action); err != nil || id != "" {
		t.Fatalf("frozen batch boundary = %q, %v; want no work", id, err)
	}
	if len(cleaned) != batchSize {
		t.Fatalf("frozen batch cleaned %d Renditions, want %d", len(cleaned), batchSize)
	}
	if _, visible := cleaned[newlyPublishedID]; visible {
		t.Fatal("Rendition published after the repeatable-read snapshot entered the frozen batch")
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE NOT is_current`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining historical Renditions = %d, want 2", remaining)
	}
	for index := 0; index < 2; index++ {
		if id, err := repository.CleanupNextRendition(ctx, "", nil, action); err != nil || id == "" {
			t.Fatalf("next snapshot cleanup %d = %q, %v", index, id, err)
		}
	}
	if _, visible := cleaned[newlyPublishedID]; !visible {
		t.Fatal("next cleanup snapshot did not observe newly published due Rendition")
	}
}

func TestCleanupRepositoryIntegrationTwoCleanersInvokeActionExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := cleanupIntegrationPool(t)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fixture := insertCleanupRenditions(t, pool, 2100, &deadline, 1, 1, false)
	firstConn, firstRepository, _ := cleanupConnectionRepository(t, ctx, pool)
	defer firstConn.Release()
	secondConn, secondRepository, secondPID := cleanupConnectionRepository(t, ctx, pool)
	defer secondConn.Release()

	var calls atomic.Int32
	firstInAction := make(chan struct{})
	releaseFirst := make(chan struct{})
	type outcome struct {
		id  string
		err error
	}
	firstResult := make(chan outcome, 1)
	go func() {
		id, err := firstRepository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			calls.Add(1)
			close(firstInAction)
			<-releaseFirst
			return false, nil
		})
		firstResult <- outcome{id: id, err: err}
	}()
	<-firstInAction
	secondResult := make(chan outcome, 1)
	go func() {
		id, err := secondRepository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			calls.Add(1)
			return false, nil
		})
		secondResult <- outcome{id: id, err: err}
	}()
	awaitLockWait(t, ctx, pool, secondPID)
	close(releaseFirst)
	first, second := <-firstResult, <-secondResult
	if first.err != nil || second.err != nil || first.id != fixture.renditionID || second.id != fixture.renditionID || calls.Load() != 1 {
		t.Fatalf("cleaner outcomes = %#v / %#v; callback calls = %d", first, second, calls.Load())
	}
	var progress int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rendition_cleanup_progress WHERE rendition_id=$1 AND disposition IN ('deleted','missing')`, fixture.renditionID).Scan(&progress); err != nil {
		t.Fatal(err)
	}
	if progress != 1 {
		t.Fatalf("terminal cleanup rows = %d, want 1", progress)
	}
}

func TestCleanupRepositoryIntegrationWaitsForMaintenanceBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := cleanupIntegrationPool(t)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fixture := insertCleanupRenditions(t, pool, 2200, &deadline, 1, 1, false)

	maintenance, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Rollback(context.Background())
	if _, err := maintenance.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='cleanup drain test',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	connection, repository, pid := cleanupConnectionRepository(t, ctx, pool)
	defer connection.Release()
	var calls atomic.Int32
	result := make(chan error, 1)
	go func() {
		id, err := repository.CleanupNextRendition(ctx, "", nil, func(context.Context, string, string, int64) (bool, error) {
			calls.Add(1)
			return false, nil
		})
		if err == nil && id != fixture.renditionID {
			err = &cleanupUnexpectedIDError{got: id, want: fixture.renditionID}
		}
		result <- err
	}()
	awaitLockWait(t, ctx, pool, pid)
	if calls.Load() != 0 {
		t.Fatal("cleanup callback ran before maintenance boundary drained")
	}
	if err := maintenance.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup callback calls after maintenance rollback = %d, want 1", calls.Load())
	}
}

type cleanupUnexpectedIDError struct {
	got, want string
}

func (e *cleanupUnexpectedIDError) Error() string {
	return "cleanup returned " + e.got + ", want " + e.want
}

type cleanupRenditionFixture struct {
	mediaID, renditionID, targetID, relativePath string
	sizeBytes                                    int64
}

func cleanupIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" && os.Getenv("NMCP_TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	return integrationPool(t)
}

func newCleanupTestRepository(t *testing.T, pool *pgxpool.Pool) *PostgresRepository {
	t.Helper()
	repository, err := NewPostgresRepository(pool, "https://files.example/files")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func cleanupConnectionRepository(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, *PostgresRepository, int32) {
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
	return connection, &PostgresRepository{db: connection, fileBaseURL: "https://files.example/files/", newID: newUUIDv4}, pid
}

func insertCleanupRenditions(t *testing.T, pool *pgxpool.Pool, seed int, candidateDeadline *time.Time, candidateVersion, currentVersion int, candidateCurrent bool) cleanupRenditionFixture {
	t.Helper()
	ctx := context.Background()
	mediaID, originalID := integrationUUID(seed), integrationUUID(seed+1)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',10,1,1)`, originalID, mediaID,
		strings.Repeat("0", 52)+originalID[len(originalID)-12:], "originals/cleanup/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	profileIDs := map[int]string{}
	for _, version := range []int{candidateVersion, currentVersion} {
		if _, exists := profileIDs[version]; exists {
			continue
		}
		if version == 1 {
			var profileID string
			if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
				t.Fatal(err)
			}
			profileIDs[version] = profileID
			continue
		}
		profileID := integrationUUID(seed + 2 + version)
		if _, err := pool.Exec(ctx, `INSERT INTO profiles
			(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			SELECT $1,key,$2,'draft',input_mime_types,processor,parameters_schema_version,parameters
			FROM profiles WHERE key='standard' AND version=1`, profileID, version); err != nil {
			t.Fatal(err)
		}
		profileIDs[version] = profileID
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := cleanupRenditionFixture{
		mediaID: mediaID, renditionID: integrationUUID(seed + 8), targetID: integrationUUID(seed + 6),
		relativePath: "renditions/cleanup/" + integrationUUID(seed+8) + ".avif", sizeBytes: int64(seed + 17),
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	insert := func(jobID, targetID, renditionID, profileID, relativePath string, current bool, purgeAfter *time.Time, sha string) {
		t.Helper()
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at)
			VALUES ($1,'transform',$2,$3,'queued',3,$4,$4,$4)`, jobID, originalID, mediaID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status,updated_at) VALUES ($1,$2,$3,'pending',$4)`, targetID, jobID, profileID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=$3,started_at=$4,updated_at=$4 WHERE id=$1`, jobID, integrationUUID(seed+9+len(relativePath)), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',attempts=1,updated_at=$2 WHERE id=$1`, targetID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO renditions
			(id,media_id,job_target_id,is_current,purge_after,relative_path,mime_type,size_bytes,width,height,sha256,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'image/avif',$7,1,1,$8,$9)`, renditionID, mediaID, targetID, current, purgeAfter, relativePath, fixture.sizeBytes, sha, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=$2,updated_at=$2 WHERE id=$1`, jobID, now); err != nil {
			t.Fatal(err)
		}
	}
	insert(integrationUUID(seed+4), fixture.targetID, fixture.renditionID, profileIDs[candidateVersion], fixture.relativePath, candidateCurrent, candidateDeadline, strings.Repeat("a", 64))
	if !candidateCurrent {
		insert(integrationUUID(seed+5), integrationUUID(seed+7), integrationUUID(seed+9), profileIDs[currentVersion], "renditions/cleanup/"+integrationUUID(seed+9)+".avif", true, nil, strings.Repeat("b", 64))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}
