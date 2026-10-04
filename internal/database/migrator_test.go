package database

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseMigrationFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filename string
		version  int64
		name     string
		wantErr  bool
	}{
		{filename: "0001_initial_schema.sql", version: 1, name: "initial_schema"},
		{filename: "42_add-index.sql", version: 42, name: "add-index"},
		{filename: "0_zero.sql", wantErr: true},
		{filename: "one_name.sql", wantErr: true},
		{filename: "1_Name.sql", wantErr: true},
		{filename: "1_.sql", wantErr: true},
		{filename: "1_name.txt", wantErr: true},
		{filename: "name.sql", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.filename, func(t *testing.T) {
			version, name, err := parseMigrationFilename(test.filename)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationFilename() error = %v", err)
			}
			if version != test.version || name != test.name {
				t.Fatalf("parseMigrationFilename() = (%d, %q), want (%d, %q)", version, name, test.version, test.name)
			}
		})
	}
}

func TestChecksumSQL(t *testing.T) {
	t.Parallel()

	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := checksumSQL([]byte("abc")); got != want {
		t.Fatalf("checksumSQL() = %q, want %q", got, want)
	}
}

func TestDiscoverMigrations(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"migrations/README.md":          {Data: []byte("documentation")},
		"migrations/0010_add_index.sql": {Data: []byte(nonTransactionalDirective + "\nCREATE INDEX example;")},
		"migrations/0002_create.sql":    {Data: []byte("CREATE TABLE example();")},
	}
	migrations, err := discoverMigrations(fsys, "migrations")
	if err != nil {
		t.Fatalf("discoverMigrations() error = %v", err)
	}
	if len(migrations) != 2 {
		t.Fatalf("len(migrations) = %d, want 2", len(migrations))
	}
	if migrations[0].version != 2 || migrations[1].version != 10 {
		t.Fatalf("migration versions = [%d, %d], want [2, 10]", migrations[0].version, migrations[1].version)
	}
	if migrations[0].checksum != checksumSQL(fsys["migrations/0002_create.sql"].Data) {
		t.Fatal("discovered checksum does not match migration contents")
	}
	if !migrations[0].transactional || migrations[0].idempotent {
		t.Fatal("migration without a directive should be transactional")
	}
	if migrations[1].transactional || !migrations[1].idempotent {
		t.Fatal("transaction-off migration metadata was not discovered")
	}

	fsys["migrations/0002_create.sql"].Data[0] = 'X'
	if migrations[0].sql != "CREATE TABLE example();" {
		t.Fatal("discovered migration changed after its source filesystem was mutated")
	}
}

func TestParseMigrationDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		sql           string
		transactional bool
		idempotent    bool
		wantErr       bool
	}{
		{name: "default", sql: "SELECT 1", transactional: true},
		{name: "transaction off", sql: nonTransactionalDirective + "\nCREATE INDEX CONCURRENTLY example ON photos (id)", idempotent: true},
		{name: "CRLF", sql: nonTransactionalDirective + "\r\nSELECT 1", idempotent: true},
		{name: "missing idempotent", sql: "-- nmcp:transaction=off\nSELECT 1", wantErr: true},
		{name: "wrong idempotent value", sql: "-- nmcp:transaction=off idempotent=false\nSELECT 1", wantErr: true},
		{name: "extra option", sql: nonTransactionalDirective + " extra=true\nSELECT 1", wantErr: true},
		{name: "not first line", sql: "-- explanation\n" + nonTransactionalDirective + "\nSELECT 1", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transactional, idempotent, err := parseMigrationDirective([]byte(test.sql))
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationDirective() error = %v", err)
			}
			if transactional != test.transactional || idempotent != test.idempotent {
				t.Fatalf("parseMigrationDirective() = (%t, %t), want (%t, %t)", transactional, idempotent, test.transactional, test.idempotent)
			}
		})
	}
}

func TestDiscoverMigrationsRejectsDuplicateVersions(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"migrations/1_first.sql":   {Data: []byte("SELECT 1")},
		"migrations/01_second.sql": {Data: []byte("SELECT 2")},
	}
	if _, err := discoverMigrations(fsys, "migrations"); err == nil {
		t.Fatal("discoverMigrations() expected duplicate version error")
	}
}

func TestDiscoverMigrationsRejectsTransactionControl(t *testing.T) {
	t.Parallel()

	statements := []string{
		"BEGIN",
		"START /* gap */ TRANSACTION",
		"COMMIT",
		"END",
		"ROLLBACK",
		"ABORT",
		"SAVEPOINT example",
		"RELEASE SAVEPOINT example",
		"PREPARE TRANSACTION 'example'",
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			fsys := fstest.MapFS{"migrations/1_unsafe.sql": {Data: []byte("SELECT 1; " + statement)}}
			if _, err := discoverMigrations(fsys, "migrations"); err == nil {
				t.Fatal("discoverMigrations() expected transaction-control error")
			}
		})
	}
}

func TestDiscoverMigrationsRejectsUnicodeIdentifierDollarQuoteBypass(t *testing.T) {
	t.Parallel()
	// The $x$ fragments are part of a non-ASCII PostgreSQL identifier, not a
	// dollar-quoted body. COMMIT must therefore remain a visible second statement.
	sql := "SELECT 1 AS é$x$; COMMIT; SELECT 1 AS é$x$;"
	fsys := fstest.MapFS{"migrations/1_unsafe.sql": {Data: []byte(sql)}}
	if _, err := discoverMigrations(fsys, "migrations"); err == nil {
		t.Fatal("discoverMigrations() accepted Unicode identifier transaction-control bypass")
	}
}

func TestDiscoverMigrationsIgnoresQuotedTransactionWords(t *testing.T) {
	t.Parallel()

	sql := `-- COMMIT;
/* ROLLBACK; /* START TRANSACTION; */ ABORT; */
CREATE FUNCTION transaction_words() RETURNS text
LANGUAGE plpgsql AS $body$
BEGIN
  RETURN E'COMMIT;\' still text';
END;
$body$;
COMMENT ON FUNCTION transaction_words() IS 'ROLLBACK; ''BEGIN''';
SELECT "COMMIT", 'END; SAVEPOINT'`
	fsys := fstest.MapFS{"migrations/1_safe.sql": {Data: []byte(sql)}}
	migrations, err := discoverMigrations(fsys, "migrations")
	if err != nil {
		t.Fatalf("discoverMigrations() error = %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("len(migrations) = %d, want 1", len(migrations))
	}
	statements, err := scanSQLStatements([]byte(sql))
	if err != nil {
		t.Fatalf("scanSQLStatements() error = %v", err)
	}
	if len(statements) != 3 {
		t.Fatalf("len(scanSQLStatements()) = %d, want 3", len(statements))
	}
}

func TestDiscoverMigrationsValidatesTransactionOffStatementCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{
			name: "one statement with comments and trailing semicolon",
			sql:  nonTransactionalDirective + "\n/* ; */ CREATE INDEX CONCURRENTLY example ON photos (id); -- trailing",
		},
		{name: "no statement", sql: nonTransactionalDirective + "\n-- only a comment", wantErr: true},
		{name: "multiple statements", sql: nonTransactionalDirective + "\nSELECT 1; SELECT 2", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := fstest.MapFS{"migrations/1_index.sql": {Data: []byte(test.sql)}}
			_, err := discoverMigrations(fsys, "migrations")
			if (err != nil) != test.wantErr {
				t.Fatalf("discoverMigrations() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestDiscoverMigrationsRejectsEmptyTransactionalFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"migrations/1_empty.sql": {Data: []byte("-- comments are not a migration\n/* still empty */")}}
	if _, err := discoverMigrations(fsys, "migrations"); err == nil {
		t.Fatal("discoverMigrations() expected empty migration error")
	}
}

func TestDiscoverMigrationsFailsClosedOnUnterminatedLexicalRegions(t *testing.T) {
	t.Parallel()

	tests := []string{
		"SELECT 'secret value",
		`SELECT E'secret value\'`,
		`SELECT "secret identifier`,
		"SELECT 1 /* secret comment",
		"SELECT $body$secret body",
	}
	for _, sql := range tests {
		fsys := fstest.MapFS{"migrations/1_invalid.sql": {Data: []byte(sql)}}
		_, err := discoverMigrations(fsys, "migrations")
		if err == nil {
			t.Fatalf("discoverMigrations(%q) expected an error", sql)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("discoverMigrations() error exposed migration SQL: %v", err)
		}
	}
}

func TestValidateHistory(t *testing.T) {
	t.Parallel()

	migrations := []migration{
		{version: 1, name: "first", checksum: "checksum-1", sql: "SELECT 1", transactional: true},
		{version: 3, name: "third", checksum: "checksum-3", sql: "SELECT 3", transactional: true},
	}
	tests := []struct {
		name        string
		applied     []appliedMigration
		wantPending int
		wantErr     error
	}{
		{name: "empty", wantPending: 2},
		{
			name:        "valid prefix",
			applied:     []appliedMigration{{version: 1, name: "first", checksum: "checksum-1", transactional: true}},
			wantPending: 1,
		},
		{
			name: "complete",
			applied: []appliedMigration{
				{version: 1, name: "first", checksum: "checksum-1", transactional: true},
				{version: 3, name: "third", checksum: "checksum-3", transactional: true},
			},
		},
		{
			name:    "unknown version",
			applied: []appliedMigration{{version: 2, name: "second", checksum: "checksum-2"}},
			wantErr: ErrUnknownAppliedVersion,
		},
		{
			name:    "checksum drift",
			applied: []appliedMigration{{version: 1, name: "first", checksum: "changed", transactional: true}},
			wantErr: ErrChecksumMismatch,
		},
		{
			name:    "renamed migration",
			applied: []appliedMigration{{version: 1, name: "renamed", checksum: "checksum-1", transactional: true}},
			wantErr: ErrInvalidMigrationHistory,
		},
		{
			name:    "non-prefix downgrade shape",
			applied: []appliedMigration{{version: 3, name: "third", checksum: "checksum-3", transactional: true}},
			wantErr: ErrInvalidMigrationHistory,
		},
		{
			name:    "execution metadata drift",
			applied: []appliedMigration{{version: 1, name: "first", checksum: "checksum-1"}},
			wantErr: ErrInvalidMigrationHistory,
		},
		{
			name:    "dirty transactional migration",
			applied: []appliedMigration{{version: 1, name: "first", checksum: "checksum-1", transactional: true, dirty: true}},
			wantErr: ErrDirtyMigration,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pending, err := validateHistory(migrations, test.applied)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("validateHistory() error = %v, want %v", err, test.wantErr)
			}
			if err == nil && len(pending) != test.wantPending {
				t.Fatalf("len(pending) = %d, want %d", len(pending), test.wantPending)
			}
		})
	}
}

func TestPlanHistoryRecoversOnlyIdempotentNonTransactionalTail(t *testing.T) {
	t.Parallel()

	migrations := []migration{
		{version: 1, name: "first", checksum: "checksum-1", transactional: true},
		{version: 2, name: "index", checksum: "checksum-2", idempotent: true},
		{version: 3, name: "third", checksum: "checksum-3", transactional: true},
	}
	applied := []appliedMigration{
		{version: 1, name: "first", checksum: "checksum-1", transactional: true},
		{version: 2, name: "index", checksum: "checksum-2", idempotent: true, dirty: true},
	}

	plan, err := planHistory(migrations, applied, true)
	if err != nil {
		t.Fatalf("planHistory() error = %v", err)
	}
	if plan.dirty == nil || plan.dirty.version != 2 {
		t.Fatalf("dirty migration = %+v, want version 2", plan.dirty)
	}
	if len(plan.pending) != 1 || plan.pending[0].version != 3 {
		t.Fatalf("pending migrations = %+v, want version 3", plan.pending)
	}

	nonTail := append(applied, appliedMigration{version: 3, name: "third", checksum: "checksum-3", transactional: true})
	if _, err := planHistory(migrations, nonTail, true); !errors.Is(err, ErrInvalidMigrationHistory) {
		t.Fatalf("planHistory(non-tail dirty) error = %v, want ErrInvalidMigrationHistory", err)
	}

	nonIdempotent := []migration{{version: 1, name: "unsafe", checksum: "checksum", transactional: false}}
	nonIdempotentHistory := []appliedMigration{{version: 1, name: "unsafe", checksum: "checksum", dirty: true}}
	if _, err := planHistory(nonIdempotent, nonIdempotentHistory, true); !errors.Is(err, ErrDirtyMigration) {
		t.Fatalf("planHistory(non-idempotent dirty) error = %v, want ErrDirtyMigration", err)
	}
}
