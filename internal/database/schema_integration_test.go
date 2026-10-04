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
		expectExecError(t, pool, `DELETE FROM system_config WHERE id=1`)
		expectExecError(t, pool, `TRUNCATE system_config`)
		expectExecError(t, pool, `DELETE FROM maintenance_state WHERE id=1`)
		if _, err := pool.Exec(ctx, `UPDATE change_feed_state SET last_position=5 WHERE id=1`); err != nil {
			t.Fatalf("advance change feed position: %v", err)
		}
		expectExecError(t, pool, `UPDATE change_feed_state SET last_position=4 WHERE id=1`)
		expectExecError(t, pool, `DELETE FROM change_feed_state WHERE id=1`)
		expectExecError(t, pool, `TRUNCATE change_feed_state`)
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
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'image/jpeg',1,1,NULL)`, newUUIDv4(t), mediaTwo, strings.Repeat("b", 64), "originals/bb/dimensions/original.jpg")
		expectExecError(t, pool, `UPDATE originals SET media_id=$1 WHERE media_id=$2`, mediaTwo, mediaOne)
		expectExecError(t, pool, `DELETE FROM originals WHERE media_id=$1`, mediaOne)
		expectExecError(t, pool, `TRUNCATE originals CASCADE`)

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
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='retired' WHERE id=$1`, profileThree); err != nil {
			t.Fatalf("retire v3 before shadow regression: %v", err)
		}
		expectExecError(t, pool, `DELETE FROM profiles WHERE id=$1`, profileThree)
		expectExecError(t, pool, `TRUNCATE profiles CASCADE`)

		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire temp-shadow connection: %v", err)
		}
		defer conn.Release()
		var schemaName string
		if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&schemaName); err != nil {
			t.Fatalf("read isolated schema name: %v", err)
		}
		if _, err := conn.Exec(ctx, `CREATE TEMP TABLE profiles (id uuid, key text, version integer, status text, activated_at timestamptz)`); err != nil {
			t.Fatalf("create shadow profiles table: %v", err)
		}
		realProfiles := pgx.Identifier{schemaName, "profiles"}.Sanitize()
		if _, err := conn.Exec(ctx, `UPDATE `+realProfiles+` SET status='active' WHERE id=$1`, profileTwo); err == nil {
			t.Fatal("temporary profiles shadow bypassed monotonic activation")
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
		expectExecError(t, pool, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, newUUIDv4(t), mediaID)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled', finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, purgeID); err != nil {
			t.Fatalf("cancel purge: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, newUUIDv4(t), mediaID); err != nil {
			t.Fatalf("new purge after cancellation: %v", err)
		}

		lifecycleJobID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, lifecycleJobID, otherMediaID); err != nil {
			t.Fatalf("insert lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,attempts,started_at,finished_at) VALUES ($1,'purge',$2,'succeeded',3,1,now(),now())`, newUUIDv4(t), otherMediaID)
		leaseOne := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, lifecycleJobID, leaseOne); err != nil {
			t.Fatalf("claim lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='cancelled', started_at=NULL, lease_token=NULL, lease_expires_at=NULL, finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, lifecycleJobID)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='queued', lease_token=NULL, lease_expires_at=NULL WHERE id=$1`, lifecycleJobID); err != nil {
			t.Fatalf("backoff lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='cancelled', finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `UPDATE jobs SET attempts=0 WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `UPDATE jobs SET max_attempts=2 WHERE id=$1`, lifecycleJobID)
		leaseTwo := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=2, lease_token=$2, lease_expires_at=now()+interval '1 minute' WHERE id=$1`, lifecycleJobID, leaseTwo); err != nil {
			t.Fatalf("reclaim lifecycle purge: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='succeeded', lease_token=NULL, lease_expires_at=NULL, finished_at=now() WHERE id=$1`, lifecycleJobID); err != nil {
			t.Fatalf("finish lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='queued', finished_at=NULL WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `DELETE FROM jobs WHERE id=$1`, lifecycleJobID)

		jobID := newUUIDv4(t)
		targetID := newUUIDv4(t)
		renditionID := newUUIDv4(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin publication: %v", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, jobID, newUUIDv4(t))
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height) VALUES ($1,$2,$3,'ignored',true,$4,'image/avif',10,$5,1,1)`, renditionID, mediaID, targetID, "renditions/cc/one/target/output.avif", strings.Repeat("e", 64))
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE jobs SET status='succeeded', lease_token=NULL, lease_expires_at=NULL, finished_at=now() WHERE id=$1`, jobID)
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
		expectExecError(t, pool, `UPDATE job_targets SET status='failed', error_code='late', error_message='late edit' WHERE id=$1`, targetID)
		expectExecError(t, pool, `DELETE FROM job_targets WHERE id=$1`, targetID)

		dimensionTarget := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, dimensionTarget); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height) VALUES ($1,$2,$3,'ignored',false,$4,'image/avif',1,$5,1,NULL)`,
				newUUIDv4(t), mediaID, dimensionTarget, "renditions/cc/one/dimension/output.avif", strings.Repeat("7", 64))
			return err
		})

		secondProfileID := insertDraftProfile(t, pool, "thumbnail", 1)
		deleteJobID := newUUIDv4(t)
		deleteTargets := []string{newUUIDv4(t), newUUIDv4(t)}
		deleteTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin delete-race fixture: %v", err)
		}
		if _, err = deleteTx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, deleteJobID, originalID, mediaID); err == nil {
			_, err = deleteTx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$3,$4,'pending'),($2,$3,$5,'pending')`, deleteTargets[0], deleteTargets[1], deleteJobID, profileID, secondProfileID)
		}
		if err != nil {
			_ = deleteTx.Rollback(ctx)
			t.Fatalf("create delete-race fixture: %v", err)
		}
		if err := deleteTx.Commit(ctx); err != nil {
			t.Fatalf("commit delete-race fixture: %v", err)
		}
		deleteStart := make(chan struct{})
		deleteResults := make(chan error, 2)
		for _, deleteTarget := range deleteTargets {
			go func(target string) {
				<-deleteStart
				_, err := pool.Exec(ctx, `DELETE FROM job_targets WHERE id=$1`, target)
				deleteResults <- err
			}(deleteTarget)
		}
		close(deleteStart)
		assertConcurrentFailures(t, deleteResults, 2)
		expectExecError(t, pool, `DELETE FROM jobs WHERE id=$1`, deleteJobID)
		expectExecError(t, pool, `UPDATE jobs SET original_id=NULL WHERE id=$1`, deleteJobID)
		expectExecError(t, pool, `TRUNCATE job_targets CASCADE`)
		expectExecError(t, pool, `TRUNCATE jobs CASCADE`)

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
		expectExecError(t, pool, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,2,'media_upsert','upload',$2,NULL)`, newUUIDv4(t), mediaID)
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
		type indexExpectation struct {
			name      string
			columns   string
			predicate []string
		}
		required := []indexExpectation{
			{name: "media_list_order_idx", columns: "(taken_at DESC NULLS LAST, id DESC)"},
			{name: "media_due_purge_idx", columns: "(purge_after, id)", predicate: []string{"deleted_at IS NOT NULL", "purge_after IS NOT NULL"}},
			{name: "profiles_one_active_key_idx", columns: "(key)", predicate: []string{"status = 'active'::text"}},
			{name: "jobs_dequeue_idx", columns: "(available_at, created_at, id)", predicate: []string{"status = 'queued'::text"}},
			{name: "jobs_list_idx", columns: "(created_at DESC, id DESC)"},
			{name: "jobs_status_list_idx", columns: "(status, created_at DESC, id DESC)"},
			{name: "jobs_media_list_idx", columns: "(media_id_snapshot, created_at DESC, id DESC)"},
			{name: "jobs_one_active_purge_idx", columns: "(media_id_snapshot)", predicate: []string{"type = 'purge'::text", "status = ANY", "'queued'::text", "'running'::text", "'failed'::text"}},
			{name: "job_targets_job_id_idx", columns: "(job_id)"},
			{name: "job_targets_profile_id_idx", columns: "(profile_id)"},
			{name: "renditions_media_id_idx", columns: "(media_id)"},
			{name: "renditions_job_target_id_idx", columns: "(job_target_id)"},
			{name: "renditions_one_current_key_idx", columns: "(media_id, profile_key)", predicate: []string{"is_current"}},
			{name: "renditions_cleanup_idx", columns: "(purge_after, media_id, id)", predicate: []string{"NOT is_current", "purge_after IS NOT NULL"}},
			{name: "backup_runs_succeeded_idx", columns: "(finished_at DESC, id DESC)", predicate: []string{"status = 'succeeded'::text"}},
			{name: "admin_batches_resume_idx", columns: "(status, updated_at, id)"},
		}
		for _, expected := range required {
			var definition, predicate string
			if err := pool.QueryRow(ctx, `
				SELECT pg_get_indexdef(i.indexrelid), COALESCE(pg_get_expr(i.indpred, i.indrelid), '')
				FROM pg_index AS i
				JOIN pg_class AS c ON c.oid=i.indexrelid
				JOIN pg_namespace AS n ON n.oid=c.relnamespace
				WHERE n.nspname=current_schema() AND c.relname=$1`, expected.name).Scan(&definition, &predicate); err != nil {
				t.Errorf("required index %s: %v", expected.name, err)
				continue
			}
			if !strings.Contains(definition, expected.columns) {
				t.Errorf("index %s definition = %q, want columns/order %q", expected.name, definition, expected.columns)
			}
			for _, fragment := range expected.predicate {
				if !strings.Contains(predicate, fragment) {
					t.Errorf("index %s predicate = %q, want fragment %q", expected.name, predicate, fragment)
				}
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
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, jobID, newUUIDv4(t)); err != nil {
		t.Fatalf("claim transform fixture: %v", err)
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

func assertConcurrentFailures(t *testing.T, results <-chan error, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if err := awaitResult(t, results); err == nil {
			t.Fatalf("concurrent operation %d unexpectedly succeeded", index)
		}
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
