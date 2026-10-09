package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func TestMigratorIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	t.Run("status is read-only and Up initializes history", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrator, err := NewMigrator(pool)
		if err != nil {
			t.Fatalf("NewMigrator() error = %v", err)
		}

		status, err := migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if status.CurrentVersion != 0 || status.ExpectedVersion != 17 || status.Ready() || !status.Pending {
			t.Fatalf("Status() = %+v, want pending version sixteen", status)
		}
		var historyExists bool
		if err := pool.QueryRow(context.Background(), `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&historyExists); err != nil {
			t.Fatalf("check history table after Status: %v", err)
		}
		if historyExists {
			t.Fatal("Status() created schema_migrations")
		}

		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&historyExists); err != nil {
			t.Fatalf("check history table after Up: %v", err)
		}
		if !historyExists {
			t.Fatal("Up() did not create schema_migrations")
		}
		status, err = migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() after Up error = %v", err)
		}
		if status.CurrentVersion != 17 || status.ExpectedVersion != 17 || !status.Ready() {
			t.Fatalf("Status() after Up = %+v, want ready version sixteen", status)
		}
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("second Up() error = %v", err)
		}
	})

	t.Run("reconciliation upgrade rejects duplicate legacy transform ownership atomically", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:16]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one through sixteen: %v", err)
		}
		profileID := insertDraftProfile(t, pool, "legacy-duplicate-owner", 1)
		jobIDs := make([]string, 0, 2)
		for index := 0; index < 2; index++ {
			mediaID := newUUIDv4(t)
			insertMedia(t, pool, mediaID)
			originalID := insertOriginal(t, pool, mediaID, fmt.Sprintf("%x", index+4), newUUIDv4(t))
			targetID := insertPendingTransform(t, pool, mediaID, originalID, profileID)
			var jobID string
			if err := pool.QueryRow(context.Background(), `SELECT job_id::text FROM job_targets WHERE id=$1`, targetID).Scan(&jobID); err != nil {
				t.Fatal(err)
			}
			jobIDs = append(jobIDs, jobID)
		}
		duplicateLease := newUUIDv4(t)
		for _, jobID := range jobIDs {
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_token=$2 WHERE id=$1`, jobID, duplicateLease); err != nil {
				t.Fatalf("stage duplicate v16 transform owner: %v", err)
			}
		}
		if err := full.Up(context.Background()); err == nil {
			t.Fatal("migration accepted duplicate legacy transform ownership")
		}
		var version int64
		var storageTablesAbsent bool
		if err := pool.QueryRow(context.Background(), `SELECT
			(SELECT max(version) FROM schema_migrations WHERE NOT dirty),
			to_regclass('storage_attempts') IS NULL AND to_regclass('reconciliation_check_reports') IS NULL`).Scan(&version, &storageTablesAbsent); err != nil {
			t.Fatal(err)
		}
		if version != 16 || !storageTablesAbsent {
			t.Fatalf("failed upgrade rollback version=%d tables_absent=%t", version, storageTablesAbsent)
		}
	})

	t.Run("reconciliation cutover waits for v16 transform writer and backfills its commit", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:16]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one through sixteen: %v", err)
		}
		mediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		originalID := insertOriginal(t, pool, mediaID, "6", newUUIDv4(t))
		profileID := insertDraftProfile(t, pool, "legacy-cutover-writer", 1)
		targetID := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		var jobID string
		if err := pool.QueryRow(context.Background(), `SELECT job_id::text FROM job_targets WHERE id=$1`, targetID).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		writer, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback(context.Background())
		cutoverLease := newUUIDv4(t)
		if _, err := writer.Exec(context.Background(), `UPDATE jobs SET lease_token=$2 WHERE id=$1`, jobID, cutoverLease); err != nil {
			t.Fatalf("stage v16 lease writer: %v", err)
		}
		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(context.Background()) }()
		select {
		case err := <-migrationResult:
			t.Fatalf("migration did not wait for v16 Jobs writer: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := writer.Commit(context.Background()); err != nil {
			t.Fatalf("commit v16 Jobs writer: %v", err)
		}
		if err := awaitResult(t, migrationResult); err != nil {
			t.Fatalf("migration after v16 Jobs writer: %v", err)
		}
		var coverage string
		if err := pool.QueryRow(context.Background(), `SELECT coverage FROM storage_attempts WHERE id=$1 AND job_id=$2`, cutoverLease, jobID).Scan(&coverage); err != nil || coverage != "legacy_active" {
			t.Fatalf("cutover committed lease coverage=%q error=%v", coverage, err)
		}
	})

	t.Run("reconciliation evidence upgrade preserves legacy as non executable", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:16]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one through sixteen: %v", err)
		}
		legacyID := newUUIDv4(t)
		if _, err := pool.Exec(context.Background(), `INSERT INTO reconciliation_reports
			(id,scope,findings,repair_disposition) VALUES ($1,'all','[]','{}')`, legacyID); err != nil {
			t.Fatalf("insert v16 legacy report: %v", err)
		}
		mediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		originalID := insertOriginal(t, pool, mediaID, "d", newUUIDv4(t))
		profileID := insertDraftProfile(t, pool, "legacy-evidence-upgrade", 1)
		targetID := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		var jobID, legacyLease string
		if err := pool.QueryRow(context.Background(), `SELECT j.id::text,j.lease_token::text FROM jobs AS j JOIN job_targets AS t ON t.job_id=j.id WHERE t.id=$1`, targetID).Scan(&jobID, &legacyLease); err != nil {
			t.Fatalf("read v16 running transform: %v", err)
		}
		if err := full.Up(context.Background()); err != nil {
			t.Fatalf("apply reconciliation evidence boundary: %v", err)
		}
		var version int64
		var format string
		if err := pool.QueryRow(context.Background(), `SELECT
			(SELECT max(version) FROM schema_migrations WHERE NOT dirty),
			(SELECT format FROM reconciliation_reports WHERE id=$1)`, legacyID).Scan(&version, &format); err != nil {
			t.Fatal(err)
		}
		if version != 17 || format != "legacy" {
			t.Fatalf("upgrade version/legacy format=%d/%q, want 17/legacy", version, format)
		}
		expectExecError(t, pool, `INSERT INTO reconciliation_reports
			(id,scope,findings,repair_disposition) VALUES ($1,'all','[]','{}')`, newUUIDv4(t))
		expectExecError(t, pool, `UPDATE reconciliation_reports SET scope='files' WHERE id=$1`, legacyID)
		expectExecError(t, pool, `DELETE FROM reconciliation_reports WHERE id=$1`, legacyID)
		expectExecError(t, pool, `TRUNCATE reconciliation_reports`)
		expectExecError(t, pool, `SELECT nmcp_begin_repair_run($1,$2,$3,'legacy-probe')`, newUUIDv4(t), legacyID, newUUIDv4(t))
		var coverage string
		var history []string
		if err := pool.QueryRow(context.Background(), `SELECT a.coverage,array_agg(e.event_type ORDER BY e.sequence)
			FROM storage_attempts AS a JOIN storage_attempt_events AS e ON e.attempt_id=a.id
			WHERE a.id=$1 AND a.job_id=$2 GROUP BY a.coverage`, legacyLease, jobID).Scan(&coverage, &history); err != nil {
			t.Fatalf("read legacy-active transform backfill: %v", err)
		}
		if coverage != "legacy_active" || fmt.Sprint(history) != "[claimed]" {
			t.Fatalf("legacy-active transform coverage/history=%q/%v", coverage, history)
		}
	})

	t.Run("rendition cleanup boundary upgrade drains legacy state", func(t *testing.T) {
		prepareVersionFifteen := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:15]).Up(context.Background()); err != nil {
				t.Fatalf("apply versions one through fifteen: %v", err)
			}
			return pool, full
		}
		insertLegacyCleanup := func(t *testing.T, pool *pgxpool.Pool, disposition string) (string, string, string) {
			t.Helper()
			ctx := context.Background()
			mediaID := newUUIDv4(t)
			insertMedia(t, pool, mediaID)
			originalID := insertOriginal(t, pool, mediaID, "7", "v16-cutover-"+mediaID)
			oldTargetID := insertPendingTransform(t, pool, mediaID, originalID, profile.StandardV1ID)
			oldRenditionID := newUUIDv4(t)
			oldPath := "renditions/77/v16-cutover-" + oldRenditionID + "/output.avif"
			publish := func(targetID, renditionID, path string, current bool) {
				t.Helper()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID); err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO renditions
						(id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height,processor_audit)
						VALUES ($1,$2,$3,'ignored',$4,$5,'image/avif',7,$6,1,1,'{"fixture":"v16-cutover"}')`,
						renditionID, mediaID, targetID, current, path, strings.Repeat("7", 64))
				}
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp()
						WHERE id=(SELECT job_id FROM job_targets WHERE id=$1)`, targetID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			publish(oldTargetID, oldRenditionID, oldPath, true)
			if _, err := pool.Exec(ctx, `UPDATE renditions SET is_current=false,purge_after=clock_timestamp()-interval '1 second' WHERE id=$1`, oldRenditionID); err != nil {
				t.Fatal(err)
			}
			currentTargetID := insertPendingTransform(t, pool, mediaID, originalID, profile.StandardV1ID)
			publish(currentTargetID, newUUIDv4(t), "renditions/77/v16-current-"+currentTargetID+"/output.avif", true)

			cleanupID := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO rendition_cleanup_progress
				(id,media_id_snapshot,rendition_id,job_target_id,relative_path,size_bytes,purge_after)
				SELECT $1,media_id,id,job_target_id,relative_path,size_bytes,purge_after FROM renditions WHERE id=$2`, cleanupID, oldRenditionID); err != nil {
				t.Fatal(err)
			}
			if disposition != "pending" {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.rendition_cleanup_progress_id',$1,true)`, cleanupID); err == nil {
					_, err = tx.Exec(ctx, `UPDATE rendition_cleanup_progress SET disposition=$2 WHERE id=$1`, cleanupID, disposition)
				}
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			return mediaID, oldRenditionID, cleanupID
		}

		t.Run("in-flight guarded delete completes before cutover", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool, full := prepareVersionFifteen(t)
			mediaID, renditionID, cleanupID := insertLegacyCleanup(t, pool, "missing")
			barrier, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Release()
			const barrierKey = int64(0x4e4d435030303136) // "NMCP0016"
			if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, barrierKey); err != nil {
				t.Fatal(err)
			}
			defer barrier.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey)

			migrations := append([]migration(nil), full.migrations[:16]...)
			const lockStatement = "LOCK TABLE rendition_cleanup_progress IN SHARE ROW EXCLUSIVE MODE;"
			instrumentedSQL := strings.Replace(migrations[15].sql, lockStatement,
				lockStatement+fmt.Sprintf("\nSELECT pg_catalog.pg_advisory_xact_lock(%d);", barrierKey), 1)
			if instrumentedSQL == migrations[15].sql {
				t.Fatal("version sixteen progress lock was not found for instrumentation")
			}
			migrations[15].sql = instrumentedSQL
			migrations[15].checksum = checksumSQL([]byte(instrumentedSQL))
			migrationResult := make(chan error, 1)
			go func() { migrationResult <- newMigrator(pool, migrations).Up(ctx) }()
			awaitRelationLock(t, pool, ctx, 0, "rendition_cleanup_progress", "ShareRowExclusiveLock", true)

			deleteResult := make(chan error, 1)
			go func() {
				tx, err := pool.Begin(ctx)
				if err == nil {
					_, err = tx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.rendition_cleanup_progress_id',$1,true)`, cleanupID)
				}
				if err == nil {
					_, err = tx.Exec(ctx, `DELETE FROM renditions WHERE id=$1 AND media_id=$2`, renditionID, mediaID)
				}
				if err == nil {
					err = tx.Commit(ctx)
				} else if tx != nil {
					_ = tx.Rollback(context.Background())
				}
				deleteResult <- err
			}()
			if err := awaitContextResult(t, ctx, deleteResult); err != nil {
				t.Fatalf("legacy guarded delete while cutover held progress lock: %v", err)
			}
			if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey); err != nil {
				t.Fatal(err)
			}
			if err := awaitContextResult(t, ctx, migrationResult); err != nil {
				t.Fatalf("version sixteen after legacy delete: %v", err)
			}
			var version int64
			var renditionRows int
			if err := pool.QueryRow(ctx, `SELECT max(version),(SELECT count(*) FROM renditions WHERE id=$1) FROM schema_migrations WHERE NOT dirty`, renditionID).Scan(&version, &renditionRows); err != nil {
				t.Fatal(err)
			}
			if version != 16 || renditionRows != 0 {
				t.Fatalf("cutover result version=%d rendition_rows=%d", version, renditionRows)
			}
			var ownerAfter, configAfter string
			var securityDefiner, publicRevoked bool
			if err := pool.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(proowner),prosecdef,COALESCE(array_to_string(proconfig,','),''),
				NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(proacl,acldefault('f',proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')
				FROM pg_catalog.pg_proc WHERE oid='nmcp_check_target_rendition()'::regprocedure`).Scan(&ownerAfter, &securityDefiner, &configAfter, &publicRevoked); err != nil {
				t.Fatal(err)
			}
			if ownerAfter != "nmcp_purge_function_owner" || !securityDefiner || !strings.Contains(configAfter, "search_path=") || !publicRevoked {
				t.Fatalf("cardinality function owner=%q security_definer=%t config=%q public_revoked=%t", ownerAfter, securityDefiner, configAfter, publicRevoked)
			}
		})

		for _, disposition := range []string{"pending", "missing"} {
			t.Run("rejects and rolls back "+disposition+" legacy progress", func(t *testing.T) {
				pool, full := prepareVersionFifteen(t)
				_, renditionID, cleanupID := insertLegacyCleanup(t, pool, disposition)
				var functionBefore string
				if err := pool.QueryRow(context.Background(), `SELECT pg_catalog.pg_get_functiondef('nmcp_check_target_rendition()'::regprocedure)`).Scan(&functionBefore); err != nil {
					t.Fatal(err)
				}
				if err := full.Up(context.Background()); err == nil {
					t.Fatalf("version sixteen accepted %s legacy progress", disposition)
				}
				var version int64
				var completionFunction, completionTrigger bool
				var functionAfter, storedDisposition string
				if err := pool.QueryRow(context.Background(), `SELECT max(version),
					to_regprocedure('nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)') IS NOT NULL,
					EXISTS (SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='rendition_cleanup_progress'::regclass AND tgname='rendition_cleanup_progress_completed'),
					pg_catalog.pg_get_functiondef('nmcp_check_target_rendition()'::regprocedure),
					(SELECT disposition FROM rendition_cleanup_progress WHERE id=$1)
					FROM schema_migrations WHERE NOT dirty`, cleanupID).Scan(&version, &completionFunction, &completionTrigger, &functionAfter, &storedDisposition); err != nil {
					t.Fatal(err)
				}
				var renditionRows int
				if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM renditions WHERE id=$1`, renditionID).Scan(&renditionRows); err != nil {
					t.Fatal(err)
				}
				if version != 15 || completionFunction || completionTrigger || functionAfter != functionBefore || storedDisposition != disposition || renditionRows != 1 {
					t.Fatalf("rollback disposition=%q version=%d function=%t trigger=%t cardinality_changed=%t stored=%q rendition_rows=%d",
						disposition, version, completionFunction, completionTrigger, functionAfter != functionBefore, storedDisposition, renditionRows)
				}
			})
		}
	})

	t.Run("physical purge tombstone upgrade rejects duplicate legacy history atomically", func(t *testing.T) {
		ctx := context.Background()
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:11]).Up(ctx); err != nil {
			t.Fatal(err)
		}
		mediaID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
			t.Fatal(err)
		}
		for position := 1; position <= 2; position++ {
			if _, err := pool.Exec(ctx, `INSERT INTO change_events (id,position,event_type,reason,media_id) VALUES ($1,$2,'media_purged','physical_purge',$3)`, newUUIDv4(t), position, mediaID); err != nil {
				t.Fatal(err)
			}
		}
		if err := full.Up(ctx); err == nil {
			t.Fatal("duplicate legacy tombstones accepted")
		}
		var version int
		var indexExists bool
		if err := pool.QueryRow(ctx, `SELECT max(version),to_regclass('change_events_one_physical_purge_per_media') IS NOT NULL FROM schema_migrations WHERE NOT dirty`).Scan(&version, &indexExists); err != nil {
			t.Fatal(err)
		}
		if version != 11 || indexExists {
			t.Fatalf("rollback version=%d index=%t", version, indexExists)
		}
	})

	t.Run("identity completion rejects noncanonical in-flight legacy tombstone atomically", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:12]).Up(ctx); err != nil {
			t.Fatal(err)
		}
		mediaID := newUUIDv4(t)
		writer, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(ctx, `INSERT INTO change_events (id,position,event_type,reason,media_id) VALUES ($1,1,'media_purged','physical_purge',$2)`, newUUIDv4(t), mediaID); err != nil {
			t.Fatal(err)
		}
		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(ctx) }()
		awaitRelationLock(t, pool, ctx, 0, "change_events", "ShareLock", false)
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-migrationResult; err == nil {
			t.Fatal("identity completion accepted noncanonical legacy tombstone")
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_purge_identity_guard WHERE media_id=$1`, mediaID).Scan(&state); err != nil || state != "purged" {
			t.Fatalf("cutover guard state=%q error=%v", state, err)
		}
		var version int
		var completionTrigger, completionColumn, completionFunction bool
		if err := pool.QueryRow(ctx, `SELECT max(version),
			EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='media_purge_identity_guard'::regclass AND tgname='media_identity_retirement_completed'),
			EXISTS(SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid='media_purge_identity_guard'::regclass AND attname='purge_job_id' AND NOT attisdropped),
			to_regprocedure('nmcp_require_completed_media_identity_retirement()') IS NOT NULL
			FROM schema_migrations WHERE NOT dirty`).Scan(&version, &completionTrigger, &completionColumn, &completionFunction); err != nil {
			t.Fatal(err)
		}
		if version != 14 || completionTrigger || completionColumn || completionFunction {
			t.Fatalf("failed identity completion migration version=%d trigger=%t column=%t function=%t", version, completionTrigger, completionColumn, completionFunction)
		}
	})

	t.Run("identity retirement cutover drains Media writer before guard alteration", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:13]).Up(ctx); err != nil {
			t.Fatal(err)
		}
		mediaID := newUUIDv4(t)
		writer, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
			t.Fatal(err)
		}
		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(ctx) }()
		awaitRelationLock(t, pool, ctx, 0, "media", "ShareLock", false)
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-migrationResult; err != nil {
			t.Fatal(err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_purge_identity_guard WHERE media_id=$1`, mediaID).Scan(&state); err != nil || state != "live" {
			t.Fatalf("cutover Media guard state=%q error=%v", state, err)
		}
	})

	t.Run("purge progress boundary upgrades version eight after prior writer drains", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:8]).Up(ctx); err != nil {
			t.Fatalf("apply versions one through eight: %v", err)
		}
		mediaID, originalID, jobID, token := newUUIDv4(t), newUUIDv4(t), newUUIDv4(t), newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after) VALUES ($1,'image/jpeg','unknown',clock_timestamp(),clock_timestamp())`, mediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`, originalID, mediaID, strings.Repeat("e", 64), "originals/ee/v9-upgrade/original.jpg"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, jobID, mediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, jobID, token); err != nil {
			t.Fatal(err)
		}
		manifestTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = manifestTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, token); err == nil {
			_, err = manifestTx.Exec(ctx, `INSERT INTO purge_file_progress (job_id,media_id_snapshot,object_kind,object_id,relative_path,size_bytes) SELECT $1,$2,'original',id,relative_path,size_bytes FROM originals WHERE id=$3`, jobID, mediaID, originalID)
		}
		if err != nil {
			_ = manifestTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := manifestTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(ctx, `CREATE FUNCTION nmcp_v9_conflict() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `CREATE TRIGGER purge_file_progress_00_ordered_boundary BEFORE UPDATE ON purge_file_progress FOR EACH ROW EXECUTE FUNCTION nmcp_v9_conflict()`); err != nil {
			t.Fatal(err)
		}
		if err := full.Up(ctx); err == nil {
			t.Fatal("version nine accepted conflicting boundary trigger")
		}
		var version int64
		var completionExists bool
		if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT to_regprocedure('nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)') IS NOT NULL`).Scan(&completionExists); err != nil {
			t.Fatal(err)
		}
		if version != 8 || completionExists {
			t.Fatalf("failed v9 migration version=%d completion_exists=%t", version, completionExists)
		}
		if _, err := pool.Exec(ctx, `DROP TRIGGER purge_file_progress_00_ordered_boundary ON purge_file_progress`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DROP FUNCTION nmcp_v9_conflict()`); err != nil {
			t.Fatal(err)
		}

		writer, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback(ctx)
		if _, err := writer.Exec(ctx, `SELECT 1 FROM purge_file_progress WHERE job_id=$1 FOR UPDATE`, jobID); err != nil {
			t.Fatal(err)
		}
		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(ctx) }()
		awaitRelationLock(t, pool, ctx, 0, "purge_file_progress", "AccessExclusiveLock", false)
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-migrationResult; err != nil {
			t.Fatalf("upgrade versions nine through fifteen: %v", err)
		}

		var securityDefiner, ownerNoLogin, fixedSearchPath, workerExecute, publicRevoked, workerSelect, workerInsert, workerNoUpdate bool
		if err := pool.QueryRow(ctx, `SELECT p.prosecdef,
			p.proowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='nmcp_purge_function_owner'),
			NOT owner_role.rolcanlogin,
			array_to_string(p.proconfig,',') LIKE 'search_path='||current_schema()||', pg_catalog, pg_temp%',
			has_function_privilege('nmcp_worker_runtime',p.oid,'EXECUTE'),
			NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE'),
			has_table_privilege('nmcp_worker_runtime','purge_file_progress','SELECT'),
			has_table_privilege('nmcp_worker_runtime','purge_file_progress','INSERT'),
			NOT has_table_privilege('nmcp_worker_runtime','purge_file_progress','UPDATE')
			FROM pg_catalog.pg_proc AS p JOIN pg_catalog.pg_roles AS owner_role ON owner_role.oid=p.proowner
			WHERE p.oid='nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)'::regprocedure`).Scan(
			&securityDefiner, &completionExists, &ownerNoLogin, &fixedSearchPath, &workerExecute, &publicRevoked, &workerSelect, &workerInsert, &workerNoUpdate); err != nil {
			t.Fatal(err)
		}
		if !securityDefiner || !completionExists || !ownerNoLogin || !fixedSearchPath || !workerExecute || !publicRevoked || !workerSelect || !workerInsert || !workerNoUpdate {
			t.Fatalf("v10 boundary security=%t owner=%t no_login=%t search_path=%t execute=%t public_revoked=%t select=%t insert=%t no_update=%t",
				securityDefiner, completionExists, ownerNoLogin, fixedSearchPath, workerExecute, publicRevoked, workerSelect, workerInsert, workerNoUpdate)
		}
		if _, err := pool.Exec(ctx, `UPDATE purge_file_progress SET disposition='deleted' WHERE job_id=$1`, jobID); err == nil {
			t.Fatal("migration/table owner bypassed ordered purge boundary")
		}
		var runtimeHistorySelect, runtimeHistoryNoWrite, workerHistorySelect, workerHistoryNoWrite bool
		if err := pool.QueryRow(ctx, `SELECT
			has_table_privilege('nmcp_runtime','schema_migrations','SELECT'),
			NOT (has_table_privilege('nmcp_runtime','schema_migrations','INSERT') OR has_table_privilege('nmcp_runtime','schema_migrations','UPDATE') OR has_table_privilege('nmcp_runtime','schema_migrations','DELETE') OR has_table_privilege('nmcp_runtime','schema_migrations','TRUNCATE')),
			has_table_privilege('nmcp_worker_runtime','schema_migrations','SELECT'),
			NOT (has_table_privilege('nmcp_worker_runtime','schema_migrations','INSERT') OR has_table_privilege('nmcp_worker_runtime','schema_migrations','UPDATE') OR has_table_privilege('nmcp_worker_runtime','schema_migrations','DELETE') OR has_table_privilege('nmcp_worker_runtime','schema_migrations','TRUNCATE'))`).Scan(
			&runtimeHistorySelect, &runtimeHistoryNoWrite, &workerHistorySelect, &workerHistoryNoWrite); err != nil {
			t.Fatal(err)
		}
		if !runtimeHistorySelect || !runtimeHistoryNoWrite || !workerHistorySelect || !workerHistoryNoWrite {
			t.Fatalf("migration history ACL runtime=(%t,%t) worker=(%t,%t)", runtimeHistorySelect, runtimeHistoryNoWrite, workerHistorySelect, workerHistoryNoWrite)
		}
		var schemaName string
		if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schemaName); err != nil {
			t.Fatal(err)
		}
		for _, roleName := range []string{"nmcp_runtime", "nmcp_worker_runtime"} {
			rolePool := integrationRolePool(t, databaseURL, schemaName, roleName)
			roleMigrator, err := NewMigrator(rolePool)
			if err != nil {
				t.Fatal(err)
			}
			status, err := roleMigrator.Status(ctx)
			if err != nil || !status.Ready() || status.CurrentVersion != 17 {
				t.Fatalf("%s migration status = %+v, %v", roleName, status, err)
			}
			if err := roleMigrator.Up(ctx); err == nil || !strings.Contains(err.Error(), "lacks CREATE privilege") {
				t.Fatalf("%s migration Up error = %v", roleName, err)
			}
			if _, err := rolePool.Exec(ctx, `UPDATE schema_migrations SET dirty=dirty WHERE false`); err == nil {
				t.Fatalf("%s wrote migration history", roleName)
			}
		}
	})

	t.Run("purge cleanup invariant upgrade rejects invalid live history atomically", func(t *testing.T) {
		ctx := context.Background()
		prepareVersionSeven := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:7]).Up(ctx); err != nil {
				t.Fatalf("apply versions one through seven: %v", err)
			}
			return pool, full
		}
		insertInvalidTarget := func(t *testing.T, pool *pgxpool.Pool) string {
			t.Helper()
			mediaID, originalID, jobID, targetID := newUUIDv4(t), newUUIDv4(t), newUUIDv4(t), newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`, originalID, mediaID, strings.Repeat("a", 64), "originals/aa/v8-preflight/original.jpg"); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profile.StandardV1ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `ALTER TABLE job_targets DISABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `ALTER TABLE job_targets ENABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			return targetID
		}
		assertRollback := func(t *testing.T, pool *pgxpool.Pool) {
			t.Helper()
			var version int64
			var progressTable, guardFunction bool
			if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT to_regclass('purge_file_progress') IS NOT NULL, to_regprocedure('nmcp_guard_media_purge_delete()') IS NOT NULL`).Scan(&progressTable, &guardFunction); err != nil {
				t.Fatal(err)
			}
			if version != 7 || progressTable || guardFunction {
				t.Fatalf("failed migration state version=%d progress=%t guard=%t", version, progressTable, guardFunction)
			}
		}

		pool, migrator := prepareVersionSeven(t)
		insertInvalidTarget(t, pool)
		if err := migrator.Up(ctx); err == nil {
			t.Fatal("migration accepted succeeded live Target without Rendition")
		}
		assertRollback(t, pool)

		t.Run("waits for earlier target writer before preflight", func(t *testing.T) {
			pool, migrator := prepareVersionSeven(t)
			mediaID, originalID, jobID, targetID := newUUIDv4(t), newUUIDv4(t), newUUIDv4(t), newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`, originalID, mediaID, strings.Repeat("b", 64), "originals/bb/v8-lock/original.jpg"); err != nil {
				t.Fatal(err)
			}
			setupTx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer setupTx.Rollback(ctx)
			if _, err := setupTx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := setupTx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profile.StandardV1ID); err != nil {
				t.Fatal(err)
			}
			if err := setupTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if _, err := writer.Exec(ctx, `ALTER TABLE job_targets DISABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `ALTER TABLE job_targets ENABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- migrator.Up(ctx) }()
			awaitRelationLock(t, pool, ctx, 0, "job_targets", "AccessExclusiveLock", false)
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := awaitContextResult(t, ctx, result); err == nil {
				t.Fatal("migration accepted invalid target committed by earlier writer")
			}
			assertRollback(t, pool)
		})
	})

	t.Run("lifecycle guard upgrade validates legacy state and lock order", func(t *testing.T) {
		ctx := context.Background()
		prepareVersionSix := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:6]).Up(ctx); err != nil {
				t.Fatalf("apply versions one through six: %v", err)
			}
			return pool, full
		}
		insertMedia := func(t *testing.T, pool *pgxpool.Pool, deleted bool) string {
			t.Helper()
			mediaID := newUUIDv4(t)
			if deleted {
				if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after)
					VALUES ($1,'image/jpeg','unknown',clock_timestamp(),clock_timestamp()+interval '1 day')`, mediaID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
				t.Fatal(err)
			}
			return mediaID
		}
		insertPurge := func(t *testing.T, pool *pgxpool.Pool, mediaID, status string) string {
			t.Helper()
			jobID := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, jobID, mediaID); err != nil {
				t.Fatal(err)
			}
			switch status {
			case "queued":
			case "cancelled":
				if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled',finished_at=clock_timestamp(),cancelled_at=clock_timestamp(),cancel_reason='media_restored' WHERE id=$1`, jobID); err != nil {
					t.Fatal(err)
				}
			case "running", "failed", "succeeded":
				if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, jobID, newUUIDv4(t)); err != nil {
					t.Fatal(err)
				}
				if status != "running" {
					if _, err := pool.Exec(ctx, `UPDATE jobs SET status=$2,lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, jobID, status); err != nil {
						t.Fatal(err)
					}
				}
			default:
				t.Fatalf("unsupported purge fixture status %q", status)
			}
			return jobID
		}
		assertVersionSixRollback := func(t *testing.T, pool *pgxpool.Pool, jobID, wantStatus string) {
			t.Helper()
			var version int64
			var status string
			var upperBound, purgeGuard bool
			if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid='system_config'::regclass
				  AND conname='system_config_deleted_media_retention_max_check'
			)`).Scan(&upperBound); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT to_regprocedure('nmcp_guard_purge_job_insert()') IS NOT NULL`).Scan(&purgeGuard); err != nil {
				t.Fatal(err)
			}
			if version != 6 || status != wantStatus || upperBound || purgeGuard {
				t.Fatalf("failed lifecycle migration state: version=%d status=%q upper_bound=%t purge_guard=%t", version, status, upperBound, purgeGuard)
			}
		}

		validPool, validMigrator := prepareVersionSix(t)
		if _, err := validPool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=36500 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		insertPurge(t, validPool, insertMedia(t, validPool, true), "queued")
		cancelledID := insertPurge(t, validPool, insertMedia(t, validPool, false), "cancelled")
		succeededID := insertPurge(t, validPool, newUUIDv4(t), "succeeded")
		if err := validMigrator.Up(ctx); err != nil {
			t.Fatalf("upgrade valid retention, unfinished work, and terminal history: %v", err)
		}
		var terminalCount int
		if err := validPool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE (id=$1 AND status='cancelled') OR (id=$2 AND status='succeeded')`, cancelledID, succeededID).Scan(&terminalCount); err != nil || terminalCount != 2 {
			t.Fatalf("terminal purge history after upgrade = %d, err=%v", terminalCount, err)
		}

		invalidPool, invalidMigrator := prepareVersionSix(t)
		if _, err := invalidPool.Exec(ctx, `UPDATE system_config SET deleted_media_retention_days=36501 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if err := invalidMigrator.Up(ctx); err == nil {
			t.Fatal("migration accepted retention above the lifecycle maximum")
		}
		var version int64
		var retention int
		var upperBound, purgeGuard bool
		if err := invalidPool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := invalidPool.QueryRow(ctx, `SELECT deleted_media_retention_days FROM system_config WHERE id=1`).Scan(&retention); err != nil {
			t.Fatal(err)
		}
		if err := invalidPool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid='system_config'::regclass
			  AND conname='system_config_deleted_media_retention_max_check'
		)`).Scan(&upperBound); err != nil {
			t.Fatal(err)
		}
		if err := invalidPool.QueryRow(ctx, `SELECT to_regprocedure('nmcp_guard_purge_job_insert()') IS NOT NULL`).Scan(&purgeGuard); err != nil {
			t.Fatal(err)
		}
		if version != 6 || retention != 36501 || upperBound || purgeGuard {
			t.Fatalf("failed lifecycle migration state: version=%d retention=%d upper_bound=%t purge_guard=%t", version, retention, upperBound, purgeGuard)
		}

		for _, mediaState := range []string{"active", "absent"} {
			for _, jobStatus := range []string{"queued", "running", "failed"} {
				t.Run("rejects "+mediaState+" Media for "+jobStatus+" purge", func(t *testing.T) {
					pool, migrator := prepareVersionSix(t)
					mediaID := newUUIDv4(t)
					if mediaState == "active" {
						mediaID = insertMedia(t, pool, false)
					}
					jobID := insertPurge(t, pool, mediaID, jobStatus)
					err := migrator.Up(ctx)
					var databaseError *pgconn.PgError
					if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
						t.Fatalf("migration error = %#v, want SQLSTATE 23514", err)
					}
					assertVersionSixRollback(t, pool, jobID, jobStatus)
				})
			}
		}

		t.Run("Delete and migration converge without lock inversion", func(t *testing.T) {
			pool, migrator := prepareVersionSix(t)
			raceCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			mediaID := insertMedia(t, pool, false)
			originalID := newUUIDv4(t)
			if _, err := pool.Exec(raceCtx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
				VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, originalID, mediaID, strings.Repeat("b", 64), "originals/00/migration-delete-"+originalID+"/original.jpg"); err != nil {
				t.Fatal(err)
			}
			repository, err := medialifecycle.NewPostgresRepository(pool, "https://files.example/files")
			if err != nil {
				t.Fatal(err)
			}

			configBlocker, err := pool.Begin(raceCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer configBlocker.Rollback(context.Background())
			if _, err := configBlocker.Exec(raceCtx, `LOCK TABLE system_config IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			// Version six predates the narrow maintenance function used by the
			// current repository. Model the legacy Delete lock prefix explicitly;
			// the migration race under test is maintenance -> Media -> config.
			deleteResult := make(chan error, 1)
			go func() {
				tx, err := pool.Begin(raceCtx)
				if err == nil {
					_, err = tx.Exec(raceCtx, `SELECT 1 FROM maintenance_state WHERE id=1 AND mode='normal' FOR SHARE`)
				}
				if err == nil {
					_, err = tx.Exec(raceCtx, `SELECT 1 FROM media WHERE id=$1 FOR UPDATE`, mediaID)
				}
				if err == nil {
					_, err = tx.Exec(raceCtx, `SELECT 1 FROM system_config WHERE id=1 FOR SHARE`)
				}
				if err == nil {
					err = tx.Commit(raceCtx)
				} else if tx != nil {
					_ = tx.Rollback(context.Background())
				}
				deleteResult <- err
			}()
			// Waiting for config proves Delete acquired the maintenance prefix and
			// Media row before migration begins.
			awaitRelationLock(t, pool, raceCtx, 0, "system_config", "RowShareLock", false)
			awaitRelationLock(t, pool, raceCtx, 0, "maintenance_state", "RowShareLock", true)
			awaitRelationLock(t, pool, raceCtx, 0, "media", "RowShareLock", true)

			migrationResult := make(chan error, 1)
			go func() { migrationResult <- migrator.Up(raceCtx) }()
			awaitRelationLock(t, pool, raceCtx, 0, "maintenance_state", "AccessExclusiveLock", false)
			if err := configBlocker.Commit(raceCtx); err != nil {
				t.Fatal(err)
			}
			if err := awaitContextResult(t, raceCtx, deleteResult); err != nil {
				t.Fatalf("legacy Delete lock prefix after config release: %v", err)
			}
			if err := awaitContextResult(t, raceCtx, migrationResult); err != nil {
				t.Fatalf("migration after Delete: %v", err)
			}
			if _, err := repository.Delete(raceCtx, mediaID); err != nil {
				t.Fatalf("Delete after migration: %v", err)
			}
			var deleted bool
			var migratedVersion int64
			if err := pool.QueryRow(raceCtx, `SELECT deleted_at IS NOT NULL FROM media WHERE id=$1`, mediaID).Scan(&deleted); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(raceCtx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&migratedVersion); err != nil {
				t.Fatal(err)
			}
			if !deleted || migratedVersion != expectedVersion(migrator.migrations) {
				t.Fatalf("converged state deleted=%t version=%d", deleted, migratedVersion)
			}
		})
	})

	t.Run("profile dimension correction upgrades only untouched unreferenced bundled drafts", func(t *testing.T) {
		ctx := context.Background()
		prepareVersionFour := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:4]).Up(ctx); err != nil {
				t.Fatalf("apply versions one through four: %v", err)
			}
			return pool, full
		}
		assertRule := func(t *testing.T, pool *pgxpool.Pool, mimeType, want string) {
			t.Helper()
			var rules []string
			if err := pool.QueryRow(ctx, `SELECT array_agg(parameters->'recipes'->$1->>'dimension_rule' ORDER BY key) FROM profiles`, mimeType).Scan(&rules); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rules, []string{want, want}) {
				t.Fatalf("%s rules = %v, want %q", mimeType, rules, want)
			}
		}
		profileState := func(t *testing.T, pool *pgxpool.Pool) (string, string, string) {
			t.Helper()
			var payload, validator, trigger string
			if err := pool.QueryRow(ctx, `SELECT jsonb_agg(parameters ORDER BY key)::text FROM profiles`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('nmcp_valid_profile_recipe(jsonb)'::regprocedure)`).Scan(&validator); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger WHERE tgrelid='profiles'::regclass AND tgname='profiles_lifecycle'`).Scan(&trigger); err != nil {
				t.Fatal(err)
			}
			return payload, validator, trigger
		}
		insertTransformTarget := func(t *testing.T, tx pgx.Tx) string {
			t.Helper()
			mediaID, originalID, jobID, targetID := newUUIDv4(t), newUUIDv4(t), newUUIDv4(t), newUUIDv4(t)
			if _, err := tx.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/gif','unknown')`, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/gif',1)`, originalID, mediaID, strings.Repeat("d", 64), "originals/00/dimension-"+originalID+"/original.gif"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profile.StandardV1ID); err != nil {
				t.Fatal(err)
			}
			return targetID
		}

		validPool, validMigrator := prepareVersionFour(t)
		assertRule(t, validPool, "image/jpeg", "preserve-aspect-no-crop-no-upscale-even-round-down")
		if err := validMigrator.Up(ctx); err != nil {
			t.Fatalf("upgrade untouched bundled drafts: %v", err)
		}
		assertRule(t, validPool, "image/jpeg", "preserve-aspect-no-crop-no-upscale-round-nearest")
		assertRule(t, validPool, "image/gif", "preserve-aspect-no-crop-no-upscale-round-nearest")
		assertRule(t, validPool, "video/mp4", "preserve-aspect-no-crop-no-upscale-even-round-down")

		for name, damage := range map[string]func(*testing.T, *pgxpool.Pool){
			"divergent": func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER; UPDATE profiles SET parameters=jsonb_set(parameters,'{evidence_status}','"changed"') WHERE key='standard'; ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			},
			"active": func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER; UPDATE profiles SET status='active',activated_at=clock_timestamp() WHERE key='standard'; ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			},
			"missing": func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER; DELETE FROM profiles WHERE key='standard'; ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			},
			"unexpected custom": func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(ctx, `
					INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
					SELECT $1,'custom',1,'draft',input_mime_types,processor,parameters_schema_version,parameters
					FROM profiles WHERE key='standard'`, newUUIDv4(t)); err != nil {
					t.Fatal(err)
				}
			},
			"admin batch referenced": func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(ctx, `INSERT INTO admin_batches (id,identity_key,operation,status,profile_id,config_snapshot,high_water,checkpoint) VALUES ($1,$2,'regenerate','running',$3,'{}','{}','{}')`, newUUIDv4(t), "dimension-migration-reference-"+newUUIDv4(t), profile.StandardV1ID); err != nil {
					t.Fatal(err)
				}
			},
			"job target referenced": func(t *testing.T, pool *pgxpool.Pool) {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				insertTransformTarget(t, tx)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			},
		} {
			t.Run(name, func(t *testing.T) {
				pool, migrator := prepareVersionFour(t)
				damage(t, pool)
				payload, validator, trigger := profileState(t, pool)
				if err := migrator.Up(ctx); err == nil {
					t.Fatalf("migration accepted %s bundled profile", name)
				}
				status, err := migrator.Status(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if status.CurrentVersion != 4 || !status.Pending {
					t.Fatalf("failed migration status = %+v", status)
				}
				gotPayload, gotValidator, gotTrigger := profileState(t, pool)
				if gotPayload != payload || gotValidator != validator || gotTrigger != trigger {
					t.Fatalf("failed migration changed state: payload=%v validator=%v trigger=%q", gotPayload != payload, gotValidator != validator, gotTrigger)
				}
			})
		}

		t.Run("update failure rolls back validator payload and trigger", func(t *testing.T) {
			pool, migrator := prepareVersionFour(t)
			payload, validator, trigger := profileState(t, pool)
			if _, err := pool.Exec(ctx, `
				CREATE FUNCTION reject_dimension_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected deferred failure'; END $$;
				CREATE CONSTRAINT TRIGGER reject_dimension_update AFTER UPDATE ON profiles
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_dimension_update()`); err != nil {
				t.Fatal(err)
			}
			if err := migrator.Up(ctx); err == nil {
				t.Fatal("migration accepted injected profile update failure")
			}
			gotPayload, gotValidator, gotTrigger := profileState(t, pool)
			if gotPayload != payload || gotValidator != validator || gotTrigger != trigger || gotTrigger != "O" {
				t.Fatalf("rollback changed state: payload=%v validator=%v trigger=%q", gotPayload != payload, gotValidator != validator, gotTrigger)
			}
		})

		t.Run("waits for earlier activation writer before preflight", func(t *testing.T) {
			pool, migrator := prepareVersionFour(t)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER; UPDATE profiles SET status='active',activated_at=clock_timestamp() WHERE key='standard'; ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- migrator.Up(ctx) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation='profiles'::regclass AND mode='AccessExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("migration did not wait for earlier profile writer")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil {
				t.Fatal("migration accepted profile activated by earlier writer")
			}
		})

		t.Run("waits for earlier job target FK writer before preflight", func(t *testing.T) {
			pool, migrator := prepareVersionFour(t)
			payload, validator, trigger := profileState(t, pool)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			targetID := insertTransformTarget(t, tx)
			result := make(chan error, 1)
			go func() { result <- migrator.Up(ctx) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation='profiles'::regclass AND mode='AccessExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("migration did not wait for earlier job-target FK writer")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil {
				t.Fatal("migration accepted reference committed by earlier job-target FK writer")
			}
			var references int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_targets WHERE id=$1`, targetID).Scan(&references); err != nil || references != 1 {
				t.Fatalf("committed FK reference count=%d err=%v", references, err)
			}
			gotPayload, gotValidator, gotTrigger := profileState(t, pool)
			if gotPayload != payload || gotValidator != validator || gotTrigger != trigger {
				t.Fatal("failed concurrent migration changed profile state")
			}
		})
	})

	t.Run("transform publication invariant upgrade is fail-closed", func(t *testing.T) {
		ctx := context.Background()
		prepareVersionFive := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:5]).Up(ctx); err != nil {
				t.Fatalf("apply versions one through five: %v", err)
			}
			return pool, full
		}
		insertLegacyTransform := func(t *testing.T, pool *pgxpool.Pool, targets int) (string, string, []string) {
			t.Helper()
			mediaID, originalID, jobID := newUUIDv4(t), newUUIDv4(t), newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`,
				originalID, mediaID, strings.Repeat(strings.ReplaceAll(originalID, "-", ""), 2), "originals/00/aggregate-"+originalID+"/original.jpg"); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
				t.Fatal(err)
			}
			targetIDs := make([]string, targets)
			for index := range targets {
				targetIDs[index] = newUUIDv4(t)
				profileID := profile.StandardV1ID
				if index == 1 {
					profileID = profile.ThumbnailV1ID
				}
				if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetIDs[index], jobID, profileID); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, jobID, newUUIDv4(t)); err != nil {
				t.Fatal(err)
			}
			return mediaID, jobID, targetIDs
		}
		publishLegacyTarget := func(t *testing.T, pool *pgxpool.Pool, mediaID, targetID string) string {
			t.Helper()
			renditionID := newUUIDv4(t)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256)
				VALUES ($1,$2,$3,'ignored',false,$4,'image/avif',1,$5)`, renditionID, mediaID, targetID,
				"renditions/00/legacy-"+renditionID+"/output.avif", strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			return renditionID
		}
		assertVersionFiveRollback := func(t *testing.T, pool *pgxpool.Pool) {
			t.Helper()
			var version int64
			var auditColumn, aggregateFunction bool
			if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='renditions' AND column_name='processor_audit')`).Scan(&auditColumn); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT to_regprocedure('nmcp_check_transform_job_success()') IS NOT NULL`).Scan(&aggregateFunction); err != nil {
				t.Fatal(err)
			}
			if version != 5 || auditColumn || aggregateFunction {
				t.Fatalf("failed migration state: version=%d audit_column=%t aggregate_function=%t", version, auditColumn, aggregateFunction)
			}
		}

		validPool, validMigrator := prepareVersionFive(t)
		mediaID, jobID, targets := insertLegacyTransform(t, validPool, 2)
		renditionID := publishLegacyTarget(t, validPool, mediaID, targets[0])
		purgeID := newUUIDv4(t)
		purgeMediaID := newUUIDv4(t)
		if _, err := validPool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after)
			VALUES ($1,'image/jpeg','unknown',clock_timestamp(),clock_timestamp()+interval '1 day')`, purgeMediaID); err != nil {
			t.Fatal(err)
		}
		if _, err := validPool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',1)`, purgeID, purgeMediaID); err != nil {
			t.Fatal(err)
		}
		if err := validMigrator.Up(ctx); err != nil {
			t.Fatalf("upgrade valid partial transform and purge job: %v", err)
		}
		var status, audit string
		if err := validPool.QueryRow(ctx, `SELECT j.status,r.processor_audit::text FROM jobs j CROSS JOIN renditions r WHERE j.id=$1 AND r.id=$2`, jobID, renditionID).Scan(&status, &audit); err != nil {
			t.Fatal(err)
		}
		if status != "running" || audit != "{}" {
			t.Fatalf("valid upgrade state = status %q audit %q", status, audit)
		}

		for _, test := range []struct {
			name   string
			damage func(*testing.T, *pgxpool.Pool)
		}{
			{name: "succeeded job with pending target", damage: func(t *testing.T, pool *pgxpool.Pool) {
				_, jobID, _ := insertLegacyTransform(t, pool, 1)
				if _, err := pool.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "running job with every target succeeded", damage: func(t *testing.T, pool *pgxpool.Pool) {
				mediaID, _, targetIDs := insertLegacyTransform(t, pool, 1)
				publishLegacyTarget(t, pool, mediaID, targetIDs[0])
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				pool, migrator := prepareVersionFive(t)
				test.damage(t, pool)
				if err := migrator.Up(ctx); err == nil {
					t.Fatal("migration accepted an invalid legacy transform aggregate")
				}
				assertVersionFiveRollback(t, pool)
			})
		}

		t.Run("waits for earlier writer before preflight", func(t *testing.T) {
			pool, migrator := prepareVersionFive(t)
			_, jobID, _ := insertLegacyTransform(t, pool, 1)
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if _, err := writer.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- migrator.Up(ctx) }()
			awaitRelationLock(t, pool, ctx, 0, "jobs", "AccessExclusiveLock", false)
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := awaitContextResult(t, ctx, result); err == nil {
				t.Fatal("migration accepted invalid aggregate committed by an earlier writer")
			}
			assertVersionFiveRollback(t, pool)
		})
	})

	t.Run("job lease invariant upgrade accepts valid history and rejects stranded queue", func(t *testing.T) {
		ctx := context.Background()
		prepareVersionThree := func(t *testing.T) (*pgxpool.Pool, *Migrator) {
			t.Helper()
			pool := integrationPool(t, databaseURL)
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := newMigrator(pool, full.migrations[:3]).Up(ctx); err != nil {
				t.Fatalf("apply versions one through three: %v", err)
			}
			return pool, full
		}
		insertExhausted := func(t *testing.T, pool *pgxpool.Pool, finalStatus string) string {
			t.Helper()
			mediaID := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after)
				VALUES ($1,'image/jpeg','unknown',clock_timestamp(),clock_timestamp()+interval '1 day')`, mediaID); err != nil {
				t.Fatal(err)
			}
			jobID := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',1)`, jobID, mediaID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, jobID, newUUIDv4(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status=$2,lease_token=NULL,lease_expires_at=NULL,finished_at=CASE WHEN $2='failed' THEN clock_timestamp() ELSE NULL END WHERE id=$1`, jobID, finalStatus); err != nil {
				t.Fatal(err)
			}
			return jobID
		}

		validPool, validMigrator := prepareVersionThree(t)
		validID := insertExhausted(t, validPool, "failed")
		if err := validMigrator.Up(ctx); err != nil {
			t.Fatalf("upgrade valid version-three history: %v", err)
		}
		var validStatus string
		if err := validPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, validID).Scan(&validStatus); err != nil || validStatus != "failed" {
			t.Fatalf("valid history changed: status=%q err=%v", validStatus, err)
		}

		invalidPool, invalidMigrator := prepareVersionThree(t)
		invalidID := insertExhausted(t, invalidPool, "queued")
		if err := invalidMigrator.Up(ctx); err == nil {
			t.Fatal("upgrade accepted stranded exhausted queued job")
		}
		var invalidStatus string
		if err := invalidPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, invalidID).Scan(&invalidStatus); err != nil || invalidStatus != "queued" {
			t.Fatalf("failed upgrade rewrote history: status=%q err=%v", invalidStatus, err)
		}
	})

	t.Run("media MIME migration preserves valid rows and enforces exact MIME values", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:2]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one and two: %v", err)
		}
		mediaID := newUUIDv4(t)
		originalID := newUUIDv4(t)
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
			t.Fatalf("insert valid preexisting Media: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,'originals/00/preexisting/original.jpg','image/jpeg',1)`,
			originalID, mediaID, strings.Repeat("a", 64)); err != nil {
			t.Fatalf("insert valid preexisting Original: %v", err)
		}
		if err := full.Up(context.Background()); err != nil {
			t.Fatalf("upgrade valid preexisting Media/Original: %v", err)
		}

		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image','unknown')`, newUUIDv4(t))
		mismatchMediaID := newUUIDv4(t)
		if _, err := pool.Exec(context.Background(), `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/png','unknown')`, mismatchMediaID); err != nil {
			t.Fatalf("insert MIME-normalized Media: %v", err)
		}
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,'originals/00/mismatch/original.jpg','image/jpeg',1)`,
			newUUIDv4(t), mismatchMediaID, strings.Repeat("b", 64))
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,'originals/00/matching/original.png','image/png',1)`,
			newUUIDv4(t), mismatchMediaID, strings.Repeat("c", 64)); err != nil {
			t.Fatalf("insert matching Original MIME: %v", err)
		}
		expectExecError(t, pool, `UPDATE media SET media_type='image/jpeg' WHERE id=$1`, mismatchMediaID)
	})

	t.Run("profile migration rejects seed conflicts without adoption", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:1]).Up(context.Background()); err != nil {
			t.Fatalf("apply version one: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'standard',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, newUUIDv4(t), testProfileParameters(t)); err != nil {
			t.Fatalf("insert preexisting standard/v1: %v", err)
		}
		if err := full.Up(context.Background()); err == nil {
			t.Fatal("profile migration adopted or overwrote standard/v1 conflict")
		}
		assertProfileMigrationRolledBack(t, pool, 1)
	})

	t.Run("profile migration rejects incompatible legacy profile", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:1]).Up(context.Background()); err != nil {
			t.Fatalf("apply version one: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'legacy',1,'draft',ARRAY['image/jpeg'],'still',1,'{}')`, newUUIDv4(t)); err != nil {
			t.Fatalf("insert legacy profile: %v", err)
		}
		if err := full.Up(context.Background()); err == nil {
			t.Fatal("profile migration accepted incompatible legacy profile")
		}
		assertProfileMigrationRolledBack(t, pool, 1)
	})

	t.Run("profile migration rejects decimal-form fixed integers", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:1]).Up(context.Background()); err != nil {
			t.Fatalf("apply version one: %v", err)
		}
		decimalBitDepth := replaceJSONOnce(t, testProfileParameters(t), `"bit_depth":8`, `"bit_depth":8.0`)
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'legacy-decimal',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, newUUIDv4(t), decimalBitDepth); err != nil {
			t.Fatalf("insert decimal-form legacy profile under version one: %v", err)
		}
		if err := full.Up(context.Background()); err == nil {
			t.Fatal("profile migration accepted decimal-form fixed integer")
		}
		assertProfileMigrationRolledBack(t, pool, 1)
	})

	t.Run("profile migration rejects uncertified legacy active profile", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:1]).Up(context.Background()); err != nil {
			t.Fatalf("apply version one: %v", err)
		}
		profileID := newUUIDv4(t)
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'legacy-active',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, profileID, testProfileParameters(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate under version one rules: %v", err)
		}
		if err := full.Up(context.Background()); err == nil {
			t.Fatal("profile migration accepted active profile without certification evidence")
		}
		assertProfileMigrationRolledBack(t, pool, 1)
	})

	t.Run("media MIME migration waits for an earlier incompatible legacy writer", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:2]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one and two: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		writer, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin legacy writer: %v", err)
		}
		defer writer.Rollback(context.Background())
		mediaID := newUUIDv4(t)
		if _, err := writer.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/png','unknown')`, mediaID); err != nil {
			t.Fatalf("stage legacy Media: %v", err)
		}
		if _, err := writer.Exec(ctx, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,'originals/00/legacy-race/original.jpg','image/jpeg',1)`,
			newUUIDv4(t), mediaID, strings.Repeat("d", 64)); err != nil {
			t.Fatalf("stage incompatible legacy Original: %v", err)
		}

		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(ctx) }()
		awaitRelationLock(t, pool, ctx, 0, "media", "AccessExclusiveLock", false)
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit legacy writer: %v", err)
		}
		if err := awaitContextResult(t, ctx, migrationResult); err == nil {
			t.Fatal("migration succeeded after an incompatible legacy writer committed")
		}

		var version int64
		if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil || version != 2 {
			t.Fatalf("migration version after incompatible writer = %d, err=%v", version, err)
		}
		var mismatches int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM media AS m JOIN originals AS o ON o.media_id=m.id
			WHERE o.mime_type IS DISTINCT FROM m.media_type`).Scan(&mismatches); err != nil || mismatches != 1 {
			t.Fatalf("incompatible legacy rows = %d, err=%v", mismatches, err)
		}
	})

	t.Run("media MIME migration waits for an earlier matching legacy writer without deadlock", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:2]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one and two: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		writer, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin legacy writer: %v", err)
		}
		defer writer.Rollback(context.Background())
		mediaID := newUUIDv4(t)
		if _, err := writer.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/png','unknown')`, mediaID); err != nil {
			t.Fatalf("stage legacy Media: %v", err)
		}
		if _, err := writer.Exec(ctx, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,'originals/00/legacy-matching/original.png','image/png',1)`,
			newUUIDv4(t), mediaID, strings.Repeat("1", 64)); err != nil {
			t.Fatalf("stage matching legacy Original: %v", err)
		}

		migrationResult := make(chan error, 1)
		go func() { migrationResult <- full.Up(ctx) }()
		awaitRelationLock(t, pool, ctx, 0, "media", "AccessExclusiveLock", false)
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit matching legacy writer: %v", err)
		}
		if err := awaitContextResult(t, ctx, migrationResult); err != nil {
			t.Fatalf("migration after matching legacy writer: %v", err)
		}

		var matches int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM media AS m JOIN originals AS o ON o.media_id=m.id
			WHERE m.id=$1 AND o.mime_type=m.media_type`, mediaID).Scan(&matches); err != nil || matches != 1 {
			t.Fatalf("matching legacy rows = %d, err=%v", matches, err)
		}
	})

	t.Run("media MIME migration blocks a later writer until triggers are installed", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:2]).Up(context.Background()); err != nil {
			t.Fatalf("apply versions one and two: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		barrier, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire migration barrier connection: %v", err)
		}
		defer barrier.Release()
		const barrierKey = int64(0x4e4d435030303033) // "NMCP0003"
		if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, barrierKey); err != nil {
			t.Fatalf("acquire migration barrier: %v", err)
		}
		defer barrier.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey)

		migrationUnderTest := full.migrations[2]
		const lockStatement = "LOCK TABLE media, originals IN ACCESS EXCLUSIVE MODE;"
		instrumentedSQL := strings.Replace(migrationUnderTest.sql, lockStatement, lockStatement+fmt.Sprintf("\nSELECT pg_catalog.pg_advisory_xact_lock(%d);", barrierKey), 1)
		if instrumentedSQL == migrationUnderTest.sql {
			t.Fatal("migration lock statement was not found for test instrumentation")
		}
		migrationUnderTest.sql = instrumentedSQL
		migrationUnderTest.checksum = checksumSQL([]byte(instrumentedSQL))
		migrationResult := make(chan error, 1)
		go func() {
			migrationResult <- newMigrator(pool, []migration{full.migrations[0], full.migrations[1], migrationUnderTest}).Up(ctx)
		}()
		awaitRelationLock(t, pool, ctx, 0, "media", "AccessExclusiveLock", true)
		awaitRelationLock(t, pool, ctx, 0, "originals", "AccessExclusiveLock", true)

		writerConn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire later writer connection: %v", err)
		}
		defer writerConn.Release()
		var writerPID int32
		if err := writerConn.QueryRow(ctx, `SELECT pg_catalog.pg_backend_pid()`).Scan(&writerPID); err != nil {
			t.Fatalf("read later writer PID: %v", err)
		}
		writerResult := make(chan error, 1)
		mediaID := newUUIDv4(t)
		originalID := newUUIDv4(t)
		go func() {
			tx, err := writerConn.Begin(ctx)
			if err == nil {
				_, err = tx.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/png','unknown')`, mediaID)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `
					INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
					VALUES ($1,$2,$3,'originals/00/blocked-race/original.jpg','image/jpeg',1)`,
					originalID, mediaID, strings.Repeat("e", 64))
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else if tx != nil {
				_ = tx.Rollback(context.Background())
			}
			writerResult <- err
		}()
		awaitRelationLock(t, pool, ctx, writerPID, "media", "RowExclusiveLock", false)

		if _, err := barrier.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, barrierKey); err != nil {
			t.Fatalf("release migration barrier: %v", err)
		}
		if err := awaitContextResult(t, ctx, migrationResult); err != nil {
			t.Fatalf("migration after releasing barrier: %v", err)
		}
		if err := awaitContextResult(t, ctx, writerResult); err == nil {
			t.Fatal("later incompatible writer passed the installed trigger")
		}
		var mediaRows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE id=$1`, mediaID).Scan(&mediaRows); err != nil || mediaRows != 0 {
			t.Fatalf("failed writer Media rows = %d, err=%v", mediaRows, err)
		}

		matchingWriter, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin matching writer after migration: %v", err)
		}
		matchingMediaID := newUUIDv4(t)
		if _, err = matchingWriter.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/png','unknown')`, matchingMediaID); err == nil {
			_, err = matchingWriter.Exec(ctx, `
				INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
				VALUES ($1,$2,$3,'originals/00/matching-after-lock/original.png','image/png',1)`,
				newUUIDv4(t), matchingMediaID, strings.Repeat("f", 64))
		}
		if err == nil {
			err = matchingWriter.Commit(ctx)
		} else {
			_ = matchingWriter.Rollback(context.Background())
		}
		if err != nil {
			t.Fatalf("matching writer after migration: %v", err)
		}
	})

	t.Run("profile migration preserves compatible custom draft", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		full, err := NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := newMigrator(pool, full.migrations[:1]).Up(context.Background()); err != nil {
			t.Fatalf("apply version one: %v", err)
		}
		customID := newUUIDv4(t)
		legacyParameters := replaceJSONOnce(t, testProfileParameters(t),
			"preserve-aspect-no-crop-no-upscale-round-nearest",
			"preserve-aspect-no-crop-no-upscale-even-round-down")
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'custom',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, customID, legacyParameters); err != nil {
			t.Fatalf("insert compatible custom draft: %v", err)
		}
		if err := newMigrator(pool, full.migrations[:2]).Up(context.Background()); err != nil {
			t.Fatalf("upgrade compatible custom draft: %v", err)
		}
		var count int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM profiles WHERE id=$1 OR key IN ('standard','thumbnail')`, customID).Scan(&count); err != nil || count != 3 {
			t.Fatalf("preserved/seeded profiles count = %d, err=%v", count, err)
		}
	})

	runInitialSchemaIntegrationTests(t, databaseURL)

	t.Run("concurrent Up calls serialize", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrationEntry := testMigration(1, "serialized", "SELECT pg_sleep(0.2)")
		migrator := newMigrator(pool, []migration{migrationEntry})

		start := make(chan struct{})
		errorsByCall := make(chan error, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		for i := 0; i < 2; i++ {
			go func() {
				ready.Done()
				<-start
				errorsByCall <- migrator.Up(context.Background())
			}()
		}
		ready.Wait()
		close(start)
		for i := 0; i < 2; i++ {
			if err := <-errorsByCall; err != nil {
				t.Fatalf("concurrent Up() error = %v", err)
			}
		}

		var count int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
			t.Fatalf("count migration history: %v", err)
		}
		if count != 1 {
			t.Fatalf("migration history count = %d, want 1", count)
		}
	})

	t.Run("transactional migration rolls back SQL and history together", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrationEntry := testMigration(1, "rollback", `
			CREATE TABLE should_roll_back (id bigint PRIMARY KEY);
			INSERT INTO should_roll_back VALUES (1);
			SELECT 1 / 0;`)
		migrator := newMigrator(pool, []migration{migrationEntry})

		if err := migrator.Up(context.Background()); err == nil {
			t.Fatal("Up() expected migration failure")
		}
		var tableExists bool
		if err := pool.QueryRow(context.Background(), `SELECT to_regclass('should_roll_back') IS NOT NULL`).Scan(&tableExists); err != nil {
			t.Fatalf("check rolled-back table: %v", err)
		}
		if tableExists {
			t.Fatal("failed transactional migration left its table behind")
		}
		var historyCount int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&historyCount); err != nil {
			t.Fatalf("count migration history: %v", err)
		}
		if historyCount != 0 {
			t.Fatalf("migration history count = %d, want 0", historyCount)
		}
	})

	t.Run("transaction-off migration executes outside a transaction", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		fsys := fstest.MapFS{
			"migrations/1_table.sql": {Data: []byte(`CREATE TABLE indexed_photos (id bigint PRIMARY KEY)`)},
			"migrations/2_index.sql": {Data: []byte(nonTransactionalDirective + `
CREATE INDEX CONCURRENTLY indexed_photos_id_idx ON indexed_photos (id)`)},
		}
		migrations, err := discoverMigrations(fsys, "migrations")
		if err != nil {
			t.Fatalf("discoverMigrations() error = %v", err)
		}
		migrator := newMigrator(pool, migrations)

		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		var indexExists bool
		if err := pool.QueryRow(context.Background(), `SELECT to_regclass('indexed_photos_id_idx') IS NOT NULL`).Scan(&indexExists); err != nil {
			t.Fatalf("check concurrent index: %v", err)
		}
		if !indexExists {
			t.Fatal("transaction-off migration did not create its index")
		}
		var dirty bool
		if err := pool.QueryRow(context.Background(), `SELECT dirty FROM schema_migrations WHERE version = 2`).Scan(&dirty); err != nil {
			t.Fatalf("read transaction-off history: %v", err)
		}
		if dirty {
			t.Fatal("successful transaction-off migration remained dirty")
		}
	})

	t.Run("unsafe migration files are rejected before execution", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		tests := []struct {
			name string
			sql  string
		}{
			{name: "transaction control", sql: "CREATE TABLE rejected_control (id bigint); COMMIT"},
			{name: "transaction-off multiple statements", sql: nonTransactionalDirective + "\nCREATE TABLE rejected_multiple (id bigint); SELECT 1"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fsys := fstest.MapFS{"migrations/1_rejected.sql": {Data: []byte(test.sql)}}
				if _, err := discoverMigrations(fsys, "migrations"); err == nil {
					t.Fatal("discoverMigrations() expected an error")
				}
			})
		}

		for _, table := range []string{"rejected_control", "rejected_multiple"} {
			var exists bool
			if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
				t.Fatalf("check rejected migration side effect: %v", err)
			}
			if exists {
				t.Fatalf("rejected migration created table %q", table)
			}
		}
	})

	t.Run("dirty idempotent transaction-off tail is rerun", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		if err := newMigrator(pool, nil).Up(context.Background()); err != nil {
			t.Fatalf("initialize history: %v", err)
		}
		fsys := fstest.MapFS{"migrations/1_recover.sql": {Data: []byte(nonTransactionalDirective + `
			INSERT INTO recovered_values VALUES (1) ON CONFLICT DO NOTHING;`)}}
		migrations, err := discoverMigrations(fsys, "migrations")
		if err != nil {
			t.Fatalf("discoverMigrations() error = %v", err)
		}
		migrationEntry := migrations[0]
		if _, err := pool.Exec(context.Background(), `CREATE TABLE recovered_values (id bigint PRIMARY KEY); INSERT INTO recovered_values VALUES (1)`); err != nil {
			t.Fatalf("precreate partial migration result: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO schema_migrations
				(version, name, checksum, transactional, idempotent, dirty)
			VALUES ($1, $2, $3, false, true, true)`,
			migrationEntry.version, migrationEntry.name, migrationEntry.checksum); err != nil {
			t.Fatalf("precreate dirty history: %v", err)
		}
		migrator := newMigrator(pool, migrations)

		status, err := migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Drift || status.Ready() || status.CurrentVersion != 0 {
			t.Fatalf("Status() = %+v, want dirty drift at current version zero", status)
		}
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() recovery error = %v", err)
		}
		var valueCount int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recovered_values`).Scan(&valueCount); err != nil {
			t.Fatalf("count recovered values: %v", err)
		}
		if valueCount != 1 {
			t.Fatalf("recovered value count = %d, want 1", valueCount)
		}
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("second Up() error = %v", err)
		}
	})

	t.Run("transactional dirty history is refused", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrationEntry := testMigration(1, "impossible", "SELECT 1")
		if err := newMigrator(pool, nil).Up(context.Background()); err != nil {
			t.Fatalf("initialize history: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO schema_migrations
				(version, name, checksum, transactional, idempotent, dirty)
			VALUES ($1, $2, $3, true, false, true)`,
			migrationEntry.version, migrationEntry.name, migrationEntry.checksum); err != nil {
			t.Fatalf("precreate dirty history: %v", err)
		}
		migrator := newMigrator(pool, []migration{migrationEntry})

		if err := migrator.Up(context.Background()); !errors.Is(err, ErrDirtyMigration) {
			t.Fatalf("Up() error = %v, want ErrDirtyMigration", err)
		}
	})

	t.Run("cancellation still releases session lock", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		applicationName := "nmcp_cancel_" + strings.ReplaceAll(newUUIDv4(t), "-", "")
		migrationSQL := fmt.Sprintf(
			"SELECT pg_catalog.set_config('application_name', '%s', false); SELECT pg_catalog.pg_sleep(10)",
			applicationName,
		)
		migrator := newMigrator(pool, []migration{testMigration(1, "cancel", migrationSQL)})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- migrator.Up(ctx) }()

		observeCtx, observeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		migrationPID := awaitApplicationBackendPID(t, pool, observeCtx, applicationName)
		observeCancel()
		cancel()
		resultCtx, resultCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer resultCancel()
		if err := awaitContextResult(t, resultCtx, result); err == nil {
			t.Fatal("Up() expected cancellation error")
		}

		candidate, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire lock-check connection: %v", err)
		}
		defer candidate.Release()
		verifier := candidate
		var candidatePID int32
		if err := candidate.QueryRow(context.Background(), `SELECT pg_catalog.pg_backend_pid()`).Scan(&candidatePID); err != nil {
			t.Fatalf("read lock-check backend PID: %v", err)
		}
		if candidatePID == migrationPID {
			verifier, err = pool.Acquire(context.Background())
			if err != nil {
				t.Fatalf("acquire distinct lock-check connection: %v", err)
			}
			defer verifier.Release()
		}
		var verifierPID int32
		if err := verifier.QueryRow(context.Background(), `SELECT pg_catalog.pg_backend_pid()`).Scan(&verifierPID); err != nil {
			t.Fatalf("read distinct verifier PID: %v", err)
		}
		if verifierPID == migrationPID {
			t.Fatalf("lock verifier reused migration backend PID %d", migrationPID)
		}
		lockCtx, lockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer lockCancel()
		var locked bool
		for !locked {
			if err := verifier.QueryRow(lockCtx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, migrationLockKey).Scan(&locked); err != nil {
				t.Fatalf("try migration lock: %v", err)
			}
			if !locked {
				select {
				case <-lockCtx.Done():
					t.Fatalf("migration session lock remained held after cancellation: %v", lockCtx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if _, err := verifier.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			t.Fatalf("release lock-check lock: %v", err)
		}
	})

	t.Run("reentrant advisory lock is fully released", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Reserve the observer before Up so it cannot be the migration backend.
		verifier, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire lock observer: %v", err)
		}
		defer verifier.Release()
		// Record the actual executor, rather than guessing which pooled connection
		// Up used. The table lives in this test's isolated schema.
		sql := fmt.Sprintf(`
			CREATE TABLE migration_backend AS SELECT pg_catalog.pg_backend_pid() AS pid;
			SELECT pg_catalog.pg_advisory_lock(%d)`, migrationLockKey)
		migrator := newMigrator(pool, []migration{testMigration(1, "reentrant_lock", sql)})
		if err := migrator.Up(ctx); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		var migrationPID int32
		if err := verifier.QueryRow(ctx, `SELECT pid FROM migration_backend`).Scan(&migrationPID); err != nil {
			t.Fatalf("read migration backend PID: %v", err)
		}
		if backendHasAdvisoryLocks(t, ctx, verifier, migrationPID) {
			t.Fatalf("migration backend %d retained advisory locks after Up", migrationPID)
		}

		// A legitimate foreign holder of the same database-global key must not
		// look like our migration leaked. Use a new physical session, not the pool.
		foreign, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatalf("connect foreign lock holder: %v", err)
		}
		defer foreign.Close(context.Background())
		if _, err := foreign.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, migrationLockKey); err != nil {
			t.Fatalf("acquire foreign migration lock: %v", err)
		}
		if backendHasAdvisoryLocks(t, ctx, verifier, migrationPID) {
			t.Fatal("foreign holder was misidentified as a migration lock leak")
		}
		var acquired bool
		if err := verifier.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, migrationLockKey).Scan(&acquired); err != nil {
			t.Fatalf("try lock held by foreign backend: %v", err)
		}
		if acquired {
			_, _ = verifier.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock_all()`)
			t.Fatal("foreign holder did not exclude the old one-shot lock probe")
		}

		// Positive control: one unlock of a twice-acquired lock leaves a real
		// reentrant leak. The same observer predicate must detect that remainder.
		if _, err := foreign.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, migrationLockKey); err != nil {
			t.Fatalf("reenter control lock: %v", err)
		}
		var unlocked bool
		if err := foreign.QueryRow(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, migrationLockKey).Scan(&unlocked); err != nil || !unlocked {
			t.Fatalf("release one control lock acquisition: unlocked=%t, err=%v", unlocked, err)
		}
		foreignPID := int32(foreign.PgConn().PID())
		if !backendHasAdvisoryLocks(t, ctx, verifier, foreignPID) {
			t.Fatal("observer missed an intentionally retained reentrant lock")
		}
		if _, err := foreign.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock_all()`); err != nil {
			t.Fatalf("release control locks: %v", err)
		}
		if backendHasAdvisoryLocks(t, ctx, verifier, foreignPID) {
			t.Fatal("observer reported a leak after control locks were released")
		}
	})

	t.Run("failed advisory unlock discards the physical connection", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrator := newMigrator(pool, []migration{testMigration(1, "terminate", `SELECT pg_terminate_backend(pg_backend_pid())`)})
		if err := migrator.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "release migration advisory locks") {
			t.Fatalf("Up() error = %v, want advisory unlock failure", err)
		}
		if total := pool.Stat().TotalConns(); total != 0 {
			t.Fatalf("pool total connections after discard = %d, want 0", total)
		}

		var replacementBackend int
		if err := pool.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&replacementBackend); err != nil {
			t.Fatalf("use pool after connection discard: %v", err)
		}
	})

	t.Run("unknown applied version is drift", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrator, err := NewMigrator(pool)
		if err != nil {
			t.Fatalf("NewMigrator() error = %v", err)
		}
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() initialization error = %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO schema_migrations
				(version, name, checksum, transactional, idempotent, dirty)
			VALUES (99, 'future', $1, true, false, false)`, strings.Repeat("0", 64)); err != nil {
			t.Fatalf("insert unknown history row: %v", err)
		}

		status, err := migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Drift || status.Ready() {
			t.Fatalf("Status() = %+v, want drift and not ready", status)
		}
		if err := migrator.Up(context.Background()); !errors.Is(err, ErrUnknownAppliedVersion) {
			t.Fatalf("Up() error = %v, want ErrUnknownAppliedVersion", err)
		}
	})

	t.Run("checksum change is drift", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrationEntry := testMigration(1, "checksum", "SELECT 1")
		migrator := newMigrator(pool, []migration{migrationEntry})
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			UPDATE schema_migrations SET checksum = $1 WHERE version = 1`, strings.Repeat("0", 64)); err != nil {
			t.Fatalf("change stored checksum: %v", err)
		}

		status, err := migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Drift || status.Ready() {
			t.Fatalf("Status() = %+v, want drift and not ready", status)
		}
		if err := migrator.Up(context.Background()); !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("Up() error = %v, want ErrChecksumMismatch", err)
		}
	})

	t.Run("invalid history metadata is drift", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		migrationEntry := testMigration(1, "metadata", "SELECT 1")
		migrator := newMigrator(pool, []migration{migrationEntry})
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			UPDATE schema_migrations SET name = 'renamed' WHERE version = 1`); err != nil {
			t.Fatalf("change stored name: %v", err)
		}

		status, err := migrator.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Drift || status.Ready() {
			t.Fatalf("Status() = %+v, want drift and not ready", status)
		}
		if err := migrator.Up(context.Background()); !errors.Is(err, ErrInvalidMigrationHistory) {
			t.Fatalf("Up() error = %v, want ErrInvalidMigrationHistory", err)
		}
	})
}

func backendHasAdvisoryLocks(t *testing.T, ctx context.Context, verifier *pgxpool.Conn, pid int32) bool {
	t.Helper()
	if pid <= 0 || uint32(pid) == verifier.Conn().PgConn().PID() {
		t.Fatalf("lock observation requires a distinct backend, got PID %d", pid)
	}
	var held bool
	if err := verifier.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid=$1
		)`, pid).Scan(&held); err != nil {
		t.Fatalf("observe advisory locks for backend %d: %v", pid, err)
	}
	return held
}

func assertProfileMigrationRolledBack(t *testing.T, pool *pgxpool.Pool, profileCount int) {
	t.Helper()
	ctx := context.Background()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations WHERE NOT dirty`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration version after rollback = %d, err=%v", version, err)
	}
	var capabilitiesExist bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('profile_processor_capabilities') IS NOT NULL`).Scan(&capabilitiesExist); err != nil {
		t.Fatal(err)
	}
	if capabilitiesExist {
		t.Fatal("failed profile migration left capability table behind")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM profiles`).Scan(&count); err != nil || count != profileCount {
		t.Fatalf("profiles after rollback = %d, err=%v", count, err)
	}
}

func testMigration(version int64, name, sql string) migration {
	return migration{version: version, name: name, checksum: checksumSQL([]byte(sql)), sql: sql, transactional: true}
}

func testNonTransactionalMigration(version int64, name, sql string) migration {
	return migration{version: version, name: name, checksum: checksumSQL([]byte(sql)), sql: sql, idempotent: true}
}

func awaitRelationLock(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pid int32, relation, mode string, granted bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var found bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks AS l
				JOIN pg_catalog.pg_class AS c ON c.oid=l.relation
				JOIN pg_catalog.pg_namespace AS n ON n.oid=c.relnamespace
				WHERE n.nspname=current_schema()
				  AND c.relname=$1 AND l.mode=$2 AND l.granted=$3
				  AND ($4::integer = 0 OR l.pid=$4)
			)`, relation, mode, granted, pid).Scan(&found)
		if err != nil {
			t.Fatalf("poll %s lock on %s: %v", mode, relation, err)
		}
		if found {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for granted=%t %s lock on %s: %v", granted, mode, relation, ctx.Err())
		case <-ticker.C:
		}
	}
}

func awaitContextResult(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatalf("wait for concurrent database operation: %v", ctx.Err())
		return ctx.Err()
	}
}

func awaitApplicationBackendPID(t *testing.T, pool *pgxpool.Pool, ctx context.Context, applicationName string) int32 {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := pool.QueryRow(ctx, `
			SELECT pid FROM pg_catalog.pg_stat_activity
			WHERE application_name=$1 AND state='active'
			ORDER BY pid LIMIT 1`, applicationName).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("observe migration backend PID: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for migration backend %q: %v", applicationName, ctx.Err())
			return 0
		case <-ticker.C:
		}
	}
}

func integrationPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to integration database: %v", err)
	}
	t.Cleanup(admin.Close)

	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate test schema name: %v", err)
	}
	schema := "nmcp_migration_test_" + hex.EncodeToString(random)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = identifier
	if config.MaxConns < 5 {
		config.MaxConns = 5
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect using isolated test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping integration database: %v", err)
	}
	return pool
}

func integrationRolePool(t *testing.T, databaseURL, schema, role string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	quotedRole := pgx.Identifier{role}.Sanitize()
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SET ROLE `+quotedRole)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}
