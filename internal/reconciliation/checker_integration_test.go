package reconciliation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigrate "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestCheckerIntegrationUsesExecuteOnlyRoleAndSealsDeterministicStoreEvidence(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, schema := reconciliationIntegrationPool(t, databaseURL)
	migrator, err := dbmigrate.NewMigrator(admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate reconciliation checker fixture: %v", err)
	}
	var checkReaders, runtimeDenied, repairDenied, publicDenied, directTablesDenied, fixedBoundary bool
	if err := admin.QueryRow(ctx, `SELECT
		has_function_privilege('nmcp_check_runtime','nmcp_read_check_database_references(timestamptz)','EXECUTE')
		  AND has_function_privilege('nmcp_check_runtime','nmcp_read_check_attempt_owners()','EXECUTE')
		  AND has_function_privilege('nmcp_check_runtime','nmcp_read_check_report_outcome(nmcp_uuid_v4)','EXECUTE'),
		NOT has_function_privilege('nmcp_runtime','nmcp_read_check_database_references(timestamptz)','EXECUTE')
		  AND NOT has_function_privilege('nmcp_worker_runtime','nmcp_read_check_attempt_owners()','EXECUTE')
		  AND NOT has_function_privilege('nmcp_runtime','nmcp_read_check_report_outcome(nmcp_uuid_v4)','EXECUTE'),
		NOT has_function_privilege('nmcp_repair_runtime','nmcp_read_check_database_references(timestamptz)','EXECUTE')
		  AND NOT has_function_privilege('nmcp_repair_runtime','nmcp_read_check_attempt_owners()','EXECUTE')
		  AND NOT has_function_privilege('nmcp_repair_runtime','nmcp_read_check_report_outcome(nmcp_uuid_v4)','EXECUTE'),
		NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc AS public_function
		  CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(public_function.proacl,pg_catalog.acldefault('f',public_function.proowner))) AS privilege
		  WHERE public_function.proname IN ('nmcp_read_check_database_references','nmcp_read_check_attempt_owners','nmcp_read_check_report_outcome')
		    AND privilege.grantee=0 AND privilege.privilege_type='EXECUTE'),
		NOT has_table_privilege('nmcp_check_runtime','originals','SELECT')
		  AND NOT has_table_privilege('nmcp_check_runtime','storage_attempts','SELECT')
		  AND NOT has_table_privilege('nmcp_check_runtime','reconciliation_check_findings','SELECT'),
		(SELECT count(*)=3 FROM pg_catalog.pg_proc AS p
		 JOIN pg_catalog.pg_roles AS owner ON owner.oid=p.proowner
		 WHERE p.proname IN ('nmcp_read_check_database_references','nmcp_read_check_attempt_owners','nmcp_read_check_report_outcome')
		   AND p.prosecdef AND owner.rolname='nmcp_check_function_owner'
		   AND p.proconfig=ARRAY[pg_catalog.format('search_path=%I, pg_catalog, pg_temp',$1::text)])`, schema).Scan(
		&checkReaders, &runtimeDenied, &repairDenied, &publicDenied, &directTablesDenied, &fixedBoundary); err != nil {
		t.Fatal(err)
	}
	if !checkReaders || !runtimeDenied || !repairDenied || !publicDenied || !directTablesDenied || !fixedBoundary {
		t.Fatalf("checker ACL readers/runtime/repair/public/tables/boundary = %t/%t/%t/%t/%t/%t",
			checkReaders, runtimeDenied, repairDenied, publicDenied, directTablesDenied, fixedBoundary)
	}

	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mediaID := "56789abc-def0-4123-8456-789abcdef012"
	originalIDText := "01234567-89ab-4cde-8f01-23456789abcd"
	originalID, _ := storage.ParseOriginalID(originalIDText)
	originalKey, _ := storage.NewOriginalKey(originalID, storage.OriginalJPEG)
	payload := []byte("referenced original")
	digest := sha256.Sum256(payload)
	publishIntegrationOriginal(t, store, originalKey, "3456789a-bcde-4f01-8234-56789abcdef0", payload)
	if _, err := admin.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes)
		VALUES($1,$2,$3,$4,'image/jpeg',$5)`, originalIDText, mediaID, hex.EncodeToString(digest[:]), originalKey.String(), len(payload)); err != nil {
		t.Fatal(err)
	}

	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	if _, err := checkPool.Exec(ctx, `SELECT count(*) FROM originals`); err == nil {
		t.Fatal("nmcp_check_runtime read originals directly")
	}
	repository, err := NewPostgresRepository(checkPool)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewChecker(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := checker.Check(ctx, ScopeAll)
	if err != nil || !clean.Sealed || clean.FindingCount != 0 {
		t.Fatalf("clean Check() = %+v, %v", clean, err)
	}
	leaf := filepath.Join(root, filepath.FromSlash(originalKey.String()))
	corrupt := append([]byte(nil), payload...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(leaf, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	assertSingleCheckFinding(t, ctx, admin, checker, "sha256_mismatch")
	if err := os.WriteFile(leaf, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSingleCheckFinding(t, ctx, admin, checker, "size_mismatch")
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	assertSingleCheckFinding(t, ctx, admin, checker, "referenced_missing")
	if err := os.WriteFile(leaf, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	orphanIDText := "6789abcd-ef01-4234-8567-89abcdef0123"
	orphanID, _ := storage.ParseOriginalID(orphanIDText)
	orphanKey, _ := storage.NewOriginalKey(orphanID, storage.OriginalPNG)
	publishIntegrationOriginal(t, store, orphanKey, "789abcde-f012-4345-8678-9abcdef01234", []byte("orphan"))
	orphan, err := checker.Check(ctx, ScopeAll)
	if err != nil || !orphan.Sealed || orphan.FindingCount != 1 {
		t.Fatalf("orphan Check() = %+v, %v", orphan, err)
	}
	var kind, actionability, relativeKey string
	if err := admin.QueryRow(ctx, `SELECT kind,actionability,relative_key
		FROM reconciliation_check_findings WHERE report_id=$1`, orphan.ReportID).Scan(&kind, &actionability, &relativeKey); err != nil {
		t.Fatal(err)
	}
	if kind != "final_orphan" || actionability != "repairable" || relativeKey != orphanKey.String() {
		t.Fatalf("persisted orphan = %s/%s/%s", kind, actionability, relativeKey)
	}
	var sources, seals int
	if err := admin.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM reconciliation_check_source_results WHERE report_id=$1 AND result='complete'),
		(SELECT count(*) FROM reconciliation_check_report_seals WHERE report_id=$1)`, orphan.ReportID).Scan(&sources, &seals); err != nil {
		t.Fatal(err)
	}
	if sources != 3 || seals != 1 {
		t.Fatalf("source/seal evidence = %d/%d", sources, seals)
	}

	// The derived reader obeys the caller's repeatable-read snapshot: a writer
	// committed after the cutoff is absent until the next checker transaction.
	tx, err := checkPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var cutoff time.Time
	var before, frozen, after int
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_check_database_references($1) WHERE row_kind='original'`, cutoff).Scan(&before); err != nil {
		t.Fatal(err)
	}
	concurrentMedia := "89abcdef-0123-4456-8789-abcdef012345"
	concurrentOriginal := "9abcdef0-1234-4567-889a-bcdef0123456"
	if _, err := admin.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, concurrentMedia); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes)
		VALUES($1,$2,$3,$4,'image/jpeg',1)`, concurrentOriginal, concurrentMedia,
		strings.Repeat("0", 64), "originals/9a/"+concurrentOriginal+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_check_database_references($1) WHERE row_kind='original'`, cutoff).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := checkPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_check_database_references(clock_timestamp()) WHERE row_kind='original'`).Scan(&after); err != nil {
		_ = next.Rollback(ctx)
		t.Fatal(err)
	}
	if err := next.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if frozen != before || after != before+1 {
		t.Fatalf("reference counts before/frozen/after = %d/%d/%d", before, frozen, after)
	}
}

func TestCheckerIntegrationDerivesRepairableTempOnlyFromExactOldTerminalOwner(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, schema := reconciliationIntegrationPool(t, databaseURL)
	migrator, err := dbmigrate.NewMigrator(admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	originalID := "01234567-89ab-4cde-8f01-23456789abcd"
	attemptID := "3456789a-bcde-4f01-8234-56789abcdef0"
	tempPath := "originals/01/" + originalID + "/.original." + attemptID + ".tmp"
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := admin.Exec(ctx, `INSERT INTO storage_attempts(id,kind,original_id,temp_relative_key,coverage)
		VALUES($1,'upload',$2,$3,'native')`, attemptID, originalID, tempPath); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at,occurred_at)
		VALUES($1,1,'registered',$2,$3),($1,2,'aborted',NULL,$3)`, attemptID, old.Add(time.Minute), old); err != nil {
		t.Fatal(err)
	}
	payload := []byte("abandoned temporary bytes")
	digest := sha256.Sum256(payload)
	size := int64(len(payload))
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	repository, _ := NewPostgresRepository(checkPool)
	checker, _ := NewChecker(repository, staticScanner{observations: []storage.Observation{{
		RelativeKey: tempPath, Type: storage.ObservationRegular, Size: &size, SHA256: &digest,
		ModifiedAt: &old, ChangedAt: &old, Stable: true,
	}}})
	result, err := checker.Check(ctx, ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 1 {
		t.Fatalf("Check() = %+v, %v", result, err)
	}
	var kind, reason, actionability string
	if err := admin.QueryRow(ctx, `SELECT kind,reason_code,actionability
		FROM reconciliation_check_findings WHERE report_id=$1`, result.ReportID).Scan(&kind, &reason, &actionability); err != nil {
		t.Fatal(err)
	}
	if kind != "aged_attempt_temp" || reason != "terminal_owner" || actionability != "repairable" {
		t.Fatalf("temp finding = %s/%s/%s", kind, reason, actionability)
	}
}

func assertSingleCheckFinding(t *testing.T, ctx context.Context, admin *pgxpool.Pool, checker *Checker, wantKind string) {
	t.Helper()
	result, err := checker.Check(ctx, ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 1 {
		t.Fatalf("Check(%s) = %+v, %v", wantKind, result, err)
	}
	var kind string
	if err := admin.QueryRow(ctx, `SELECT kind FROM reconciliation_check_findings WHERE report_id=$1`, result.ReportID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != wantKind {
		t.Fatalf("finding kind = %s, want %s", kind, wantKind)
	}
}

func publishIntegrationOriginal(t *testing.T, store *storage.Store, key storage.OriginalKey, attemptText string, payload []byte) {
	t.Helper()
	attempt, err := storage.ParseAttemptID(attemptText)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := store.BeginOriginal(context.Background(), key, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write(payload); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := temporary.Publish(context.Background(), storage.Validation{ExpectedSize: int64(len(payload)), ExpectedSHA256: &digest}); err != nil {
		t.Fatal(err)
	}
}

func reconciliationIntegrationPool(t *testing.T, databaseURL string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "nmcp_reconciliation_test_" + hex.EncodeToString(random)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop reconciliation schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = identifier
	config.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, schema
}

func reconciliationRolePool(t *testing.T, databaseURL, schema, role string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
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

type staticScanner struct{ observations []storage.Observation }

func (scanner staticScanner) Scan(context.Context) ([]storage.Observation, error) {
	return scanner.observations, nil
}
