package reconciliation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigrate "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
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

func TestCheckerIntegrationRejectsOlderSameVersionCurrent(t *testing.T) {
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
	mediaID := "56789abc-def0-4123-8456-789abcdef012"
	originalID := "01234567-89ab-4cde-8f01-23456789abcd"
	profileID := "11111111-1111-4111-8111-111111111111"
	jobs := []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}
	targets := []string{"44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"}
	renditions := []string{"66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"}
	times := []time.Time{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)}
	parameters := profile.StandardV1Parameters()
	jpegRecipe := parameters.Recipes["image/jpeg"]
	parameters.Recipes = map[string]profile.Recipe{"image/jpeg": jpegRecipe}
	profileParameters, err := json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.Rollback(ctx) }()
	statements := []struct {
		sql  string
		args []any
	}{
		{`ALTER TABLE jobs DISABLE TRIGGER USER`, nil}, {`ALTER TABLE job_targets DISABLE TRIGGER USER`, nil}, {`ALTER TABLE renditions DISABLE TRIGGER USER`, nil},
		{`INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence) VALUES('70000000-0000-4000-8000-000000000001','nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,'reconciliation test')`, nil},
		{`INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, []any{mediaID}},
		{`INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES($1::nmcp_uuid_v4,$2,repeat('0',64),'originals/01/'||$1::nmcp_uuid_v4::text||'/original.jpg','image/jpeg',1)`, []any{originalID, mediaID}},
		{`INSERT INTO profiles(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters) VALUES($1,'same',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, []any{profileID, profileParameters}},
		{`INSERT INTO jobs(id,type,original_id,media_id_snapshot,status,attempts,max_attempts,started_at,finished_at) VALUES($1,'transform',$2,$3,'succeeded',1,3,$4,$4),($5,'transform',$2,$3,'succeeded',1,3,$6,$6)`, []any{jobs[0], originalID, mediaID, times[0], jobs[1], times[1]}},
		{`INSERT INTO job_targets(id,job_id,profile_id,status,attempts) VALUES($1,$2,$3,'succeeded',1),($4,$5,$3,'succeeded',1)`, []any{targets[0], jobs[0], profileID, targets[1], jobs[1]}},
		{`INSERT INTO renditions(id,media_id,job_target_id,profile_key,is_current,purge_after,relative_path,mime_type,size_bytes,sha256,created_at,processor_audit) VALUES ($1::nmcp_uuid_v4,$2,$3::nmcp_uuid_v4,'same',false,$4::timestamptz+interval '1 day','renditions/01/'||$5::nmcp_uuid_v4::text||'/'||$3::nmcp_uuid_v4::text||'/'||$1::nmcp_uuid_v4::text||'.webp','image/webp',1,repeat('1',64),$6,'{"fixture":"reconciliation"}'), ($7::nmcp_uuid_v4,$2,$8::nmcp_uuid_v4,'same',true,NULL,'renditions/01/'||$5::nmcp_uuid_v4::text||'/'||$8::nmcp_uuid_v4::text||'/'||$7::nmcp_uuid_v4::text||'.webp','image/webp',1,repeat('2',64),$4,'{"fixture":"reconciliation"}')`, []any{renditions[0], mediaID, targets[0], times[1], originalID, times[0], renditions[1], targets[1]}},
		{`INSERT INTO change_events(id,position,event_type,reason,media_id,payload,occurred_at) VALUES ('88888888-8888-4888-8888-888888888888',1,'media_upsert','rendition_current',$1,jsonb_build_object('current_renditions',jsonb_build_array(jsonb_build_object('id',$2::text,'profile',jsonb_build_object('key','same')))),$3), ('99999999-9999-4999-8999-999999999999',2,'media_upsert','rendition_current',$1,jsonb_build_object('current_renditions',jsonb_build_array(jsonb_build_object('id',$4::text,'profile',jsonb_build_object('key','same')))),$5)`, []any{mediaID, renditions[0], times[0], renditions[1], times[1]}},
		{`ALTER TABLE jobs ENABLE TRIGGER USER`, nil}, {`ALTER TABLE job_targets ENABLE TRIGGER USER`, nil}, {`ALTER TABLE renditions ENABLE TRIGGER USER`, nil},
	}
	for _, statement := range statements {
		if _, err := fixture.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture statement %q: %v", statement.sql, err)
		}
	}
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	readValidity := func() map[string]bool {
		tx, err := checkPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err := tx.Query(ctx, `SELECT subject_id::text,current_valid FROM nmcp_read_check_database_references(clock_timestamp()) WHERE row_kind='rendition' ORDER BY subject_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := map[string]bool{}
		for rows.Next() {
			var id string
			var valid bool
			if err := rows.Scan(&id, &valid); err != nil {
				t.Fatal(err)
			}
			result[id] = valid
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return result
	}
	valid := readValidity()
	if !valid[renditions[0]] || !valid[renditions[1]] {
		t.Fatalf("newer same-version current validity = %+v", valid)
	}
	dueTx, err := checkPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	var dueRenditions int
	if err := dueTx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_check_database_references(clock_timestamp()) WHERE row_kind='expired_rendition' AND subject_id=$1`, renditions[0]).Scan(&dueRenditions); err != nil {
		_ = dueTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := dueTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if dueRenditions != 1 {
		t.Fatalf("due rendition rows = %d, want 1", dueRenditions)
	}
	corrupt, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.Exec(ctx, `UPDATE renditions SET is_current=false,purge_after=clock_timestamp() WHERE id=$1`, renditions[1]); err == nil {
		_, err = corrupt.Exec(ctx, `UPDATE renditions SET is_current=true,purge_after=NULL WHERE id=$1`, renditions[0])
	}
	if err != nil {
		_ = corrupt.Rollback(ctx)
		t.Fatal(err)
	}
	if err := corrupt.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	invalid := readValidity()
	if invalid[renditions[0]] || invalid[renditions[1]] {
		t.Fatalf("older same-version current validity = %+v", invalid)
	}
	if _, err := admin.Exec(ctx, `DROP INDEX renditions_one_current_key_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE renditions SET is_current=true,purge_after=NULL WHERE id=$1`, renditions[1]); err != nil {
		t.Fatal(err)
	}
	multiple := readValidity()
	if multiple[renditions[0]] || multiple[renditions[1]] {
		t.Fatalf("multiple-current validity = %+v", multiple)
	}
	profileV2 := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	jobV2 := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	targetV2 := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	renditionV2 := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	timeV2 := times[1].Add(time.Second)
	v2, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v2Statements := []struct {
		sql  string
		args []any
	}{
		{`ALTER TABLE jobs DISABLE TRIGGER USER`, nil}, {`ALTER TABLE job_targets DISABLE TRIGGER USER`, nil}, {`ALTER TABLE renditions DISABLE TRIGGER USER`, nil},
		{`UPDATE renditions SET is_current=false,purge_after=$1 WHERE media_id=$2`, []any{timeV2.Add(time.Hour), mediaID}},
		{`INSERT INTO profiles(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters) VALUES($1,'same',2,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, []any{profileV2, profileParameters}},
		{`INSERT INTO jobs(id,type,original_id,media_id_snapshot,status,attempts,max_attempts,started_at,finished_at) VALUES($1,'transform',$2,$3,'succeeded',1,3,$4,$4)`, []any{jobV2, originalID, mediaID, timeV2}},
		{`INSERT INTO job_targets(id,job_id,profile_id,status,attempts) VALUES($1,$2,$3,'succeeded',1)`, []any{targetV2, jobV2, profileV2}},
		{`INSERT INTO renditions(id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,created_at,processor_audit) VALUES($1,$2,$3,'same',true,'renditions/01/'||$4::nmcp_uuid_v4::text||'/'||$3::nmcp_uuid_v4::text||'/'||$1::nmcp_uuid_v4::text||'.webp','image/webp',1,repeat('3',64),$5,'{"fixture":"reconciliation-v2"}')`, []any{renditionV2, mediaID, targetV2, originalID, timeV2}},
		{`ALTER TABLE jobs ENABLE TRIGGER USER`, nil}, {`ALTER TABLE job_targets ENABLE TRIGGER USER`, nil}, {`ALTER TABLE renditions ENABLE TRIGGER USER`, nil},
		{`INSERT INTO change_events(id,position,event_type,reason,media_id,payload,occurred_at) VALUES('eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',3,'media_upsert','rendition_current',$1,jsonb_build_object('current_renditions',jsonb_build_array(jsonb_build_object('id',$2::text,'profile',jsonb_build_object('key','same')))),$3)`, []any{mediaID, renditionV2, timeV2}},
	}
	for _, statement := range v2Statements {
		if _, err := v2.Exec(ctx, statement.sql, statement.args...); err != nil {
			_ = v2.Rollback(ctx)
			t.Fatalf("v2 fixture %q: %v", statement.sql, err)
		}
	}
	if err := v2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	newerVersion := readValidity()
	for id, valid := range newerVersion {
		if !valid {
			t.Fatalf("newer-version current validity[%s] = false: %+v", id, newerVersion)
		}
	}
	if _, err := admin.Exec(ctx, `UPDATE renditions SET is_current=false,purge_after=clock_timestamp() WHERE id=$1`, renditionV2); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE renditions SET is_current=true,purge_after=NULL WHERE id=$1`, renditions[1]); err != nil {
		t.Fatal(err)
	}
	olderVersion := readValidity()
	for id, valid := range olderVersion {
		if valid {
			t.Fatalf("older-version current validity[%s] = true: %+v", id, olderVersion)
		}
	}
	var tableSelect, requiredColumn, forbiddenColumn bool
	if err := admin.QueryRow(ctx, `SELECT has_table_privilege('nmcp_check_function_owner','change_events','SELECT'),has_column_privilege('nmcp_check_function_owner','change_events','position','SELECT'),has_column_privilege('nmcp_check_function_owner','change_events','id','SELECT')`).Scan(&tableSelect, &requiredColumn, &forbiddenColumn); err != nil {
		t.Fatal(err)
	}
	if tableSelect || !requiredColumn || forbiddenColumn {
		t.Fatalf("change_events privileges table/required/unused = %t/%t/%t", tableSelect, requiredColumn, forbiddenColumn)
	}
	var manifestTable, manifestRequired, manifestForbidden, manifestWrite bool
	if err := admin.QueryRow(ctx, `SELECT has_table_privilege('nmcp_check_function_owner','reconciliation_repair_manifest_items','SELECT'),has_column_privilege('nmcp_check_function_owner','reconciliation_repair_manifest_items','quarantine_id','SELECT'),has_column_privilege('nmcp_check_function_owner','reconciliation_repair_manifest_items','run_id','SELECT'),has_table_privilege('nmcp_check_function_owner','reconciliation_repair_manifest_items','INSERT')`).Scan(&manifestTable, &manifestRequired, &manifestForbidden, &manifestWrite); err != nil {
		t.Fatal(err)
	}
	if manifestTable || !manifestRequired || manifestForbidden || manifestWrite {
		t.Fatalf("repair-manifest privileges table/required/unused/write = %t/%t/%t/%t", manifestTable, manifestRequired, manifestForbidden, manifestWrite)
	}
}

func TestCheckerIntegrationPreservesPartialDatabaseSourceFailure(t *testing.T) {
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
	dueMediaID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := admin.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source,deleted_at,purge_after) VALUES($1,'image/jpeg','unknown',clock_timestamp()-interval '2 days',clock_timestamp()-interval '1 day')`, dueMediaID); err != nil {
		t.Fatal(err)
	}
	invalidMediaID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	invalidOriginalID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := admin.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, invalidMediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES($1,$2,repeat('0',64),'invalid/path','image/jpeg',1)`, invalidOriginalID, invalidMediaID); err != nil {
		t.Fatal(err)
	}
	purgedMediaID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	purgeJobID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	purgedObjectID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	fixture, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`ALTER TABLE jobs DISABLE TRIGGER USER`, nil},
		{`INSERT INTO jobs(id,type,media_id_snapshot,status,attempts,max_attempts,started_at,finished_at) VALUES($1,'purge',$2,'succeeded',1,3,clock_timestamp(),clock_timestamp())`, []any{purgeJobID, purgedMediaID}},
		{`ALTER TABLE jobs ENABLE TRIGGER USER`, nil},
		{`ALTER TABLE media_purge_identity_guard DISABLE TRIGGER USER`, nil},
		{`INSERT INTO media_purge_identity_guard(media_id,state,purge_job_id) VALUES($1,'purged',$2)`, []any{purgedMediaID, purgeJobID}},
		{`ALTER TABLE media_purge_identity_guard ENABLE TRIGGER USER`, nil},
		{`ALTER TABLE purge_file_progress DISABLE TRIGGER USER`, nil},
		{`INSERT INTO purge_file_progress(job_id,media_id_snapshot,object_kind,object_id,relative_path,size_bytes,disposition,completed_at) VALUES($1,$2,'original',$3,'originals/ff/'||$3::nmcp_uuid_v4::text||'/original.jpg',1,'deleted',clock_timestamp())`, []any{purgeJobID, purgedMediaID, purgedObjectID}},
		{`ALTER TABLE purge_file_progress ENABLE TRIGGER USER`, nil},
	} {
		if _, err := fixture.Exec(ctx, statement.sql, statement.args...); err != nil {
			_ = fixture.Rollback(ctx)
			t.Fatalf("terminal fixture %q: %v", statement.sql, err)
		}
	}
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE OR REPLACE FUNCTION nmcp_read_check_attempt_owners() RETURNS TABLE(attempt_id uuid,attempt_kind text,coverage text,original_id uuid,job_id uuid,temp_relative_key text,event_type text,lease_expires_at timestamptz,event_occurred_at timestamptz) LANGUAGE plpgsql SECURITY DEFINER SET search_path=`+pgx.Identifier{schema}.Sanitize()+`,pg_catalog,pg_temp AS $$ BEGIN RAISE EXCEPTION 'injected attempt reader failure' USING ERRCODE='58000'; END; $$`); err != nil {
		t.Fatal(err)
	}
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	repository, _ := NewPostgresRepository(checkPool)
	capture, err := repository.CaptureSnapshot(ctx)
	if err != nil || capture.DatabaseReferencesError != nil || capture.AttemptOwnersError == nil || capture.EndedAt.IsZero() {
		t.Fatalf("CaptureSnapshot() = %+v, %v", capture, err)
	}
	var sawDueMedia, sawInvalidKey, sawTerminal bool
	for _, reference := range capture.References {
		if reference.Kind == "expired_media" && reference.SubjectID == dueMediaID {
			sawDueMedia = true
		}
		if reference.Kind == "original" && reference.SubjectID == invalidOriginalID && !validReferenceKey(reference) {
			sawInvalidKey = true
		}
		if reference.Kind == "purged_original" && reference.SubjectID == purgedObjectID {
			sawTerminal = true
		}
	}
	if !sawDueMedia || !sawInvalidKey || !sawTerminal {
		t.Fatalf("database reference source coverage due/invalid/terminal=%t/%t/%t: %+v", sawDueMedia, sawInvalidKey, sawTerminal, capture.References)
	}
	scan := &countingScanner{}
	checker, _ := NewChecker(repository, scan)
	result, checkErr := checker.Check(ctx, ScopeAll)
	if checkErr == nil || result.ReportID == "" || result.Sealed || scan.calls != 0 {
		t.Fatalf("Check() = %+v, %v; scans=%d", result, checkErr, scan.calls)
	}
	rows, err := admin.Query(ctx, `SELECT source,result,error_code FROM reconciliation_check_source_results WHERE report_id=$1 ORDER BY source`, result.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var source, result string
		var code *string
		if err := rows.Scan(&source, &result, &code); err != nil {
			t.Fatal(err)
		}
		if code == nil {
			got[source] = result
		} else {
			got[source] = result + ":" + *code
		}
	}
	if got["database_references"] != "complete" || got["attempt_owners"] != "error:attempt_owners" || got["storage_scan"] != "error:database_source_failed" {
		t.Fatalf("source outcomes = %+v; Check error = %v", got, checkErr)
	}
	if err := repository.CompleteSource(ctx, result.ReportID, "attempt_owners", stringPointer("attempt_owners")); err != nil {
		t.Fatalf("exact source replay: %v", err)
	}
	if err := repository.CompleteSource(ctx, result.ReportID, "attempt_owners", stringPointer("different_error")); err == nil {
		t.Fatal("contradictory source replay succeeded")
	}
}

func TestCheckerIntegrationPreservesAttemptSourceWhenReferenceReaderFails(t *testing.T) {
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
	definition := `CREATE OR REPLACE FUNCTION nmcp_read_check_database_references(check_cutoff timestamptz) RETURNS TABLE(row_kind text,subject_id uuid,media_id uuid,original_id uuid,job_id uuid,job_target_id uuid,relative_key text,expected_state text,size_bytes bigint,sha256 text,is_current boolean,provenance_valid boolean,current_valid boolean,due_at timestamptz) LANGUAGE plpgsql SECURITY DEFINER SET search_path=` + pgx.Identifier{schema}.Sanitize() + `,pg_catalog,pg_temp AS $$ BEGIN RAISE EXCEPTION 'injected reference reader failure' USING ERRCODE='58000'; END; $$`
	if _, err := admin.Exec(ctx, definition); err != nil {
		t.Fatal(err)
	}
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	repository, _ := NewPostgresRepository(checkPool)
	capture, err := repository.CaptureSnapshot(ctx)
	if err != nil || capture.DatabaseReferencesError == nil || capture.AttemptOwnersError != nil || capture.EndedAt.IsZero() {
		t.Fatalf("CaptureSnapshot() = %+v, %v", capture, err)
	}
	checker, _ := NewChecker(repository, &countingScanner{})
	result, checkErr := checker.Check(ctx, ScopeAll)
	if checkErr == nil || result.ReportID == "" || result.Sealed {
		t.Fatalf("Check() = %+v, %v", result, checkErr)
	}
	rows, err := admin.Query(ctx, `SELECT source,result,error_code FROM reconciliation_check_source_results WHERE report_id=$1`, result.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var source, result string
		var code *string
		if err := rows.Scan(&source, &result, &code); err != nil {
			t.Fatal(err)
		}
		got[source] = result
		if code != nil {
			got[source] += "-" + *code
		}
	}
	if got["database_references"] != "error-database_references" || got["attempt_owners"] != "complete" || got["storage_scan"] != "error-database_source_failed" {
		t.Fatalf("source outcomes = %+v", got)
	}
}

func TestCheckerIntegrationSealsStableHardlinkFinding(t *testing.T) {
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
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mediaID := "56789abc-def0-4123-8456-789abcdef012"
	originalText := "01234567-89ab-4cde-8f01-23456789abcd"
	originalID, _ := storage.ParseOriginalID(originalText)
	key, _ := storage.NewOriginalKey(originalID, storage.OriginalJPEG)
	payload := []byte("linked original")
	digest := sha256.Sum256(payload)
	publishIntegrationOriginal(t, store, key, "3456789a-bcde-4f01-8234-56789abcdef0", payload)
	if _, err := admin.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES($1,$2,$3,$4,'image/jpeg',$5)`, originalText, mediaID, hex.EncodeToString(digest[:]), key.String(), len(payload)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-link")
	if err := os.Link(filepath.Join(root, filepath.FromSlash(key.String())), outside); err != nil {
		t.Fatal(err)
	}
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	repository, _ := NewPostgresRepository(checkPool)
	now, err := repository.DatabaseNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayReport := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	replaySnapshot := Snapshot{StartedAt: now, CutoffAt: now, EndedAt: now}
	if err := repository.BeginReport(ctx, replayReport, ScopeAll, replaySnapshot, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginReport(ctx, replayReport, ScopeAll, replaySnapshot, now); err != nil {
		t.Fatalf("exact begin replay: %v", err)
	}
	replayFinding := Finding{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Kind: "unexpected_hardlink", Reason: "regular_inode_has_multiple_links", SubjectType: "path", RelativeKey: stringPointer("linked"), ObservedType: "regular", ObservedAt: now}
	firstOrdinal, err := repository.AppendFinding(ctx, replayReport, replayFinding)
	if err != nil {
		t.Fatal(err)
	}
	secondOrdinal, err := repository.AppendFinding(ctx, replayReport, replayFinding)
	if err != nil {
		t.Fatalf("exact finding replay: %v", err)
	}
	if firstOrdinal != 1 || secondOrdinal != 1 {
		t.Fatalf("replayed ordinals = %d/%d", firstOrdinal, secondOrdinal)
	}
	conflict := replayFinding
	conflict.Reason = "different_reason"
	if _, err := repository.AppendFinding(ctx, replayReport, conflict); err == nil {
		t.Fatal("contradictory finding replay succeeded")
	}
	for _, source := range []string{"database_references", "attempt_owners", "storage_scan"} {
		if err := repository.CompleteSource(ctx, replayReport, source, nil); err != nil {
			t.Fatal(err)
		}
		if err := repository.CompleteSource(ctx, replayReport, source, nil); err != nil {
			t.Fatalf("exact source completion replay for %s: %v", source, err)
		}
	}
	code := "contradictory"
	if err := repository.CompleteSource(ctx, replayReport, "storage_scan", &code); err == nil {
		t.Fatal("contradictory source completion replay succeeded")
	}
	if err := repository.FinalizeReport(ctx, replayReport, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinalizeReport(ctx, replayReport, 0, now); err == nil {
		t.Fatal("contradictory finalization replay succeeded")
	}
	checker, _ := NewChecker(repository, store)
	result, err := checker.Check(ctx, ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 1 {
		t.Fatalf("Check() = %+v, %v", result, err)
	}
	var kind string
	var observedSHA *string
	if err := admin.QueryRow(ctx, `SELECT kind,observed_sha256 FROM reconciliation_check_findings WHERE report_id=$1`, result.ReportID).Scan(&kind, &observedSHA); err != nil {
		t.Fatal(err)
	}
	if kind != "unexpected_hardlink" || observedSHA != nil {
		t.Fatalf("hardlink finding = %s/%v", kind, observedSHA)
	}
}

func TestCheckerIntegrationCorrelatesQuarantineJournal(t *testing.T) {
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
	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	repository, _ := NewPostgresRepository(checkPool)
	now, err := repository.DatabaseNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reportID := "11111111-1111-4111-8111-111111111111"
	findingID := "22222222-2222-4222-8222-222222222222"
	runID := "33333333-3333-4333-8333-333333333333"
	authorizationID := "44444444-4444-4444-8444-444444444444"
	itemID := "55555555-5555-4555-8555-555555555555"
	quarantineID := "66666666-6666-4666-8666-666666666666"
	attemptID := "77777777-7777-4777-8777-777777777777"
	eventID := "88888888-8888-4888-8888-888888888888"
	snapshot := Snapshot{StartedAt: now, CutoffAt: now, EndedAt: now}
	if err := repository.BeginReport(ctx, reportID, ScopeAll, snapshot, now); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"database_references", "attempt_owners"} {
		if err := repository.CompleteSource(ctx, reportID, source, nil); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte("journaled")
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	size := int64(len(payload))
	old := now.Add(-49 * time.Hour)
	sourceKey := "originals/99/99999999-9999-4999-8999-999999999999/original.jpg"
	finding := Finding{ID: findingID, Kind: "final_orphan", Reason: "unreferenced_canonical_final", SubjectType: "original", SubjectID: stringPointer("99999999-9999-4999-8999-999999999999"), RelativeKey: &sourceKey, ExpectedState: stringPointer("unreferenced"), ObservedType: "regular", ObservedSize: &size, ObservedSHA: &sha, ObservedMTime: &old, ObservedCTime: &old, ObservedAt: now}
	if _, err := repository.AppendFinding(ctx, reportID, finding); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinalizeReport(ctx, reportID, 1, now); err != nil {
		t.Fatal(err)
	}
	repairPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_repair_runtime")
	if _, err := repairPool.Exec(ctx, `SELECT nmcp_begin_repair_run($1,$2,$3,'integration')`, runID, reportID, authorizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := repairPool.Exec(ctx, `SELECT nmcp_prepare_repair_manifest_item($1,$2,$3,$4)`, itemID, runID, findingID, quarantineID); err != nil {
		t.Fatal(err)
	}
	fixture, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO reconciliation_repair_attempts(id,manifest_item_id,attempt_number) VALUES($1,$2,1)`, []any{attemptID, itemID}},
		{`ALTER TABLE reconciliation_repair_events DISABLE TRIGGER USER`, nil},
		{`INSERT INTO reconciliation_repair_events(id,attempt_id,sequence,event_type,outcome_code,observed_size_bytes,observed_sha256) VALUES($1,$2,1,'completed','quarantined',$3,$4)`, []any{eventID, attemptID, size, sha}},
		{`ALTER TABLE reconciliation_repair_events ENABLE TRIGGER USER`, nil},
	} {
		if _, err := fixture.Exec(ctx, statement.sql, statement.args...); err != nil {
			_ = fixture.Rollback(ctx)
			t.Fatalf("quarantine fixture %q: %v", statement.sql, err)
		}
	}
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".quarantine", quarantineID), []byte("mismatch"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	checker, _ := NewChecker(repository, store)
	assertSingleCheckFinding(t, ctx, admin, checker, "quarantine_mismatch")
	if err := os.Remove(filepath.Join(root, ".quarantine", quarantineID)); err != nil {
		t.Fatal(err)
	}
	assertSingleCheckFinding(t, ctx, admin, checker, "quarantine_missing")
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
