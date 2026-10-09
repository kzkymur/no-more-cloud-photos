package upload

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
)

func TestRepositoryIntegrationAcceptanceReplayConflictAndDuplicate(t *testing.T) {
	ctx := context.Background()
	pool, repository := uploadIntegrationRepository(t)
	timezone, err := repository.DefaultTimezone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := integrationAcceptance(t, "first-key", "first-request", timezone, []byte("same-content"), testUUIDs[0], testUUIDs[1])
	registerIntegrationAcceptance(t, repository, input)
	publishes := 0
	outcome, err := repository.Finalize(ctx, input, func() (string, error) {
		publishes++
		return "originals/00/00000000-0000-4000-8000-000000000001/original.jpg", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != 201 || publishes != 1 || !bytesContain(outcome.Body, `"job":null`) {
		t.Fatalf("new outcome=%+v publishes=%d body=%s", outcome, publishes, outcome.Body)
	}
	var mediaCount, originalCount, jobCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM media), (SELECT count(*) FROM originals),
		(SELECT count(*) FROM jobs), (SELECT count(*) FROM change_events)`).Scan(
		&mediaCount, &originalCount, &jobCount, &eventCount); err != nil {
		t.Fatal(err)
	}
	if mediaCount != 1 || originalCount != 1 || jobCount != 0 || eventCount != 1 {
		t.Fatalf("row counts media/original/job/event = %d/%d/%d/%d", mediaCount, originalCount, jobCount, eventCount)
	}

	replayInput := freshReplayAcceptance(t, repository, input)
	replay, err := repository.Finalize(ctx, replayInput, func() (string, error) {
		t.Fatal("replay published")
		return "", nil
	})
	if err != nil || !replay.Replayed || replay.Status != 201 || !jsonSemanticallyEqual(replay.Body, outcome.Body) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	conflictInput := freshReplayAcceptance(t, repository, input)
	conflictInput.RequestHash = sha256.Sum256([]byte("different request"))
	conflict, err := repository.Finalize(ctx, conflictInput, func() (string, error) {
		t.Fatal("conflict published")
		return "", nil
	})
	if err != nil || conflict.Status != 409 || !bytesContain(conflict.Body, `"idempotency_conflict"`) {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}

	duplicateInput := integrationAcceptance(t, "duplicate-key", "duplicate-request", timezone, []byte("same-content"), testUUIDs[2], testUUIDs[3])
	registerIntegrationAcceptance(t, repository, duplicateInput)
	duplicate, err := repository.Finalize(ctx, duplicateInput, func() (string, error) {
		t.Fatal("duplicate published")
		return "", nil
	})
	if err != nil || duplicate.Status != 409 || !bytesContain(duplicate.Body, `"duplicate_media"`) ||
		!bytesContain(duplicate.Body, `"request_id":"duplicate-request"`) {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	duplicateReplayInput := freshReplayAcceptance(t, repository, duplicateInput)
	duplicateReplayInput.RequestID = "current-replay-request"
	duplicateReplay, err := repository.Finalize(ctx, duplicateReplayInput, func() (string, error) {
		t.Fatal("duplicate replay published")
		return "", nil
	})
	if err != nil || !duplicateReplay.Replayed || errorRequestID(duplicateReplay.Body) != "duplicate-request" ||
		bytesContain(duplicateReplay.Body, "current-replay-request") {
		t.Fatalf("duplicate replay=%+v err=%v", duplicateReplay, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=statement_timestamp() WHERE id=$1`, input.MediaID); err != nil {
		t.Fatal(err)
	}
	deletedDuplicate := integrationAcceptance(t, "deleted-duplicate-key", "deleted-request", timezone, []byte("same-content"), testUUIDs[3], testUUIDs[4])
	registerIntegrationAcceptance(t, repository, deletedDuplicate)
	deletedOutcome, err := repository.Finalize(ctx, deletedDuplicate, func() (string, error) {
		t.Fatal("deleted duplicate published")
		return "", nil
	})
	if err != nil || !bytesContain(deletedOutcome.Body, `"deleted":true`) ||
		!bytesContain(deletedOutcome.Body, `"existing_media_id":"`+input.MediaID+`"`) {
		t.Fatalf("deleted duplicate=%+v err=%v", deletedOutcome, err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	profileA, _ := NewUUIDv4()
	profileZ, _ := NewUUIDv4()
	if _, err := pool.Exec(ctx, `
		INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters,activated_at)
		VALUES ($1,'zeta',2,'active',ARRAY['image/jpeg'],'nmcp-media',1,'{}',statement_timestamp()),
		       ($2,'alpha',3,'active',ARRAY['image/jpeg'],'nmcp-media',1,'{}',statement_timestamp())`, profileZ, profileA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	originalID, _ := NewUUIDv4()
	mediaID, _ := NewUUIDv4()
	profileInput := integrationAcceptance(t, "profile-key", "profile-request", timezone, []byte("different-content"), originalID, mediaID)
	registerIntegrationAcceptance(t, repository, profileInput)
	profileOutcome, err := repository.Finalize(ctx, profileInput, func() (string, error) {
		return "originals/11/11111111-1111-4111-8111-111111111111/original.jpg", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var response uploadResponse
	if err := json.Unmarshal(profileOutcome.Body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Job == nil || response.Job.MaxAttempts != 3 || len(response.Job.Targets) != 2 ||
		response.Job.Targets[0].Profile.Key != "alpha" || response.Job.Targets[1].Profile.Key != "zeta" {
		t.Fatalf("profile response = %s", profileOutcome.Body)
	}
	var persistedJobs, persistedTargets int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM job_targets)`).Scan(&persistedJobs, &persistedTargets); err != nil {
		t.Fatal(err)
	}
	if persistedJobs != 1 || persistedTargets != 2 {
		t.Fatalf("persisted jobs/targets = %d/%d", persistedJobs, persistedTargets)
	}
}

func TestRepositoryIntegrationConcurrentSameKeyReplay(t *testing.T) {
	ctx := context.Background()
	pool, repository := uploadIntegrationRepository(t)
	timezone, err := repository.DefaultTimezone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []acceptance{
		integrationAcceptance(t, "concurrent-replay", "replay-request", timezone, []byte("concurrent-replay-content"), testUUIDs[0], testUUIDs[1]),
		integrationAcceptance(t, "concurrent-replay", "replay-request", timezone, []byte("concurrent-replay-content"), testUUIDs[2], testUUIDs[3]),
	}
	for _, input := range inputs {
		registerIntegrationAcceptance(t, repository, input)
	}
	type result struct {
		outcome Outcome
		err     error
	}
	start := make(chan struct{})
	calling := make(chan struct{}, 2)
	publishReached := make(chan struct{}, 1)
	releasePublish := make(chan struct{})
	results := make(chan result, 2)
	var publishes atomic.Int32
	var ready sync.WaitGroup
	ready.Add(2)
	for index := range inputs {
		go func(index int) {
			ready.Done()
			<-start
			calling <- struct{}{}
			outcome, err := repository.Finalize(ctx, inputs[index], func() (string, error) {
				publishes.Add(1)
				publishReached <- struct{}{}
				<-releasePublish
				return "originals/00/concurrent-replay/original.jpg", nil
			})
			results <- result{outcome: outcome, err: err}
		}(index)
	}
	ready.Wait()
	close(start)
	<-calling
	<-calling
	<-publishReached
	close(releasePublish)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent replay errors = %v, %v", first.err, second.err)
	}
	if first.outcome.Status != 201 || second.outcome.Status != 201 ||
		first.outcome.Replayed == second.outcome.Replayed || !jsonSemanticallyEqual(first.outcome.Body, second.outcome.Body) {
		t.Fatalf("concurrent replay outcomes = %+v, %+v", first.outcome, second.outcome)
	}
	if publishes.Load() != 1 {
		t.Fatalf("publish calls = %d, want 1", publishes.Load())
	}
	assertUploadRowCounts(t, pool, 1, 1, 0, 1, 1)
}

func TestRepositoryIntegrationConcurrentDifferentKeysSameSHA(t *testing.T) {
	ctx := context.Background()
	pool, repository := uploadIntegrationRepository(t)
	timezone, err := repository.DefaultTimezone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []acceptance{
		integrationAcceptance(t, "sha-key-a", "sha-request-a", timezone, []byte("shared-sha-content"), testUUIDs[0], testUUIDs[1]),
		integrationAcceptance(t, "sha-key-b", "sha-request-b", timezone, []byte("shared-sha-content"), testUUIDs[2], testUUIDs[3]),
	}
	for _, input := range inputs {
		registerIntegrationAcceptance(t, repository, input)
	}
	type result struct {
		outcome Outcome
		err     error
	}
	start := make(chan struct{})
	calling := make(chan struct{}, 2)
	publishReached := make(chan struct{}, 1)
	releasePublish := make(chan struct{})
	results := make(chan result, 2)
	var publishes atomic.Int32
	var ready sync.WaitGroup
	ready.Add(2)
	for index := range inputs {
		go func(index int) {
			ready.Done()
			<-start
			calling <- struct{}{}
			outcome, err := repository.Finalize(ctx, inputs[index], func() (string, error) {
				publishes.Add(1)
				publishReached <- struct{}{}
				<-releasePublish
				return "originals/00/concurrent-sha/original.jpg", nil
			})
			results <- result{outcome: outcome, err: err}
		}(index)
	}
	ready.Wait()
	close(start)
	<-calling
	<-calling
	<-publishReached
	close(releasePublish)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent SHA errors = %v, %v", first.err, second.err)
	}
	statuses := map[int]int{first.outcome.Status: 1}
	statuses[second.outcome.Status]++
	if statuses[201] != 1 || statuses[409] != 1 {
		t.Fatalf("concurrent SHA statuses = %d, %d", first.outcome.Status, second.outcome.Status)
	}
	duplicate := first.outcome
	if duplicate.Status != 409 {
		duplicate = second.outcome
	}
	if duplicate.Replayed || !bytesContain(duplicate.Body, `"duplicate_media"`) || publishes.Load() != 1 {
		t.Fatalf("duplicate outcome=%+v publishes=%d", duplicate, publishes.Load())
	}
	assertUploadRowCounts(t, pool, 1, 1, 0, 1, 2)
}

func TestRepositoryIntegrationMaintenanceAndTimezoneRejection(t *testing.T) {
	t.Run("maintenance rejects before publication", func(t *testing.T) {
		ctx := context.Background()
		pool, repository := uploadIntegrationRepository(t)
		timezone, err := repository.DefaultTimezone(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='integration',entered_at=statement_timestamp() WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		input := integrationAcceptance(t, "maintenance-key", "maintenance-request", timezone, []byte("maintenance-content"), testUUIDs[0], testUUIDs[1])
		_, err = repository.Finalize(ctx, input, func() (string, error) {
			t.Fatal("maintenance request published")
			return "", nil
		})
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "unavailable" {
			t.Fatalf("maintenance error = %#v", err)
		}
		assertUploadRowCounts(t, pool, 0, 0, 0, 0, 0)
	})

	t.Run("timezone change returns retry signal", func(t *testing.T) {
		ctx := context.Background()
		pool, repository := uploadIntegrationRepository(t)
		timezone, err := repository.DefaultTimezone(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE system_config SET default_timezone='UTC' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		input := integrationAcceptance(t, "timezone-key", "timezone-request", timezone, []byte("timezone-content"), testUUIDs[0], testUUIDs[1])
		registerIntegrationAcceptance(t, repository, input)
		_, err = repository.Finalize(ctx, input, func() (string, error) {
			t.Fatal("timezone-change request published")
			return "", nil
		})
		var changed *timezoneChangedError
		if !errors.As(err, &changed) || changed.Timezone != "UTC" || !errors.Is(err, errTimezoneChanged) {
			t.Fatalf("timezone-change error = %#v", err)
		}
		assertUploadRowCounts(t, pool, 0, 0, 0, 0, 0)
	})
}

func uploadIntegrationRepository(t *testing.T) (*pgxpool.Pool, *pgRepository) {
	t.Helper()
	pool := uploadIntegrationPool(t, nil)
	return pool, newPGRepository(pool, nil)
}

func uploadIntegrationPool(t *testing.T, configure func(*pgxpool.Config, *pgxpool.Pool)) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("NMCP_TEST_DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	baseConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if baseConfig.MaxConns < 2 {
		baseConfig.MaxConns = 2
	}
	base, err := pgxpool.NewWithConfig(ctx, baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	id, err := NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	schema := "upload_" + strings.ReplaceAll(id, "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop upload integration schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Integration races may need an operation, an explicit blocker, and an
	// independent observer connection at the same time. Do not inherit a
	// single-connection DSN that turns lock evidence into pool starvation.
	if config.MaxConns < 4 {
		config.MaxConns = 4
	}
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,false)`, schema+",pg_catalog,pg_temp")
		return err
	}
	if configure != nil {
		configure(config, base)
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

func assertUploadRowCounts(t *testing.T, pool *pgxpool.Pool, media, originals, jobs, events, idempotency int) {
	t.Helper()
	var gotMedia, gotOriginals, gotJobs, gotEvents, gotIdempotency int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM media), (SELECT count(*) FROM originals),
		(SELECT count(*) FROM jobs), (SELECT count(*) FROM change_events),
		(SELECT count(*) FROM idempotency_requests)`).Scan(
		&gotMedia, &gotOriginals, &gotJobs, &gotEvents, &gotIdempotency); err != nil {
		t.Fatal(err)
	}
	if gotMedia != media || gotOriginals != originals || gotJobs != jobs || gotEvents != events || gotIdempotency != idempotency {
		t.Fatalf("row counts media/originals/jobs/events/idempotency = %d/%d/%d/%d/%d, want %d/%d/%d/%d/%d",
			gotMedia, gotOriginals, gotJobs, gotEvents, gotIdempotency, media, originals, jobs, events, idempotency)
	}
}

func integrationAcceptance(t *testing.T, key, requestID, timezone string, content []byte, originalID, mediaID string) acceptance {
	t.Helper()
	result := validMetadata()
	exif, err := result.EXIFJSON()
	if err != nil {
		t.Fatal(err)
	}
	source, err := result.SourceMetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	filename := "photo.jpg"
	return acceptance{
		Key: key, RequestID: requestID, RequestHash: CanonicalRequestHashV1(digest, uint64(len(content)), &filename),
		SHA256: digest, Size: int64(len(content)), Filename: &filename, OriginalID: originalID, AttemptID: mediaID, MediaID: mediaID,
		Timezone: timezone, Metadata: result, EXIFJSON: json.RawMessage(exif), SourceJSON: json.RawMessage(source),
	}
}

func registerIntegrationAcceptance(t *testing.T, repository *pgRepository, input acceptance) {
	t.Helper()
	if err := repository.RegisterAttempt(context.Background(), input.AttemptID, input.OriginalID); err != nil {
		t.Fatalf("register upload attempt: %v", err)
	}
}

func freshReplayAcceptance(t *testing.T, repository *pgRepository, input acceptance) acceptance {
	t.Helper()
	var err error
	if input.OriginalID, err = NewUUIDv4(); err != nil {
		t.Fatal(err)
	}
	if input.AttemptID, err = NewUUIDv4(); err != nil {
		t.Fatal(err)
	}
	if input.MediaID, err = NewUUIDv4(); err != nil {
		t.Fatal(err)
	}
	registerIntegrationAcceptance(t, repository, input)
	return input
}

func bytesContain(body []byte, value string) bool { return strings.Contains(string(body), value) }

func jsonSemanticallyEqual(first, second []byte) bool {
	var firstValue, secondValue any
	if json.Unmarshal(first, &firstValue) != nil || json.Unmarshal(second, &secondValue) != nil {
		return false
	}
	return reflect.DeepEqual(firstValue, secondValue)
}

func errorRequestID(body []byte) string {
	var response errorResponse
	if json.Unmarshal(body, &response) != nil {
		return ""
	}
	return response.Error.RequestID
}
