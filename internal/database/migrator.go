// Package database provides PostgreSQL migration support.
package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	migrationsDirectory       = "migrations"
	migrationLockKey          = int64(0x4e4d43504d494752) // "NMCPMIGR"
	cleanupTimeout            = 5 * time.Second
	nonTransactionalDirective = "-- nmcp:transaction=off idempotent=true"
)

var (
	// ErrUnknownAppliedVersion means the database contains a migration that is
	// not present in this binary.
	ErrUnknownAppliedVersion = errors.New("unknown applied migration version")
	// ErrChecksumMismatch means an embedded migration changed after it was applied.
	ErrChecksumMismatch = errors.New("migration checksum mismatch")
	// ErrInvalidMigrationHistory means applied migrations are not an exact prefix
	// of the embedded migration sequence.
	ErrInvalidMigrationHistory = errors.New("invalid migration history")
	// ErrDirtyMigration means a migration attempt did not finish cleanly.
	ErrDirtyMigration = errors.New("dirty migration history")
)

//go:embed migrations
var embeddedMigrations embed.FS

// Status describes whether the database schema is ready for this binary.
type Status struct {
	CurrentVersion  int64  `json:"current_version"`
	ExpectedVersion int64  `json:"expected_version"`
	Pending         bool   `json:"pending"`
	Drift           bool   `json:"drift"`
	DriftReason     string `json:"drift_reason,omitempty"`
}

// Ready reports whether there are no pending migrations or history drift.
func (s Status) Ready() bool {
	return !s.Pending && !s.Drift
}

// Migrator applies the immutable migrations embedded in the binary.
type Migrator struct {
	pool       *pgxpool.Pool
	migrations []migration
}

type migration struct {
	version       int64
	name          string
	checksum      string
	sql           string
	transactional bool
	idempotent    bool
}

type appliedMigration struct {
	version       int64
	name          string
	checksum      string
	transactional bool
	idempotent    bool
	dirty         bool
}

// NewMigrator discovers and validates the migrations embedded in this binary.
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	if pool == nil {
		return nil, errors.New("database migration pool is nil")
	}

	migrations, err := discoverMigrations(embeddedMigrations, migrationsDirectory)
	if err != nil {
		return nil, fmt.Errorf("discover embedded migrations: %w", err)
	}
	return newMigrator(pool, migrations), nil
}

func newMigrator(pool *pgxpool.Pool, migrations []migration) *Migrator {
	return &Migrator{pool: pool, migrations: slices.Clone(migrations)}
}

// Status inspects migration state without creating or modifying database objects.
func (m *Migrator) Status(ctx context.Context) (Status, error) {
	status := Status{ExpectedVersion: expectedVersion(m.migrations)}

	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return status, fmt.Errorf("acquire connection for migration status: %w", err)
	}
	defer conn.Release()

	var historyExists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&historyExists); err != nil {
		return status, fmt.Errorf("check migration history table: %w", err)
	}
	if !historyExists {
		status.Pending = len(m.migrations) > 0
		return status, nil
	}

	applied, err := readApplied(ctx, conn)
	if err != nil {
		return status, err
	}
	status.CurrentVersion = currentVersion(applied)

	pending, err := validateHistory(m.migrations, applied)
	if err != nil {
		status.Drift = true
		status.DriftReason = err.Error()
		return status, nil
	}
	status.Pending = len(pending) > 0
	return status, nil
}

// Up creates migration history infrastructure and applies all pending migrations.
// One session-level advisory lock serializes the complete operation, including
// migrations that PostgreSQL does not permit inside a transaction.
func (m *Migrator) Up(ctx context.Context) (returnErr error) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migrations: %w", err)
	}
	locked := false
	defer func() {
		if !locked {
			conn.Release()
			return
		}

		unlockCtx, cancelUnlock := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		// Advisory locks are reentrant. A single successful unlock is not proof
		// that migration SQL did not increment the same lock count. This
		// connection is dedicated to Up, so clear every session lock before it can
		// be returned to the pool.
		_, unlockErr := conn.Exec(unlockCtx, `SELECT pg_catalog.pg_advisory_unlock_all()`)
		cancelUnlock()
		if unlockErr == nil {
			conn.Release()
			return
		}
		returnErr = errors.Join(returnErr, fmt.Errorf("release migration advisory locks: %w", unlockErr))

		physicalConn := conn.Hijack()
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancelClose()
		if err := physicalConn.Close(closeCtx); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("discard migration connection: %w", err))
		}
	}()

	// Once acquisition is attempted, its server-side outcome is uncertain on a
	// context/network error. The defer must therefore confirm unlock or discard
	// the physical session instead of ever returning it to the pool unchecked.
	locked = true
	if _, err := conn.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint PRIMARY KEY CHECK (version > 0),
			name text NOT NULL CHECK (name <> ''),
			checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
			transactional boolean NOT NULL,
			idempotent boolean NOT NULL,
			dirty boolean NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			CHECK (transactional OR idempotent)
		)`); err != nil {
		return fmt.Errorf("create migration history table: %w", err)
	}

	applied, err := readApplied(ctx, conn)
	if err != nil {
		return err
	}
	plan, err := planHistory(m.migrations, applied, true)
	if err != nil {
		return err
	}

	if plan.dirty != nil {
		if err := applyNonTransactional(ctx, conn, *plan.dirty); err != nil {
			return err
		}
	}
	for _, migration := range plan.pending {
		if migration.transactional {
			if err := applyTransactional(ctx, conn, migration); err != nil {
				return err
			}
			continue
		}
		if err := applyNonTransactional(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func applyTransactional(ctx context.Context, conn *pgxpool.Conn, migration migration) (returnErr error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %d_%s: %w", migration.version, migration.name, err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			returnErr = errors.Join(returnErr, fmt.Errorf("roll back migration %d_%s: %w", migration.version, migration.name, err))
		}
	}()

	if _, err := tx.Exec(ctx, migration.sql); err != nil {
		return fmt.Errorf("apply migration %d_%s: %w", migration.version, migration.name, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migrations
			(version, name, checksum, transactional, idempotent, dirty)
		VALUES ($1, $2, $3, $4, $5, false)`,
		migration.version, migration.name, migration.checksum, migration.transactional, migration.idempotent); err != nil {
		return fmt.Errorf("record migration %d_%s: %w", migration.version, migration.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d_%s: %w", migration.version, migration.name, err)
	}
	return nil
}

func applyNonTransactional(ctx context.Context, conn *pgxpool.Conn, migration migration) error {
	if _, err := conn.Exec(ctx, `
		INSERT INTO schema_migrations
			(version, name, checksum, transactional, idempotent, dirty)
		VALUES ($1, $2, $3, $4, $5, true)
		ON CONFLICT (version) DO UPDATE SET
			name = EXCLUDED.name,
			checksum = EXCLUDED.checksum,
			transactional = EXCLUDED.transactional,
			idempotent = EXCLUDED.idempotent,
			dirty = true,
			applied_at = clock_timestamp()`,
		migration.version, migration.name, migration.checksum, migration.transactional, migration.idempotent); err != nil {
		return fmt.Errorf("record in-progress migration %d_%s: %w", migration.version, migration.name, err)
	}
	if _, err := conn.Exec(ctx, migration.sql); err != nil {
		return fmt.Errorf("apply migration %d_%s: %w", migration.version, migration.name, err)
	}
	commandTag, err := conn.Exec(ctx, `
		UPDATE schema_migrations
		SET dirty = false, applied_at = clock_timestamp()
		WHERE version = $1 AND dirty`, migration.version)
	if err != nil {
		return fmt.Errorf("complete migration %d_%s: %w", migration.version, migration.name, err)
	}
	if commandTag.RowsAffected() != 1 {
		return fmt.Errorf("%w: migration %d completion row is missing", ErrInvalidMigrationHistory, migration.version)
	}
	return nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readApplied(ctx context.Context, q queryer) ([]appliedMigration, error) {
	rows, err := q.Query(ctx, `
		SELECT version, name, checksum, transactional, idempotent, dirty
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()

	var applied []appliedMigration
	for rows.Next() {
		var row appliedMigration
		if err := rows.Scan(&row.version, &row.name, &row.checksum, &row.transactional, &row.idempotent, &row.dirty); err != nil {
			return nil, fmt.Errorf("read migration history row: %w", err)
		}
		applied = append(applied, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read migration history rows: %w", err)
	}
	return applied, nil
}

func validateHistory(migrations []migration, applied []appliedMigration) ([]migration, error) {
	plan, err := planHistory(migrations, applied, false)
	if err != nil {
		return nil, err
	}
	return plan.pending, nil
}

type migrationPlan struct {
	dirty   *migration
	pending []migration
}

func planHistory(migrations []migration, applied []appliedMigration, recoverDirty bool) (migrationPlan, error) {
	byVersion := make(map[int64]migration, len(migrations))
	for _, migration := range migrations {
		byVersion[migration.version] = migration
	}

	for i, row := range applied {
		expected, known := byVersion[row.version]
		if !known {
			return migrationPlan{}, fmt.Errorf("%w: %d", ErrUnknownAppliedVersion, row.version)
		}
		if row.checksum != expected.checksum {
			return migrationPlan{}, fmt.Errorf("%w for version %d", ErrChecksumMismatch, row.version)
		}
		if row.name != expected.name {
			return migrationPlan{}, fmt.Errorf("%w: version %d has an unexpected name", ErrInvalidMigrationHistory, row.version)
		}
		if i >= len(migrations) || row.version != migrations[i].version {
			return migrationPlan{}, fmt.Errorf("%w: version %d is not the next expected version", ErrInvalidMigrationHistory, row.version)
		}
		if row.transactional != expected.transactional || row.idempotent != expected.idempotent {
			return migrationPlan{}, fmt.Errorf("%w: version %d has unexpected execution metadata", ErrInvalidMigrationHistory, row.version)
		}
		if !row.dirty {
			continue
		}
		if i != len(applied)-1 {
			return migrationPlan{}, fmt.Errorf("%w: %w at non-tail version %d", ErrInvalidMigrationHistory, ErrDirtyMigration, row.version)
		}
		if !recoverDirty || expected.transactional || !expected.idempotent {
			return migrationPlan{}, fmt.Errorf("%w: %w at version %d", ErrInvalidMigrationHistory, ErrDirtyMigration, row.version)
		}
		dirty := expected
		return migrationPlan{dirty: &dirty, pending: slices.Clone(migrations[len(applied):])}, nil
	}

	return migrationPlan{pending: slices.Clone(migrations[len(applied):])}, nil
}

func discoverMigrations(fsys fs.FS, directory string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, directory)
	if err != nil {
		return nil, err
	}

	var migrations []migration
	versions := make(map[int64]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationFilename(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, exists := versions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, entry.Name())
		}

		contents, err := fs.ReadFile(fsys, directory+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if len(contents) == 0 {
			return nil, fmt.Errorf("migration %q is empty", entry.Name())
		}
		transactional, idempotent, err := parseMigrationDirective(contents)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), err)
		}
		statements, err := scanSQLStatements(contents)
		if err != nil {
			return nil, fmt.Errorf("migration %q: invalid SQL: %w", entry.Name(), err)
		}
		if len(statements) == 0 {
			return nil, fmt.Errorf("migration %q: migration must contain at least one executable statement", entry.Name())
		}
		for _, statement := range statements {
			if isTransactionControl(statement) {
				return nil, fmt.Errorf("migration %q: explicit transaction control is not allowed", entry.Name())
			}
		}
		if !transactional && len(statements) != 1 {
			return nil, fmt.Errorf("migration %q: transaction-off migration must contain exactly one statement", entry.Name())
		}
		versions[version] = entry.Name()
		migrations = append(migrations, migration{
			version:       version,
			name:          name,
			checksum:      checksumSQL(contents),
			sql:           string(contents),
			transactional: transactional,
			idempotent:    idempotent,
		})
	}

	slices.SortFunc(migrations, func(a, b migration) int {
		if a.version < b.version {
			return -1
		}
		if a.version > b.version {
			return 1
		}
		return 0
	})
	return migrations, nil
}

func parseMigrationDirective(contents []byte) (transactional bool, idempotent bool, err error) {
	lines := strings.Split(string(contents), "\n")
	firstLine := strings.TrimSuffix(lines[0], "\r")
	if firstLine == nonTransactionalDirective {
		return false, true, nil
	}
	if strings.HasPrefix(firstLine, "-- nmcp:") {
		return false, false, errors.New("invalid migration directive; transaction=off requires explicit idempotent=true")
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(strings.TrimSuffix(line, "\r"), "-- nmcp:") {
			return false, false, errors.New("migration directive must be the first line")
		}
	}
	return true, false, nil
}

func parseMigrationFilename(filename string) (int64, string, error) {
	if !strings.HasSuffix(filename, ".sql") {
		return 0, "", fmt.Errorf("invalid migration filename %q", filename)
	}
	stem := strings.TrimSuffix(filename, ".sql")
	versionText, name, found := strings.Cut(stem, "_")
	if !found || versionText == "" || name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return 0, "", fmt.Errorf("invalid migration filename %q", filename)
	}
	if name[0] < 'a' || name[0] > 'z' {
		return 0, "", fmt.Errorf("invalid migration filename %q", filename)
	}
	for _, digit := range versionText {
		if digit < '0' || digit > '9' {
			return 0, "", fmt.Errorf("invalid migration filename %q", filename)
		}
	}
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("invalid migration version in %q", filename)
	}
	return version, name, nil
}

func checksumSQL(sql []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(sql))
}

func expectedVersion(migrations []migration) int64 {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

func currentVersion(applied []appliedMigration) int64 {
	var current int64
	for _, migration := range applied {
		if migration.dirty {
			break
		}
		current = migration.version
	}
	return current
}
