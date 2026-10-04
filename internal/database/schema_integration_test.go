package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runInitialSchemaIntegrationTests(t *testing.T, databaseURL string) {
	t.Helper()
	t.Run("initial schema seeds and checks", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()

		var deletedRetention, renditionRetention *int
		var timezone string
		var interval, backupRetention, profiles int
		if err := pool.QueryRow(ctx, `
			SELECT deleted_media_retention_days, superseded_rendition_retention_days,
			       default_timezone, db_backup_interval_hours, db_backup_retention_days,
			       (SELECT count(*) FROM profiles)
			FROM system_config WHERE id = 1`).Scan(
			&deletedRetention, &renditionRetention, &timezone, &interval, &backupRetention, &profiles,
		); err != nil {
			t.Fatalf("read initial config: %v", err)
		}
		if deletedRetention != nil || renditionRetention != nil || timezone != "Asia/Tokyo" || interval != 24 || backupRetention != 30 {
			t.Fatalf("unexpected initial config: deleted=%v rendition=%v timezone=%q interval=%d retention=%d",
				deletedRetention, renditionRetention, timezone, interval, backupRetention)
		}
		if profiles != 0 {
			t.Fatalf("profile seed count = %d, want 0", profiles)
		}
		var position int64
		var mode string
		if err := pool.QueryRow(ctx, `SELECT last_position FROM change_feed_state WHERE id=1`).Scan(&position); err != nil {
			t.Fatalf("read feed seed: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1`).Scan(&mode); err != nil {
			t.Fatalf("read maintenance seed: %v", err)
		}
		if position != 0 || mode != "normal" {
			t.Fatalf("singleton seeds = position %d, mode %q", position, mode)
		}

		expectExecError(t, pool, `INSERT INTO system_config (id, default_timezone, db_backup_interval_hours, db_backup_retention_days) VALUES (2, 'UTC', 24, 30)`)
		expectExecError(t, pool, `UPDATE system_config SET default_timezone='Not/A_Real_Zone' WHERE id=1`)
		expectExecError(t, pool, `UPDATE system_config SET deleted_media_retention_days=-1 WHERE id=1`)
		expectExecError(t, pool, `UPDATE system_config SET db_backup_interval_hours=0 WHERE id=1`)
		if _, err := pool.Exec(ctx, `UPDATE system_config SET default_timezone='UTC', deleted_media_retention_days=0 WHERE id=1`); err != nil {
			t.Fatalf("valid config update: %v", err)
		}
	})

	t.Run("media original uniqueness and checks", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		mediaOne := newUUIDv4(t)
		mediaTwo := newUUIDv4(t)
		insertMedia(t, pool, mediaOne)
		insertMedia(t, pool, mediaTwo)

		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source) VALUES ($1,'image',now(),'unknown')`, newUUIDv4(t))
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source,taken_at_timezone) VALUES ($1,'image',now(),'embedded_offset','UTC')`, newUUIDv4(t))
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source,taken_at_timezone) VALUES ($1,'image',now(),'default_timezone','Not/A_Real_Zone')`, newUUIDv4(t))
		expectExecError(t, pool, `UPDATE media SET purge_after=now() WHERE id=$1`, mediaOne)
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at_source) VALUES ('00000000-0000-0000-0000-000000000000','image','unknown')`)

		sha := strings.Repeat("a", 64)
		if _, err := pool.Exec(ctx, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, newUUIDv4(t), mediaOne, sha, "originals/aa/one/original.jpg"); err != nil {
			t.Fatalf("insert original: %v", err)
		}
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,$4,'image/jpeg',1)`, newUUIDv4(t), mediaTwo, sha, "originals/bb/two/original.jpg")
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'IMAGE/JPEG',1,1,NULL)`, newUUIDv4(t), mediaTwo, strings.Repeat("B", 64), "/absolute")

		if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=now(), purge_after=now() WHERE id=$1`, mediaOne); err != nil {
			t.Fatalf("logically delete media: %v", err)
		}
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,$4,'image/jpeg',1)`, newUUIDv4(t), mediaTwo, sha, "originals/bb/two/original.jpg")

		concurrentMediaOne := newUUIDv4(t)
		concurrentMediaTwo := newUUIDv4(t)
		insertMedia(t, pool, concurrentMediaOne)
		insertMedia(t, pool, concurrentMediaTwo)
		start := make(chan struct{})
		errorsByInsert := make(chan error, 2)
		originalIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index, id := range []string{concurrentMediaOne, concurrentMediaTwo} {
			go func(index int, id string) {
				<-start
				_, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`,
					originalIDs[index], id, strings.Repeat("9", 64), fmt.Sprintf("originals/99/concurrent-%d/original.jpg", index))
				errorsByInsert <- err
			}(index, id)
		}
		close(start)
		assertOneConcurrentWinner(t, errorsByInsert)
	})

	t.Run("profile activation is monotonic and immutable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileOne := insertDraftProfile(t, pool, "standard", 1)
		profileTwo := insertDraftProfile(t, pool, "standard", 2)
		profileThree := insertDraftProfile(t, pool, "standard", 3)

		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileOne); err != nil {
			t.Fatalf("activate v1: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileThree); err != nil {
			t.Fatalf("activate v3: %v", err)
		}
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, profileTwo)
		expectExecError(t, pool, `UPDATE profiles SET parameters='{"changed":true}'::jsonb WHERE id=$1`, profileThree)
		expectExecError(t, pool, `UPDATE profiles SET status='draft' WHERE id=$1`, profileOne)
		expectExecError(t, pool, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters,activated_at) VALUES ($1,'bad',1,'active',ARRAY['image/jpeg'],'still',1,'{}',now())`, newUUIDv4(t))
		expectExecError(t, pool, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters) VALUES ($1,'bad',1,'draft',ARRAY['image/jpeg','image/jpeg'],'still',1,'{}')`, newUUIDv4(t))

		var activeVersion int
		if err := pool.QueryRow(ctx, `SELECT version FROM profiles WHERE key='standard' AND status='active'`).Scan(&activeVersion); err != nil {
			t.Fatalf("read active profile: %v", err)
		}
		if activeVersion != 3 {
			t.Fatalf("active version = %d, want 3", activeVersion)
		}

		concurrentTwo := insertDraftProfile(t, pool, "concurrent", 2)
		concurrentThree := insertDraftProfile(t, pool, "concurrent", 3)
		highTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin high activation: %v", err)
		}
		if _, err := highTx.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, concurrentThree); err != nil {
			_ = highTx.Rollback(ctx)
			t.Fatalf("stage high activation: %v", err)
		}
		lowResult := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, concurrentTwo)
			lowResult <- err
		}()
		if err := highTx.Commit(ctx); err != nil {
			t.Fatalf("commit high activation: %v", err)
		}
		if err := awaitResult(t, lowResult); err == nil {
			t.Fatal("lower concurrent activation unexpectedly succeeded after higher version")
		}

		orderedTwo := insertDraftProfile(t, pool, "ordered", 2)
		orderedThree := insertDraftProfile(t, pool, "ordered", 3)
		lowTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin low activation: %v", err)
		}
		if _, err := lowTx.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, orderedTwo); err != nil {
			_ = lowTx.Rollback(ctx)
			t.Fatalf("stage low activation: %v", err)
		}
		highResult := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, orderedThree)
			highResult <- err
		}()
		if err := lowTx.Commit(ctx); err != nil {
			t.Fatalf("commit low activation: %v", err)
		}
		if err := awaitResult(t, highResult); err != nil {
			t.Fatalf("higher concurrent activation after lower commit: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT version FROM profiles WHERE key='ordered' AND status='active'`).Scan(&activeVersion); err != nil || activeVersion != 3 {
			t.Fatalf("ordered concurrent active version = %d, err=%v", activeVersion, err)
		}
	})

	t.Run("concurrent current-rendition uniqueness", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileID := insertDraftProfile(t, pool, "standard", 1)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate profile: %v", err)
		}
		mediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		originalID := insertOriginal(t, pool, mediaID, "8", "concurrent")
		targetOne := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		targetTwo := insertPendingTransform(t, pool, mediaID, originalID, profileID)

		start := make(chan struct{})
		results := make(chan error, 2)
		renditionIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index, targetID := range []string{targetOne, targetTwo} {
			go func(index int, targetID string) {
				<-start
				tx, err := pool.Begin(ctx)
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID)
				}
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256) VALUES ($1,$2,$3,'ignored',true,$4,'image/avif',1,$5)`,
						renditionIDs[index], mediaID, targetID, fmt.Sprintf("renditions/88/concurrent/target-%d/output.avif", index), strings.Repeat(fmt.Sprintf("%x", index+6), 64))
				}
				if err == nil {
					err = tx.Commit(ctx)
				} else if tx != nil {
					_ = tx.Rollback(ctx)
				}
				results <- err
			}(index, targetID)
		}
		close(start)
		assertOneConcurrentWinner(t, results)
		var currentCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE media_id=$1 AND profile_key='standard' AND is_current`, mediaID).Scan(&currentCount); err != nil {
			t.Fatalf("count current renditions: %v", err)
		}
		if currentCount != 1 {
			t.Fatalf("current rendition count = %d, want 1", currentCount)
		}
	})

	t.Run("job target rendition and purge history invariants", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileID := insertDraftProfile(t, pool, "standard", 1)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate profile: %v", err)
		}

		mediaID := newUUIDv4(t)
		otherMediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		insertMedia(t, pool, otherMediaID)
		originalID := insertOriginal(t, pool, mediaID, "c", "first")
		otherOriginalID := insertOriginal(t, pool, otherMediaID, "d", "second")
		expectExecError(t, pool, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, newUUIDv4(t), otherOriginalID, mediaID)
		expectExecError(t, pool, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,$3,'queued',3)`, newUUIDv4(t), originalID, mediaID)

		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, newUUIDv4(t), originalID, mediaID)
			return err
		})

		purgeID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, purgeID, mediaID); err != nil {
			t.Fatalf("insert purge job: %v", err)
		}
		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, newUUIDv4(t), purgeID, profileID)
			return err
		})
		expectExecError(t, pool, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,finished_at) VALUES ($1,'purge',$2,'failed',3,now())`, newUUIDv4(t), mediaID)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled', finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, purgeID); err != nil {
			t.Fatalf("cancel purge: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, newUUIDv4(t), mediaID); err != nil {
			t.Fatalf("new purge after cancellation: %v", err)
		}

		jobID := newUUIDv4(t)
		targetID := newUUIDv4(t)
		renditionID := newUUIDv4(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin publication: %v", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,started_at,finished_at) VALUES ($1,'transform',$2,$3,'succeeded',3,now(),now())`, jobID, originalID, mediaID); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height) VALUES ($1,$2,$3,'ignored',true,$4,'image/avif',10,$5,1,1)`, renditionID, mediaID, targetID, "renditions/cc/one/target/output.avif", strings.Repeat("e", 64))
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("publication statements: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit publication: %v", err)
		}

		var profileKey string
		if err := pool.QueryRow(ctx, `SELECT profile_key FROM renditions WHERE id=$1`, renditionID).Scan(&profileKey); err != nil || profileKey != "standard" {
			t.Fatalf("derived rendition profile key = %q, err=%v", profileKey, err)
		}
		expectExecError(t, pool, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256) VALUES ($1,$2,$3,'standard',false,$4,'image/avif',1,$5)`, newUUIDv4(t), otherMediaID, targetID, "renditions/dd/bad/target/output.avif", strings.Repeat("f", 64))

		if _, err := pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, mediaID); err != nil {
			t.Fatalf("physically purge media: %v", err)
		}
		var originalReference *string
		var targetCount, renditionCount int
		if err := pool.QueryRow(ctx, `SELECT original_id::text FROM jobs WHERE id=$1`, jobID).Scan(&originalReference); err != nil {
			t.Fatalf("read preserved job: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_targets WHERE id=$1`, targetID).Scan(&targetCount); err != nil {
			t.Fatalf("count preserved target: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE id=$1`, renditionID).Scan(&renditionCount); err != nil {
			t.Fatalf("count cascaded rendition: %v", err)
		}
		if originalReference != nil || targetCount != 1 || renditionCount != 0 {
			t.Fatalf("purge history = original %v target %d rendition %d", originalReference, targetCount, renditionCount)
		}
	})

	t.Run("durable state is constrained and immutable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		mediaID := newUUIDv4(t)
		requestHash := strings.Repeat("1", 64)
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body,media_id_snapshot) VALUES ('POST /media','request-1',$1,201,'{}',$2)`, requestHash, mediaID); err != nil {
			t.Fatalf("insert idempotency row: %v", err)
		}
		expectExecError(t, pool, `UPDATE idempotency_requests SET http_status=409 WHERE scope='POST /media' AND key='request-1'`)
		expectExecError(t, pool, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body) VALUES ('POST /other','x',$1,201,'{}')`, requestHash)
		start := make(chan struct{})
		idempotencyResults := make(chan error, 2)
		idempotencyMediaIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index := 0; index < 2; index++ {
			go func(index int) {
				<-start
				_, err := pool.Exec(ctx, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body,media_id_snapshot) VALUES ('POST /media','concurrent',$1,201,$2,$3)`,
					strings.Repeat(fmt.Sprintf("%x", index+3), 64), fmt.Sprintf(`{"winner":%d}`, index), idempotencyMediaIDs[index])
				idempotencyResults <- err
			}(index)
		}
		close(start)
		assertOneConcurrentWinner(t, idempotencyResults)

		eventID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,1,'media_upsert','upload',$2,'{}')`, eventID, mediaID); err != nil {
			t.Fatalf("insert change event: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,2,'media_deleted','logical_delete',$2,'{}')`, newUUIDv4(t), mediaID)
		expectExecError(t, pool, `DELETE FROM change_events WHERE id=$1`, eventID)

		expectExecError(t, pool, `INSERT INTO backup_runs (id,status,config_snapshot,finished_at) VALUES ($1,'succeeded','{}',now())`, newUUIDv4(t))
		if _, err := pool.Exec(ctx, `INSERT INTO backup_runs (id,status,config_snapshot,finished_at,final_relative_path,size_bytes,sha256,manifest,postgres_version,tool_version) VALUES ($1,'succeeded','{}',now(),'backups/run/dump',1,$2,'{}','17','pg_dump 17')`, newUUIDv4(t), strings.Repeat("2", 64)); err != nil {
			t.Fatalf("insert succeeded backup: %v", err)
		}
		expectExecError(t, pool, `UPDATE maintenance_state SET mode='maintenance' WHERE id=1`)
		if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance', reason='restore', owner='admin', entered_at=now() WHERE id=1`); err != nil {
			t.Fatalf("enter maintenance: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO reconciliation_reports (id,scope,findings,repair_disposition,quarantine_paths) VALUES ($1,'all','{}','{}',ARRAY['../escape'])`, newUUIDv4(t))
	})

	t.Run("required indexes are present and usable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		required := []string{
			"media_list_order_idx", "media_due_purge_idx", "profiles_one_active_key_idx",
			"jobs_dequeue_idx", "jobs_list_idx", "jobs_status_list_idx", "jobs_media_list_idx",
			"jobs_one_active_purge_idx", "job_targets_job_id_idx", "job_targets_profile_id_idx",
			"renditions_media_id_idx", "renditions_job_target_id_idx", "renditions_one_current_key_idx",
			"renditions_cleanup_idx", "backup_runs_succeeded_idx", "admin_batches_resume_idx",
		}
		for _, name := range required {
			var definition string
			if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1`, name).Scan(&definition); err != nil {
				t.Errorf("required index %s: %v", name, err)
			}
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire explain connection: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SET enable_seqscan=off`); err != nil {
			t.Fatalf("disable sequential scans for index capability evidence: %v", err)
		}
		rows, err := conn.Query(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM jobs WHERE status='queued' AND available_at <= now() ORDER BY available_at,created_at,id LIMIT 1`)
		if err != nil {
			t.Fatalf("explain dequeue query: %v", err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan explain: %v", err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("explain rows: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("EXPLAIN returned no plan")
		}
		if plan := strings.Join(lines, "\n"); !strings.Contains(plan, "jobs_dequeue_idx") {
			t.Fatalf("dequeue plan does not use jobs_dequeue_idx with sequential scans disabled: %s", plan)
		}
	})
}

func migratedIntegrationPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool := integrationPool(t, databaseURL)
	migrator, err := NewMigrator(pool)
	if err != nil {
		t.Fatalf("NewMigrator(): %v", err)
	}
	if err := migrator.Up(context.Background()); err != nil {
		t.Fatalf("apply initial schema: %v", err)
	}
	return pool
}

func newUUIDv4(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[0:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

func expectExecError(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err == nil {
		t.Fatalf("statement unexpectedly succeeded: %s", sql)
	}
}

func expectTxCommitError(t *testing.T, pool *pgxpool.Pool, run func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin expected-failure transaction: %v", err)
	}
	if err := run(tx); err != nil {
		_ = tx.Rollback(ctx)
		return
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("transaction commit unexpectedly succeeded")
	}
}

func insertMedia(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image','unknown')`, id); err != nil {
		t.Fatalf("insert media %s: %v", id, err)
	}
}

func insertOriginal(t *testing.T, pool *pgxpool.Pool, mediaID, digestNibble, suffix string) string {
	t.Helper()
	id := newUUIDv4(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
		VALUES ($1,$2,$3,$4,'image/jpeg',1)`, id, mediaID, strings.Repeat(digestNibble, 64), "originals/aa/"+suffix+"/original.jpg"); err != nil {
		t.Fatalf("insert original: %v", err)
	}
	return id
}

func insertDraftProfile(t *testing.T, pool *pgxpool.Pool, key string, version int) string {
	t.Helper()
	id := newUUIDv4(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
		VALUES ($1,$2,$3,'draft',ARRAY['image/jpeg'],'still',1,'{}')`, id, key, version); err != nil {
		t.Fatalf("insert draft profile %s/%d: %v", key, version, err)
	}
	return id
}

func insertPendingTransform(t *testing.T, pool *pgxpool.Pool, mediaID, originalID, profileID string) string {
	t.Helper()
	ctx := context.Background()
	jobID := newUUIDv4(t)
	targetID := newUUIDv4(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transform fixture: %v", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create transform fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transform fixture: %v", err)
	}
	return targetID
}

func assertOneConcurrentWinner(t *testing.T, results <-chan error) {
	t.Helper()
	successes := 0
	failures := 0
	for index := 0; index < 2; index++ {
		if err := awaitResult(t, results); err != nil {
			failures++
		} else {
			successes++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent results = %d success, %d failure; want one each", successes, failures)
	}
}

func awaitResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent database operation")
		return nil
	}
}
