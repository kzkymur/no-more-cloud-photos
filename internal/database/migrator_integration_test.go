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
	"github.com/jackc/pgx/v5/pgxpool"
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
		if status.CurrentVersion != 0 || status.ExpectedVersion != 5 || status.Ready() || !status.Pending {
			t.Fatalf("Status() = %+v, want pending version five", status)
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
		if status.CurrentVersion != 5 || status.ExpectedVersion != 5 || !status.Ready() {
			t.Fatalf("Status() after Up = %+v, want ready version five", status)
		}
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("second Up() error = %v", err)
		}
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
			jobID := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',1)`, jobID, newUUIDv4(t)); err != nil {
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
