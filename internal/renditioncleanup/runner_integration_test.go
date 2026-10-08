package renditioncleanup

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const (
	integrationPoisonCount  = maxTrackedPoisonCandidates + 1
	integrationHealthyIndex = integrationPoisonCount + 1
	integrationCurrentIndex = integrationHealthyIndex + 1
)

type observedPostgresCleanup struct {
	repository        *medialifecycle.PostgresRepository
	calls             int
	empty             int
	maxBatchRemaining int
	maxExclusions     int
	sawKeysetCursor   bool
	seen              map[string]int
}

func (cleanup *observedPostgresCleanup) CleanupNextRendition(ctx context.Context, preferred string, excluded []string, unlink Unlink) (string, error) {
	cleanup.calls++
	if len(excluded) > cleanup.maxExclusions {
		cleanup.maxExclusions = len(excluded)
	}
	id, err := cleanup.repository.CleanupNextRendition(ctx, preferred, excluded, unlink)
	if id == "" {
		cleanup.empty++
	} else {
		cleanup.seen[id]++
	}

	value := reflect.ValueOf(cleanup.repository).Elem()
	if remaining := value.FieldByName("cleanupBatch").Len(); remaining > cleanup.maxBatchRemaining {
		cleanup.maxBatchRemaining = remaining
	}
	cleanup.sawKeysetCursor = cleanup.sawKeysetCursor || !value.FieldByName("cleanupAfter").IsNil()
	return id, err
}

type healthyCallbackStore struct {
	cancel context.CancelFunc
	calls  int
	ids    []string
}

func (store *healthyCallbackStore) DeleteRendition(_ context.Context, key storage.RenditionKey, _ storage.DeleteExpectation) (storage.DeleteResult, error) {
	store.calls++
	store.ids = append(store.ids, key.RenditionID().String())
	store.cancel()
	return storage.DeleteResult{Missing: true}, nil
}

func TestRunnerIntegrationPostgresMoreThanTrackedPoisonReachesHealthy(t *testing.T) {
	pool := cleanupRunnerIntegrationPool(t)
	fixture := insertCleanupRunnerFixture(t, pool)
	repository, err := medialifecycle.NewPostgresRepository(pool, "https://files.example/files")
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedPostgresCleanup{repository: repository, seen: make(map[string]int)}

	now := time.Unix(1_700_000_000, 0).UTC()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := &healthyCallbackStore{cancel: cancel}
	sweeps := 0
	runner, err := New(observed, store, Options{
		SweepInterval:       time.Nanosecond,
		SweepLimit:          DefaultSweepLimit,
		TransientBackoff:    time.Millisecond,
		MaxTransientBackoff: time.Millisecond,
		PoisonCooldown:      time.Hour,
		MaxPoisonCooldown:   time.Hour,
		Now:                 func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			sweeps++
			return nil
		},
		Random: func() float64 { return 1 },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if store.calls != 1 || !reflect.DeepEqual(store.ids, []string{fixture.healthyID}) {
		t.Fatalf("healthy callback calls=%d ids=%v, want exactly [%s]", store.calls, store.ids, fixture.healthyID)
	}
	if len(runner.poison) != maxTrackedPoisonCandidates || observed.maxExclusions != maxTrackedPoisonCandidates {
		t.Fatalf("poison memory=%d max exclusions=%d, want %d", len(runner.poison), observed.maxExclusions, maxTrackedPoisonCandidates)
	}
	if _, retained := runner.poison[fixture.firstPoisonID]; retained {
		t.Fatalf("deterministically oldest poison %s was not evicted", fixture.firstPoisonID)
	}
	if _, retained := runner.poison[fixture.lastPoisonID]; !retained {
		t.Fatalf("newest poison %s was not retained", fixture.lastPoisonID)
	}
	if observed.maxBatchRemaining != 49 {
		t.Fatalf("maximum frozen batch remaining=%d, want 49 after selecting from a bounded 50-candidate batch", observed.maxBatchRemaining)
	}
	if !observed.sawKeysetCursor || sweeps <= integrationPoisonCount/DefaultSweepLimit {
		t.Fatalf("persistent keyset cursor=%t sweeps=%d, want cursor across multiple 50-candidate batches and sweeps", observed.sawKeysetCursor, sweeps)
	}
	if observed.empty == 0 || observed.calls <= integrationPoisonCount+1 {
		t.Fatalf("repository calls=%d empty batch boundaries=%d", observed.calls, observed.empty)
	}
	for index := 1; index <= integrationPoisonCount; index++ {
		id := cleanupRunnerUUID("33000000", index)
		if observed.seen[id] != 1 {
			t.Fatalf("poison %s selected %d times, want once", id, observed.seen[id])
		}
	}
	if observed.seen[fixture.healthyID] != 1 {
		t.Fatalf("healthy %s selected %d times, want once", fixture.healthyID, observed.seen[fixture.healthyID])
	}

	var healthyExists bool
	var poisonRemaining, healthyProgress int
	if err := pool.QueryRow(context.Background(), `SELECT
		EXISTS (SELECT 1 FROM renditions WHERE id=$1),
		(SELECT count(*) FROM renditions WHERE id>=$2 AND id<=$3),
		(SELECT count(*) FROM rendition_cleanup_progress WHERE rendition_id=$1 AND disposition='missing')`,
		fixture.healthyID, fixture.firstPoisonID, fixture.lastPoisonID).Scan(&healthyExists, &poisonRemaining, &healthyProgress); err != nil {
		t.Fatal(err)
	}
	if healthyExists || poisonRemaining != integrationPoisonCount || healthyProgress != 1 {
		t.Fatalf("healthy exists=%t poison remaining=%d healthy progress=%d", healthyExists, poisonRemaining, healthyProgress)
	}
}

type cleanupRunnerFixture struct {
	firstPoisonID string
	lastPoisonID  string
	healthyID     string
}

func insertCleanupRunnerFixture(t *testing.T, pool *pgxpool.Pool) cleanupRunnerFixture {
	t.Helper()
	ctx := context.Background()
	const mediaID = "30000000-0000-4000-8000-000000000001"
	const originalID = "30000000-0000-4000-8000-000000000002"
	var profileID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, originalID, mediaID, strings.Repeat("f", 64), "originals/30/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts)
		SELECT ('31000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid,'transform',$1,$2,'queued',3
		FROM generate_series(1,$3) AS generated(n)`, originalID, mediaID, integrationCurrentIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status)
		SELECT ('32000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid,
		       ('31000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid,$1,'pending'
		FROM generate_series(1,$2) AS generated(n)`, profileID, integrationCurrentIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,
		lease_token=('36000000-0000-4000-8000-' || right(id::text,12))::uuid,
		lease_expires_at=statement_timestamp()+interval '1 hour',started_at=statement_timestamp(),updated_at=statement_timestamp()
		WHERE id>='31000000-0000-4000-8000-000000000001'::uuid
		  AND id<=('31000000-0000-4000-8000-' || lpad(to_hex($1),12,'0'))::uuid`, integrationCurrentIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',attempts=1,updated_at=statement_timestamp()
		WHERE id>='32000000-0000-4000-8000-000000000001'::uuid
		  AND id<=('32000000-0000-4000-8000-' || lpad(to_hex($1),12,'0'))::uuid`, integrationCurrentIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO renditions
		(id,media_id,job_target_id,is_current,purge_after,relative_path,mime_type,size_bytes,width,height,sha256,processor_audit)
		SELECT rendition_id,$1,target_id,false,statement_timestamp()-interval '1 hour',
		       'renditions/30/' || $2 || '/' || target_id || '/' ||
		       CASE WHEN n=$3 THEN rendition_id::text ELSE ('34000000-0000-4000-8000-' || lpad(to_hex(n),12,'0')) END || '.avif',
		       'image/avif',1,1,1,lpad(to_hex(n),64,'0'),'{"fixture":"cleanup-runner"}'::jsonb
		FROM generate_series(1,$3) AS generated(n)
		CROSS JOIN LATERAL (SELECT
			('32000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid AS target_id,
			('33000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid AS rendition_id
		) AS ids`, mediaID, originalID, integrationHealthyIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO renditions
		(id,media_id,job_target_id,is_current,relative_path,mime_type,size_bytes,width,height,sha256,processor_audit)
		VALUES ($1,$2,$3,true,$4,'image/avif',1,1,1,$5,'{"fixture":"cleanup-runner-current"}')`,
		cleanupRunnerUUID("33000000", integrationCurrentIndex), mediaID, cleanupRunnerUUID("32000000", integrationCurrentIndex),
		"renditions/30/"+originalID+"/"+cleanupRunnerUUID("32000000", integrationCurrentIndex)+"/"+cleanupRunnerUUID("33000000", integrationCurrentIndex)+".avif",
		strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,
		finished_at=statement_timestamp(),updated_at=statement_timestamp()
		WHERE id>='31000000-0000-4000-8000-000000000001'::uuid
		  AND id<=('31000000-0000-4000-8000-' || lpad(to_hex($1),12,'0'))::uuid`, integrationCurrentIndex); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return cleanupRunnerFixture{
		firstPoisonID: cleanupRunnerUUID("33000000", 1),
		lastPoisonID:  cleanupRunnerUUID("33000000", integrationPoisonCount),
		healthyID:     cleanupRunnerUUID("33000000", integrationHealthyIndex),
	}
}

func cleanupRunnerIntegrationPool(t *testing.T) *pgxpool.Pool {
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
	schema := "renditioncleanup_" + strings.ReplaceAll(cleanupRunnerUUID("39000000", int(time.Now().UnixNano()&0xffffffffff)), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop rendition cleanup integration schema: %v", err)
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

func cleanupRunnerUUID(prefix string, value int) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012x", prefix, value)
}
