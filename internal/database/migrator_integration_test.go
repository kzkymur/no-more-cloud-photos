package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		if status.CurrentVersion != 0 || status.ExpectedVersion != 0 || !status.Ready() {
			t.Fatalf("Status() = %+v, want ready version zero", status)
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
	})

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
		migrator := newMigrator(pool, []migration{testMigration(1, "cancel", "SELECT pg_sleep(10)")})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		if err := migrator.Up(ctx); err == nil {
			t.Fatal("Up() expected cancellation error")
		}
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire lock-check connection: %v", err)
		}
		defer conn.Release()
		var locked bool
		if err := conn.QueryRow(context.Background(), `SELECT pg_catalog.pg_try_advisory_lock($1)`, migrationLockKey).Scan(&locked); err != nil {
			t.Fatalf("try migration lock: %v", err)
		}
		if !locked {
			t.Fatal("migration session lock remained held after cancellation")
		}
		if _, err := conn.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			t.Fatalf("release lock-check lock: %v", err)
		}
	})

	t.Run("reentrant advisory lock is fully released", func(t *testing.T) {
		pool := integrationPool(t, databaseURL)
		sql := fmt.Sprintf("SELECT pg_catalog.pg_advisory_lock(%d)", migrationLockKey)
		migrator := newMigrator(pool, []migration{testMigration(1, "reentrant_lock", sql)})
		if err := migrator.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}

		first, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire first connection: %v", err)
		}
		defer first.Release()
		second, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire independent lock-check connection: %v", err)
		}
		defer second.Release()
		var acquired bool
		if err := second.QueryRow(context.Background(), `SELECT pg_catalog.pg_try_advisory_lock($1)`, migrationLockKey).Scan(&acquired); err != nil {
			t.Fatalf("try advisory lock from independent session: %v", err)
		}
		if !acquired {
			t.Fatal("reentrant migration lock remained held after Up")
		}
		if _, err := second.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock_all()`); err != nil {
			t.Fatalf("release independent lock: %v", err)
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

func testMigration(version int64, name, sql string) migration {
	return migration{version: version, name: name, checksum: checksumSQL([]byte(sql)), sql: sql, transactional: true}
}

func testNonTransactionalMigration(version int64, name, sql string) migration {
	return migration{version: version, name: name, checksum: checksumSQL([]byte(sql)), sql: sql, idempotent: true}
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
