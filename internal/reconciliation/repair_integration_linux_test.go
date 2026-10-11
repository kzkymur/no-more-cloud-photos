//go:build linux

package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	dbmigrate "github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"golang.org/x/sys/unix"
)

func TestRepairIntegrationCheckerDryRunApplyChecker(t *testing.T) {
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
	if err = migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	originalID := "99999999-9999-4999-8999-999999999999"
	key, err := storage.ParseOriginalKey("originals/99/" + originalID + "/original.jpg")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("explicit-repair-integration")
	path := filepath.Join(root, filepath.FromSlash(key.String()))
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	checkRepository, err := NewPostgresRepository(checkPool)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewChecker(checkRepository, store)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := checker.Check(ctx, ScopeAll)
	if err != nil || !checked.Sealed || checked.FindingCount != 1 {
		t.Fatalf("initial Check() = %+v, %v", checked, err)
	}

	repairPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_repair_runtime")
	repairRepository, err := NewRepairPostgresRepository(repairPool)
	if err != nil {
		t.Fatal(err)
	}
	repairer, err := NewRepairer(repairRepository, store)
	if err != nil {
		t.Fatal(err)
	}
	request := RepairPlanRequest{ReportID: checked.ReportID, AuthorizationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Actor: "integration"}
	type planning struct {
		plan RepairPlan
		err  error
	}
	plans := make(chan planning, 2)
	for index := 0; index < 2; index++ {
		go func() {
			plan, planErr := repairer.Plan(ctx, request)
			plans <- planning{plan: plan, err: planErr}
		}()
	}
	first, secondPlan := <-plans, <-plans
	if first.err != nil || secondPlan.err != nil {
		t.Fatalf("concurrent Plan() errors = %v / %v", first.err, secondPlan.err)
	}
	plan := first.plan
	if len(plan.Items) != 1 || len(secondPlan.plan.Items) != 1 {
		t.Fatalf("concurrent plan sizes = %d / %d", len(plan.Items), len(secondPlan.plan.Items))
	}
	if secondPlan.plan.Run.RunID != plan.Run.RunID || secondPlan.plan.Items[0].ItemID != plan.Items[0].ItemID || secondPlan.plan.Items[0].QuarantineID != plan.Items[0].QuarantineID {
		t.Fatalf("concurrent plans diverged: %+v / %+v", plan, secondPlan.plan)
	}
	if len(plan.Items) != 1 || plan.Items[0].SourceType != RepairOriginalFinal {
		t.Fatalf("plan = %+v", plan)
	}
	replayed, err := repairer.Plan(ctx, request)
	if err != nil || !replayed.Replayed || replayed.Run.RunID != plan.Run.RunID || replayed.Items[0].ItemID != plan.Items[0].ItemID || replayed.Items[0].QuarantineID != plan.Items[0].QuarantineID {
		t.Fatalf("plan replay = %+v, %v", replayed, err)
	}
	if _, err := repairer.Plan(ctx, RepairPlanRequest{ReportID: checked.ReportID, AuthorizationID: request.AuthorizationID, Actor: "contradictory-actor"}); !errors.Is(err, ErrRepairConflict) {
		t.Fatalf("contradictory plan replay error = %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE reconciliation_repair_plan_seals SET plan_digest=repeat('0',64) WHERE run_id=$1`, plan.Run.RunID); err == nil {
		t.Fatal("sealed repair plan was mutable")
	}

	if _, err = admin.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='repair-test',owner='integration',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var before [5]int64
	if err = admin.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM reconciliation_repair_runs),
		(SELECT count(*) FROM reconciliation_repair_manifest_items),
		(SELECT count(*) FROM reconciliation_repair_attempts),
		(SELECT count(*) FROM reconciliation_repair_events),
		(SELECT count(*) FROM reconciliation_repair_results)`).Scan(&before[0], &before[1], &before[2], &before[3], &before[4]); err != nil {
		t.Fatal(err)
	}
	preview, err := repairer.DryRun(ctx, checked.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Decision != "eligible" {
		t.Fatalf("preview = %+v", preview)
	}
	var after [5]int64
	if err = admin.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM reconciliation_repair_runs),
		(SELECT count(*) FROM reconciliation_repair_manifest_items),
		(SELECT count(*) FROM reconciliation_repair_attempts),
		(SELECT count(*) FROM reconciliation_repair_events),
		(SELECT count(*) FROM reconciliation_repair_results)`).Scan(&after[0], &after[1], &after[2], &after[3], &after[4]); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("dry-run changed repair evidence: before=%v after=%v", before, after)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
		t.Fatalf("dry-run source=%q error=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".quarantine")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created quarantine: %v", err)
	}
	attemptID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	preparedEventID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if _, err := repairRepository.BeginAttempt(ctx, attemptID, plan.Items[0].ItemID, preparedEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := repairPool.Exec(ctx, `SELECT nmcp_append_repair_event($1,$2,'applying','applying',NULL,NULL,NULL)`,
		"ffffffff-ffff-4fff-8fff-ffffffffffff", attemptID); err == nil {
		t.Fatal("applying event committed without a same-transaction convergence boundary")
	}
	var executionIDCalls atomic.Int32
	repairer.newID = func() (string, error) {
		if executionIDCalls.Add(1) == 3 {
			return "", errors.New("injected rename event id failure")
		}
		return newUUIDv4()
	}

	type execution struct {
		result RepairExecutionResult
		err    error
	}
	executions := make(chan execution, 2)
	for index := 0; index < 2; index++ {
		go func() {
			result, executeErr := repairer.Execute(ctx, plan.Run.RunID)
			executions <- execution{result: result, err: executeErr}
		}()
	}
	for index := 0; index < 2; index++ {
		executed := <-executions
		if executed.err != nil {
			t.Fatal(executed.err)
		}
		if executed.result.Result != "succeeded" || executed.result.Manifest != 1 || executed.result.Quarantined != 1 {
			t.Fatalf("repair result = %+v", executed.result)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source remains after repair: %v", err)
	}
	destination := filepath.Join(root, ".quarantine", plan.Items[0].QuarantineID)
	got, err := os.ReadFile(destination)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatalf("destination error=%v bytes=%q", err, got)
	}

	second, err := checker.Check(ctx, ScopeAll)
	if err != nil || !second.Sealed || second.FindingCount != 0 {
		t.Fatalf("post-repair Check() = %+v, %v", second, err)
	}
}

func TestRepairIntegrationAllFourSourceTypes(t *testing.T) {
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
	if err = migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mediaID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	originalID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	orphanID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	profileID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	jobID := "11111111-1111-4111-8111-111111111111"
	targetID := "22222222-2222-4222-8222-222222222222"
	renderID := "33333333-3333-4333-8333-333333333333"
	tempRenderID := "44444444-4444-4444-8444-444444444444"
	uploadAttemptID := "55555555-5555-4555-8555-555555555555"
	transformAttemptID := "66666666-6666-4666-8666-666666666666"
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fixture, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Rollback(context.Background())
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`ALTER TABLE jobs DISABLE TRIGGER USER`, nil},
		{`ALTER TABLE job_targets DISABLE TRIGGER USER`, nil},
		{`ALTER TABLE profiles DISABLE TRIGGER USER`, nil},
		{`INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, []any{mediaID}},
		{`INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES($1::nmcp_uuid_v4,$2::nmcp_uuid_v4,repeat('1',64),'originals/dd/'||$1::nmcp_uuid_v4::text||'/original.jpg','image/jpeg',1)`, []any{originalID, mediaID}},
		{`INSERT INTO profiles(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters) VALUES($1,'repair-all-four',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,'{}')`, []any{profileID}},
		{`INSERT INTO jobs(id,type,original_id,media_id_snapshot,status,attempts,max_attempts,started_at,finished_at) VALUES($1,'transform',$2,$3,'succeeded',1,3,$4,$4)`, []any{jobID, originalID, mediaID, old}},
		{`INSERT INTO job_targets(id,job_id,profile_id,status,attempts) VALUES($1,$2,$3,'succeeded',1)`, []any{targetID, jobID, profileID}},
		{`INSERT INTO storage_attempts(id,kind,original_id,temp_relative_key,coverage) VALUES($1::nmcp_uuid_v4,'upload',$2::nmcp_uuid_v4,'originals/dd/'||$2::nmcp_uuid_v4::text||'/.original.'||$1::nmcp_uuid_v4::text||'.tmp','native')`, []any{uploadAttemptID, originalID}},
		{`INSERT INTO storage_attempts(id,kind,original_id,job_id,temp_relative_key,coverage) VALUES($1,'transform',$2,$3,NULL,'native')`, []any{transformAttemptID, originalID, jobID}},
		{`INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at,occurred_at) VALUES($1,1,'registered',$3,$3),($1,2,'aborted',NULL,$3),($2,1,'claimed',$3,$3),($2,2,'released',NULL,$3)`, []any{uploadAttemptID, transformAttemptID, old}},
		{`ALTER TABLE jobs ENABLE TRIGGER USER`, nil},
		{`ALTER TABLE job_targets ENABLE TRIGGER USER`, nil},
		{`ALTER TABLE profiles ENABLE TRIGGER USER`, nil},
	} {
		if _, err := fixture.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture statement %q: %v", statement.sql, err)
		}
	}
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	type source struct {
		subjectType, subjectID, path        string
		mediaID, jobID, targetID, attemptID *string
		payload                             []byte
	}
	sources := []source{
		{subjectType: "original", subjectID: orphanID, path: "originals/aa/" + orphanID + "/original.jpg", payload: []byte("original-final")},
		{subjectType: "original_attempt_temp", subjectID: originalID, path: "originals/dd/" + originalID + "/.original." + uploadAttemptID + ".tmp", attemptID: &uploadAttemptID, payload: []byte("original-temp")},
		{subjectType: "rendition", subjectID: renderID, path: "renditions/dd/" + originalID + "/" + targetID + "/" + renderID + ".avif", mediaID: &mediaID, jobID: &jobID, targetID: &targetID, payload: []byte("rendition-final")},
		{subjectType: "rendition_attempt_temp", subjectID: tempRenderID, path: "renditions/dd/" + originalID + "/" + targetID + "/." + tempRenderID + ".avif." + transformAttemptID + ".tmp", mediaID: &mediaID, jobID: &jobID, targetID: &targetID, attemptID: &transformAttemptID, payload: []byte("rendition-temp")},
	}
	reportID := "77777777-7777-4777-8777-777777777777"
	if _, err := admin.Exec(ctx, `INSERT INTO reconciliation_check_reports(
		id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
		db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at)
		SELECT $1,1,1,'all',version,name,checksum,statement_timestamp()-interval '5 seconds',
		statement_timestamp()-interval '4 seconds',statement_timestamp()-interval '3 seconds',
		statement_timestamp()-interval '2 seconds',statement_timestamp()-interval '48 hours 4 seconds'
		FROM schema_migrations ORDER BY version DESC LIMIT 1`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO reconciliation_check_source_results(report_id,source,result) SELECT $1,source,'complete' FROM unnest(ARRAY['database_references','attempt_owners','storage_scan']) source`, reportID); err != nil {
		t.Fatal(err)
	}
	for index, source := range sources {
		absolute := filepath.Join(root, filepath.FromSlash(source.path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, source.payload, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime, ctime, digest := repairIntegrationObservation(t, absolute, source.payload)
		kind := "final_orphan"
		if source.attemptID != nil {
			kind = "aged_attempt_temp"
		}
		findingID := []string{"88888888-8888-4888-8888-888888888881", "88888888-8888-4888-8888-888888888882", "88888888-8888-4888-8888-888888888883", "88888888-8888-4888-8888-888888888884"}[index]
		if _, err := admin.Exec(ctx, `INSERT INTO reconciliation_check_findings(
			id,report_id,ordinal,kind,reason_code,actionability,subject_type,subject_id,media_id,job_id,job_target_id,attempt_id,
			relative_key,expected_state,observed_type,observed_size_bytes,observed_sha256,observed_mtime,observed_ctime,observed_at)
			VALUES($1,$2,$3,$4,'integration_fixture','repairable',$5,$6,$7,$8,$9,$10,$11,$12,'regular',$13,$14,$15,$16,clock_timestamp())`,
			findingID, reportID, index+1, kind, source.subjectType, source.subjectID, source.mediaID, source.jobID, source.targetID, source.attemptID,
			source.path, map[bool]string{true: "unreferenced", false: "unreferenced"}[source.attemptID == nil], len(source.payload), digest, mtime, ctime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO reconciliation_check_report_seals(report_id,finding_count,fs_scan_ended_at) VALUES($1,4,clock_timestamp())`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='all-four-test',owner='integration',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	repairPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_repair_runtime")
	repository, _ := NewRepairPostgresRepository(repairPool)
	repairer, _ := NewRepairer(repository, store)
	plan, err := repairer.Plan(ctx, RepairPlanRequest{ReportID: reportID, AuthorizationID: "99999999-9999-4999-8999-999999999999", Actor: "all-four-integration"})
	if err != nil || len(plan.Items) != 4 {
		t.Fatalf("four-source Plan() = %+v, %v", plan, err)
	}
	preview, err := repairer.DryRun(ctx, reportID)
	if err != nil || len(preview.Items) != 4 {
		t.Fatalf("four-source DryRun() = %+v, %v", preview, err)
	}
	for _, item := range preview.Items {
		if item.Decision != "eligible" {
			t.Fatalf("four-source preview item = %+v", item)
		}
	}
	result, err := repairer.Execute(ctx, plan.Run.RunID)
	if err != nil || result.Result != "succeeded" || result.Quarantined != 4 {
		t.Fatalf("four-source Execute() = %+v, %v", result, err)
	}
	var eventID, attemptID, outcome, observedSHA string
	var observedSize int64
	if err := admin.QueryRow(ctx, `SELECT e.id::text,e.attempt_id::text,e.outcome_code,e.observed_size_bytes,e.observed_sha256
		FROM reconciliation_repair_events e JOIN reconciliation_repair_attempts a ON a.id=e.attempt_id
		WHERE a.manifest_item_id=$1 AND e.event_type='completed'`, plan.Items[0].ItemID).Scan(&eventID, &attemptID, &outcome, &observedSize, &observedSHA); err != nil {
		t.Fatal(err)
	}
	var replayedSequence int
	if err := repairPool.QueryRow(ctx, `SELECT nmcp_append_repair_event($1,$2,'completed',$3,NULL,$4,$5)`, eventID, attemptID, outcome, observedSize, observedSHA).Scan(&replayedSequence); err != nil || replayedSequence < 1 {
		t.Fatalf("exact event replay sequence=%d error=%v", replayedSequence, err)
	}
	if _, err := repairPool.Exec(ctx, `SELECT nmcp_append_repair_event($1,$2,'completed','wrong_outcome',NULL,$3,$4)`, eventID, attemptID, observedSize, observedSHA); err == nil {
		t.Fatal("contradictory event replay was accepted")
	}
}

func repairIntegrationObservation(t *testing.T, path string, payload []byte) (time.Time, time.Time, string) {
	t.Helper()
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MTIME|unix.STATX_CTIME, &stat); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return time.Unix(stat.Mtime.Sec, int64(stat.Mtime.Nsec)).UTC().Truncate(time.Microsecond),
		time.Unix(stat.Ctime.Sec, int64(stat.Ctime.Nsec)).UTC().Truncate(time.Microsecond), hex.EncodeToString(digest[:])
}

func TestRepairStorageEventPreservesPossibleRenameWhenSourceWasRecreated(t *testing.T) {
	event, outcome, code := repairStorageEvent(&storage.RepairError{
		Cause: storage.RepairCauseCollision,
		Stage: storage.RepairStagePrepare,
		Progress: storage.RepairProgress{
			SourcePresent:      true,
			DestinationPresent: true,
		},
		Err: storage.ErrCollision,
	}, false)
	if event != "uncertain" || outcome != "unknown" || code != "outcome_uncertain" {
		t.Fatalf("recreated-source collision classified as %q/%q/%q", event, outcome, code)
	}
}

func TestRepairIntegrationUncertainRecoveryBecomesUnknownAndRemainsCheckable(t *testing.T) {
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
	if err = migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	var recreated atomic.Bool
	var sourcePath string
	injected := errors.New("canonical verification response lost")
	store, err := storage.Open(root, storage.Options{Faults: storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
		if event.Boundary != storage.BoundaryRepairCanonicalVerify || event.Phase != storage.After || !recreated.CompareAndSwap(false, true) {
			return nil
		}
		entries, readErr := os.ReadDir(filepath.Join(root, ".quarantine"))
		if readErr != nil {
			return readErr
		}
		if len(entries) != 1 {
			return errors.New("expected one quarantine destination")
		}
		moved, readErr := os.ReadFile(filepath.Join(root, ".quarantine", entries[0].Name()))
		if readErr != nil {
			return readErr
		}
		// Model an external writer recreating the canonical source after this
		// attempt's rename but before recovery can observe the response.
		if writeErr := os.WriteFile(sourcePath, moved, 0o600); writeErr != nil {
			return writeErr
		}
		return injected
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	originalID := "88888888-8888-4888-8888-888888888888"
	key, err := storage.ParseOriginalKey("originals/88/" + originalID + "/original.jpg")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("uncertain-repair-integration")
	path := filepath.Join(root, filepath.FromSlash(key.String()))
	sourcePath = path
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	checkPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_check_runtime")
	checkRepository, _ := NewPostgresRepository(checkPool)
	checker, _ := NewChecker(checkRepository, store)
	checked, err := checker.Check(ctx, ScopeAll)
	if err != nil || checked.FindingCount != 1 {
		t.Fatalf("initial Check() = %+v, %v", checked, err)
	}
	repairPool := reconciliationRolePool(t, databaseURL, schema, "nmcp_repair_runtime")
	repairRepository, _ := NewRepairPostgresRepository(repairPool)
	repairer, _ := NewRepairer(repairRepository, store)
	plan, err := repairer.Plan(ctx, RepairPlanRequest{ReportID: checked.ReportID, AuthorizationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Actor: "uncertain-integration"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='uncertain-repair-test',owner='integration',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	result, err := repairer.Execute(ctx, plan.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "unknown" || result.Unknown != 1 || result.Quarantined != 0 {
		t.Fatalf("uncertain repair result = %+v", result)
	}
	history, err := repairRepository.ReadHistory(ctx, plan.Items[0].ItemID)
	if err != nil || len(history) == 0 || history[len(history)-1].EventType != "unknown" {
		t.Fatalf("uncertain history = %+v, %v", history, err)
	}
	destination := filepath.Join(root, ".quarantine", plan.Items[0].QuarantineID)
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(payload) {
		t.Fatalf("recreated source error=%v bytes=%q", readErr, got)
	}
	if got, readErr := os.ReadFile(destination); readErr != nil || string(got) != string(payload) {
		t.Fatalf("possible quarantine error=%v bytes=%q", readErr, got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tracked, err := checker.Check(ctx, ScopeAll)
	if err != nil || tracked.FindingCount != 0 {
		t.Fatalf("tracked possible quarantine Check() = %+v, %v", tracked, err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	second, err := checker.Check(ctx, ScopeAll)
	if err != nil || second.FindingCount != 1 {
		t.Fatalf("post-unknown Check() = %+v, %v", second, err)
	}
	var kind string
	if err := admin.QueryRow(ctx, `SELECT kind FROM reconciliation_check_findings WHERE report_id=$1`, second.ReportID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "quarantine_missing" {
		t.Fatalf("post-unknown finding kind = %q", kind)
	}
}
