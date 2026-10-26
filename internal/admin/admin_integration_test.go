package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/httpapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func integrationService(t *testing.T) *Service {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set: PostgreSQL integration NOT executed")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "admin_" + strings.ReplaceAll(newID(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
		base.Close()
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 6
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",pg_catalog,pg_temp"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsedURL.Query()
	query.Set("search_path", schema+",pg_catalog,pg_temp")
	parsedURL.RawQuery = query.Encode()
	t.Setenv("NMCP_DATABASE_URL", parsedURL.String())
	t.Setenv("NMCP_FILE_BASE_URL", "https://files.example.test/files/")
	return New(pool)
}

func certifiedProfile(t *testing.T, s *Service, key string, version int) profile.Definition {
	t.Helper()
	definition := profile.ThumbnailV1()
	definition.ID, definition.Key, definition.Version = newID(), key, version
	definition.InputMIMETypes = []string{"image/jpeg"}
	parameters := profile.ThumbnailV1Parameters()
	for mime := range parameters.Recipes {
		if mime != "image/jpeg" {
			delete(parameters.Recipes, mime)
		}
	}
	definition.Parameters, _ = json.Marshal(parameters)
	if err := s.ProfileCreate(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	return definition
}

func certifyJPEG(t *testing.T, s *Service) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO profile_processor_certifications
		(id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence)
		VALUES($1,'nmcp-media',1,'image/jpeg','still','still-avif',1920,1,100,'admin integration fixture only')`, newID())
	if err != nil {
		t.Fatal(err)
	}
}

func insertMedia(t *testing.T, s *Service, number int) string {
	t.Helper()
	mediaID := fmt.Sprintf("10000000-0000-4000-8000-%012x", number)
	originalID := newID()
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO media(id,media_type,taken_at_source) VALUES($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO originals(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES($1,$2,$3,$4,'image/jpeg',10,1,1)`, originalID, mediaID, fmt.Sprintf("%064x", number),
		"originals/"+originalID[:2]+"/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	return mediaID
}

func TestAdminIntegrationConfigProfileCLIAndAudit(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	for _, value := range []string{"0", "36500", "null"} {
		var out, diagnostics bytes.Buffer
		if code := Run(ctx, []string{"config", "set", "--name", "deleted_media_retention_days", "--value", value, "--json"}, &out, &diagnostics); code != 0 {
			t.Fatalf("code=%d output=%q diagnostics=%q", code, out.String(), diagnostics.String())
		}
	}
	if _, err := s.ConfigSet(ctx, "default_timezone", "America/New_York"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigSet(ctx, "default_timezone", "Not/A_Zone"); !errors.Is(err, ErrValidation) {
		t.Fatal(err)
	}
	var auditCount int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE command='config_set'`).Scan(&auditCount); err != nil || auditCount != 4 {
		t.Fatalf("audits=%d err=%v", auditCount, err)
	}
	certifyJPEG(t, s)
	first := certifiedProfile(t, s, "test", 2)
	stale := certifiedProfile(t, s, "test", 1)
	insertMedia(t, s, 1)
	if err := s.ProfileTransition(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ProfileTransition(ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProfileTransition(ctx, stale.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale activation = %v", err)
	}
	var jobs int
	var status string
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs),status FROM profiles WHERE id=$1`, stale.ID).Scan(&jobs, &status); err != nil || jobs != 0 || status != "draft" {
		t.Fatalf("implicit backfill jobs=%d status=%s err=%v", jobs, status, err)
	}
}

func TestAdminIntegrationBatchFrozenMembershipPinResumeAndAtomicity(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	certifyJPEG(t, s)
	first := certifiedProfile(t, s, "test", 1)
	if err := s.ProfileTransition(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{20, 40, 60} {
		insertMedia(t, s, number)
	}
	request := BatchRequest{Operation: "regenerate", ProfileKey: "test", All: true}
	preview, err := s.BeginBatch(ctx, request, "", true)
	if err != nil || preview.Total != 3 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_batches`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dryrun created batch: %d %v", count, err)
	}
	batch, err := s.BeginBatch(ctx, request, "", false)
	if err != nil {
		t.Fatal(err)
	}
	batch, err = s.BatchPage(ctx, batch.Token, 1)
	if err != nil || batch.Succeeded != 1 {
		t.Fatalf("first page=%+v err=%v", batch, err)
	}
	// A new random/smaller Media-ID is outside this batch even though it is
	// below the UUID high-water. Activation cannot change the pinned ID.
	insertMedia(t, s, 10)
	next := certifiedProfile(t, s, "test", 2)
	if err := s.ProfileTransition(ctx, next.ID, true); err != nil {
		t.Fatal(err)
	}
	batch, err = New(s.Pool).BeginBatch(ctx, request, batch.Token, false)
	if err != nil || batch.ProfileID == nil || *batch.ProfileID != first.ID {
		t.Fatalf("resume lost pin: %+v %v", batch, err)
	}
	// Inject failure AFTER effects and receipt updates at commit. Everything
	// in the page must roll back; resuming applies it exactly once.
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_admin_page() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER reject_admin_page AFTER UPDATE ON admin_batches DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION reject_admin_page()`); err != nil {
		t.Fatal(err)
	}
	rejected, rejectErr := s.BatchPage(ctx, batch.Token, 1)
	if rejectErr == nil || rejected.Succeeded != 1 || rejected.Status != "running" || !bytes.Equal(rejected.Checkpoint, batch.Checkpoint) {
		t.Fatalf("speculative rejected result=%+v err=%v", rejected, rejectErr)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("effect/checkpoint crash gap jobs=%d err=%v", count, err)
	}
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_admin_page ON admin_batches`); err != nil {
		t.Fatal(err)
	}
	for batch.Status == "running" {
		batch, err = s.BatchPage(ctx, batch.Token, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if batch.Succeeded != 3 || batch.Failed != 0 {
		t.Fatal(batch)
	}
	if _, err := s.BatchPage(ctx, batch.Token, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM job_targets WHERE profile_id=$1`, first.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("targets=%d err=%v", count, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE media_id_snapshot=$1`, "10000000-0000-4000-8000-00000000000a").Scan(&count); err != nil || count != 0 {
		t.Fatalf("new low UUID joined frozen membership: %d %v", count, err)
	}
	changed := request
	changed.ProfileKey = "other"
	if _, err := s.BeginBatch(ctx, changed, batch.Token, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("resume identity changed: %v", err)
	}
}

func TestAdminIntegrationTryLocksAndAuditConflict(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if err := tryLock(ctx, tx, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent config=%v", err)
	}
	var failed int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE command='config_set' AND outcome='failed'`).Scan(&failed); err != nil || failed != 1 {
		t.Fatalf("failed audit=%d err=%v", failed, err)
	}
}

func TestAdminIntegrationRepairDryRunNormalModeIsReadOnlyConflict(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := s.Repair(ctx, store, newID(), true); !errors.Is(err, ErrConflict) {
		t.Fatalf("normal mode repair preview=%v", err)
	}
	state, err := s.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "normal" {
		t.Fatalf("dry-run entered maintenance: %+v %v", state, err)
	}
	var writes int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_audit)+(SELECT count(*) FROM reconciliation_repair_runs)`).Scan(&writes); err != nil || writes != 0 {
		t.Fatalf("dry-run writes=%d err=%v", writes, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dryrun storage changed: %v %v", entries, err)
	}
}

func failedMixedJob(t *testing.T, s *Service) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	certifyJPEG(t, s)
	first := certifiedProfile(t, s, "mixed", 1)
	second := certifiedProfile(t, s, "other", 1)
	for _, id := range []string{first.ID, second.ID} {
		if err := s.ProfileTransition(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	mediaID := insertMedia(t, s, 100)
	batch, err := s.BeginBatch(ctx, BatchRequest{Operation: "regenerate", ProfileKey: "mixed", MediaID: mediaID}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BatchPage(ctx, batch.Token, 1); err != nil {
		t.Fatal(err)
	}
	var jobID, targetID, originalID string
	if err := s.Pool.QueryRow(ctx, `SELECT j.id::text,t.id::text,j.original_id::text FROM jobs j JOIN job_targets t ON t.job_id=j.id`).Scan(&jobID, &targetID, &originalID); err != nil {
		t.Fatal(err)
	}
	otherTarget := newID()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO job_targets(id,job_id,profile_id,status) VALUES($1,$2,$3,'pending')`, otherTarget, jobID, second.ID); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := s.Pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=attempts+1,started_at=COALESCE(started_at,clock_timestamp()),
			lease_token=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, jobID, newID()); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			if _, err := s.Pool.Exec(ctx, `UPDATE jobs SET status='queued',lease_token=NULL,lease_expires_at=NULL WHERE id=$1`, jobID); err != nil {
				t.Fatal(err)
			}
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',attempts=1 WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	renditionID := newID()
	if _, err := tx.Exec(ctx, `INSERT INTO renditions(id,media_id,job_target_id,profile_key,is_current,purge_after,
		relative_path,mime_type,width,height,size_bytes,sha256,processor_audit)
		VALUES($1,$2,$3,'mixed',false,NULL,$4,'image/avif',1,1,1,$5,'{"fixture":"admin retry"}')`,
		renditionID, mediaID, targetID, "renditions/"+originalID[:2]+"/"+originalID+"/"+targetID+"/"+renditionID+".avif", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='failed',attempts=1,error_code='process_failed',error_message='fixture' WHERE id=$1`, otherTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='failed',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp(),
		error_code='process_failed',error_message='fixture' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID, targetID, otherTarget
}

func TestAdminIntegrationRetryReceiptsAtomicCrashAndSuccessfulTargetImmutable(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	jobID, successID, failedID := failedMixedJob(t, s)
	var before []byte
	if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(t) FROM job_targets t WHERE id=$1`, successID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	request := BatchRequest{Operation: "jobs_retry", JobID: jobID, AdditionalAttempts: 3}
	batch, err := s.BeginBatch(ctx, request, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_retry_page() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'injected retry commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER reject_retry_page AFTER UPDATE ON admin_batches DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION reject_retry_page()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BatchPage(ctx, batch.Token, 1); err == nil {
		t.Fatal("commit rejection did not fail")
	}
	var status string
	var attempts, maximum, retryAudits int
	if err := s.Pool.QueryRow(ctx, `SELECT status,attempts,max_attempts,(SELECT count(*) FROM admin_audit WHERE command='jobs_retry')
		FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts, &maximum, &retryAudits); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 3 || maximum != 3 || retryAudits != 0 {
		t.Fatalf("retry crash gap status=%s attempts=%d max=%d audits=%d", status, attempts, maximum, retryAudits)
	}
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_retry_page ON admin_batches`); err != nil {
		t.Fatal(err)
	}
	// New service simulates a restarted CLI after losing all process state.
	batch, err = New(s.Pool).BatchPage(ctx, batch.Token, 1)
	if err != nil || batch.Succeeded != 1 {
		t.Fatalf("resume=%+v err=%v", batch, err)
	}
	if _, err := s.BatchPage(ctx, batch.Token, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT status,attempts,max_attempts,(SELECT count(*) FROM admin_audit WHERE command='jobs_retry')
		FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts, &maximum, &retryAudits); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 3 || maximum != 6 || retryAudits != 1 {
		t.Fatalf("replayed retry status=%s attempts=%d max=%d audits=%d", status, attempts, maximum, retryAudits)
	}
	var after []byte
	var failedStatus string
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT to_jsonb(t) FROM job_targets t WHERE id=$1),
		(SELECT status FROM job_targets WHERE id=$2)`, successID, failedID).Scan(&after, &failedStatus); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || failedStatus != "pending" {
		t.Fatalf("success target mutated=%v failedStatus=%s", !bytes.Equal(before, after), failedStatus)
	}
}

func TestAdminIntegrationCLIBatchVisibleThroughHTTPAndNoDuplicates(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	certifyJPEG(t, s)
	definition := certifiedProfile(t, s, "web", 1)
	if err := s.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatal(err)
	}
	mediaID := insertMedia(t, s, 200)
	var out, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"regenerate", "--profile", "web", "--media-id", mediaID, "--batch-size", "1", "--json"}, &out, &diagnostics); code != 0 {
		t.Fatalf("CLI code=%d stdout=%s stderr=%s", code, out.String(), diagnostics.String())
	}
	var batch BatchResult
	if err := json.Unmarshal(out.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	codec, err := readapi.NewCursorCodec(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	reads, err := readapi.NewService(s.Pool, codec, "https://files.example.test/files/")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(httpapi.Dependencies{Reads: reads})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/jobs?media_id="+mediaID, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "queued") || !strings.Contains(response.Body.String(), definition.ID) {
		t.Fatalf("HTTP code=%d body=%s", response.Code, response.Body.String())
	}
	// A separate explicit invocation cannot duplicate the already-pinned
	// target work. A replay of the token is an idempotent success.
	out.Reset()
	diagnostics.Reset()
	if code := Run(ctx, []string{"regenerate", "--profile", "web", "--media-id", mediaID, "--json"}, &out, &diagnostics); code != 4 {
		t.Fatalf("duplicate CLI code=%d body=%s", code, out.String())
	}
	if _, err := s.BeginBatch(ctx, BatchRequest{Operation: "regenerate", ProfileKey: "web", MediaID: mediaID}, batch.Token, false); err != nil {
		t.Fatal(err)
	}
}

func TestAdminIntegrationMaintenanceResumeRequiresFreshCleanFullCheck(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); !errors.Is(err, ErrConflict) {
		t.Fatalf("maintenance config mutation=%v", err)
	}
	if _, err := s.MaintenanceResume(ctx, store); err != nil {
		t.Fatal(err)
	}
	state, err := s.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "normal" || len(state.SuccessfulCheck) == 0 {
		t.Fatalf("resume state=%+v err=%v", state, err)
	}
	// Missing durable original must prevent resuming and preserve mode.
	insertMedia(t, s, 300)
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test missing',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MaintenanceResume(ctx, store); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing-file resume=%v", err)
	}
	state, err = s.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "maintenance" {
		t.Fatalf("failed resume state=%+v err=%v", state, err)
	}
}

func TestAdminIntegrationFiniteMediaCleanupDryRunAndExecution(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mediaID := insertMedia(t, s, 400)
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := medialifecycle.NewService(s.Pool, "https://files.example.test/files/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	preview, err := s.Cleanup(ctx, store, "media", true)
	if err != nil || preview.Eligible != 1 || preview.Completed != 0 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := s.Cleanup(ctx, store, "media", false)
	if err != nil || result.Completed != 1 || result.Failed != 0 || result.MorePossible {
		t.Fatalf("cleanup=%+v err=%v", result, err)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM media WHERE id=$1)`, mediaID).Scan(&exists); err != nil || exists {
		t.Fatalf("cleanup did not converge exists=%v err=%v", exists, err)
	}
	if _, err := s.Cleanup(ctx, store, "all", false); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("backup unsupported falsely succeeded: %v", err)
	}
}

func dueHistoricalRendition(t *testing.T, s *Service) string {
	t.Helper()
	ctx := context.Background()
	_, targetID, _ := failedMixedJob(t, s)
	var renditionID, mediaID, originalID, profileID string
	if err := s.Pool.QueryRow(ctx, `SELECT r.id::text,r.media_id::text,j.original_id::text,t.profile_id::text
		FROM renditions r JOIN job_targets t ON t.id=r.job_target_id JOIN jobs j ON j.id=t.job_id WHERE t.id=$1`, targetID).Scan(&renditionID, &mediaID, &originalID, &profileID); err != nil {
		t.Fatal(err)
	}
	jobID, newTarget, newRendition := newID(), newID(), newID()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	statements := []struct {
		sql  string
		args []any
	}{
		{`UPDATE renditions SET purge_after=clock_timestamp()-interval '1 hour' WHERE id=$1`, []any{renditionID}},
		{`INSERT INTO jobs(id,type,original_id,media_id_snapshot,status,max_attempts) VALUES($1,'transform',$2,$3,'queued',3)`, []any{jobID, originalID, mediaID}},
		{`INSERT INTO job_targets(id,job_id,profile_id,status) VALUES($1,$2,$3,'pending')`, []any{newTarget, jobID, profileID}},
		{`UPDATE jobs SET status='running',attempts=1,started_at=clock_timestamp(),lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, []any{jobID, newID()}},
		{`UPDATE job_targets SET status='succeeded',attempts=1 WHERE id=$1`, []any{newTarget}},
		{`INSERT INTO renditions(id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,width,height,size_bytes,sha256,processor_audit)
		VALUES($1,$2,$3,'mixed',true,$4,'image/avif',1,1,1,$5,'{"fixture":"current"}')`, []any{newRendition, mediaID, newTarget,
			"renditions/" + originalID[:2] + "/" + originalID + "/" + newTarget + "/" + newRendition + ".avif", strings.Repeat("b", 64)}},
		{`UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, []any{jobID}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return renditionID
}

func TestAdminIntegrationCleanupAuditFailureRecoversExactNativeReceipts(t *testing.T) {
	for _, kind := range []string{"media", "renditions"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationService(t)
			ctx := context.Background()
			store, err := storage.Open(t.TempDir(), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var objectID string
			if kind == "renditions" {
				objectID = dueHistoricalRendition(t, s)
			} else {
				objectID = insertMedia(t, s, 500)
				if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); err != nil {
					t.Fatal(err)
				}
				lifecycle, err := medialifecycle.NewService(s.Pool, "https://files.example.test/files/")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lifecycle.Delete(ctx, objectID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_cleanup_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.command='cleanup_result' THEN RAISE EXCEPTION 'injected result audit rejection'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER reject_cleanup_audit BEFORE INSERT ON admin_audit FOR EACH ROW EXECUTE FUNCTION reject_cleanup_audit()`); err != nil {
				t.Fatal(err)
			}
			first, err := s.Cleanup(ctx, store, kind, false)
			if !errors.Is(err, ErrPartial) || first.Failed != 1 || first.Completed != 0 || first.RunID == "" {
				t.Fatalf("failed-audit cleanup=%+v err=%v", first, err)
			}
			var authorized, pending, nativeDone bool
			if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_audit WHERE command='cleanup_begin' AND outcome='partial'
				AND $1::uuid=ANY(affected_ids) AND sanitized_arguments->>'run_id'=$2),
				EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$2::uuid AND object_id=$1 AND outcome='pending')`, objectID, first.RunID).Scan(&authorized, &pending); err != nil {
				t.Fatal(err)
			}
			nativeDone, err = s.cleanupProof(ctx, kind, cleanupItem{id: objectID, mediaID: objectID})
			if kind == "renditions" {
				var mediaID string
				if err := s.Pool.QueryRow(ctx, `SELECT media_id::text FROM admin_cleanup_items WHERE run_id=$1`, first.RunID).Scan(&mediaID); err != nil {
					t.Fatal(err)
				}
				nativeDone, err = s.cleanupProof(ctx, kind, cleanupItem{id: objectID, mediaID: mediaID})
			}
			if err != nil || !authorized || !pending || !nativeDone {
				t.Fatalf("lost recoverable authorization authorized=%v pending=%v nativeDone=%v err=%v", authorized, pending, nativeDone, err)
			}
			if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_cleanup_audit ON admin_audit`); err != nil {
				t.Fatal(err)
			}
			resumed, err := New(s.Pool).Cleanup(ctx, store, kind, false)
			if err != nil || resumed.RunID != first.RunID || resumed.Completed != 1 || len(resumed.IDs) != 1 || resumed.IDs[0] != objectID {
				t.Fatalf("resumed=%+v err=%v", resumed, err)
			}
			var receipts int
			if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE command='cleanup_result' AND $1::uuid=ANY(affected_ids)
				AND sanitized_arguments->>'run_id'=$2 AND sanitized_arguments->>'outcome'='completed'`, objectID, first.RunID).Scan(&receipts); err != nil || receipts != 1 {
				t.Fatalf("completion receipts=%d err=%v", receipts, err)
			}
		})
	}
}

func TestAdminIntegrationDirectRepairFencesResumeAndDryRunHasNoEffects(t *testing.T) {
	s := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	store, err := storage.Open(root, storage.Options{Faults: storage.FaultInjectorFunc(func(ctx context.Context, event storage.FaultEvent) error {
		if event.Boundary == storage.BoundaryRepairRename && event.Phase == storage.Before {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := newID()
	path := filepath.Join(root, "originals", id[:2], id, "original.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if !errors.Is(err, ErrConflict) || !checked.Sealed || checked.FindingCount != 1 {
		t.Fatalf("check=%+v err=%v", checked, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='direct repair test',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	// The checker records the latest schema, not the older repair migration.
	// Both the reader admission and persisted plan seal must support it.
	var reportVersion, latestVersion int64
	if err := s.Pool.QueryRow(ctx, `SELECT migration_version,(SELECT max(version) FROM schema_migrations)
		FROM reconciliation_check_reports WHERE id=$1`, checked.ReportID).Scan(&reportVersion, &latestVersion); err != nil || reportVersion != latestVersion || reportVersion < 21 {
		t.Fatalf("report version=%d latest=%d err=%v", reportVersion, latestVersion, err)
	}
	preview, err := s.Repair(ctx, store, checked.ReportID, true)
	if err != nil || preview == nil {
		t.Fatalf("maintenance preview=%+v err=%v", preview, err)
	}
	var mutations int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_audit)+(SELECT count(*) FROM reconciliation_repair_runs)`).Scan(&mutations); err != nil || mutations != 0 {
		t.Fatalf("preview mutations=%d err=%v", mutations, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("preview moved source: %v", err)
	}
	repository, err := reconciliation.NewRepairPostgresRepository(s.Pool)
	if err != nil {
		t.Fatal(err)
	}
	repairer, err := reconciliation.NewRepairer(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := repairer.Plan(ctx, reconciliation.RepairPlanRequest{ReportID: checked.ReportID, AuthorizationID: newID(), Actor: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	var sealedVersion int64
	if err := s.Pool.QueryRow(ctx, `SELECT report_migration_version FROM reconciliation_repair_plan_seals WHERE run_id=$1`, plan.Run.RunID).Scan(&sealedVersion); err != nil || sealedVersion != reportVersion {
		t.Fatalf("sealed version=%d report=%d err=%v", sealedVersion, reportVersion, err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := repairer.Execute(ctx, plan.Run.RunID)
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Direct Repairer does NOT take CLI sessionLock. Its real maintenance
	// FOR SHARE fence must still prevent resume across check/cutover.
	_, resumeErr := s.MaintenanceResume(ctx, store)
	close(release)
	if !errors.Is(resumeErr, ErrConflict) {
		t.Fatalf("resume crossed direct repair transaction: %v", resumeErr)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

// Compatibility is explicit, not a removal of sealed evidence admission.
func TestAdminIntegrationRepairReportCompatibilityRemainsClosed(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checked, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if err != nil || !checked.Sealed || checked.FindingCount != 0 {
		t.Fatalf("clean check=%+v err=%v", checked, err)
	}
	checkAdmission := func(t *testing.T, version int64, badName, badChecksum, sealed, sourceError, admitted bool) {
		t.Helper()
		id := newID()
		_, err := s.Pool.Exec(ctx, `INSERT INTO reconciliation_check_reports
			(id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
			db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at)
			SELECT $1,format_version,classifier_version,scope,$3,
			CASE WHEN $4 THEN 'unknown_migration' ELSE (SELECT name FROM schema_migrations WHERE version=$3) END,
			CASE WHEN $5 THEN repeat('0',64) ELSE (SELECT checksum FROM schema_migrations WHERE version=$3) END,
			db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at
			FROM reconciliation_check_reports WHERE id=$2`, id, checked.ReportID, version, badName, badChecksum)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO reconciliation_check_source_results(report_id,source,result,error_code)
			SELECT $1,source,CASE WHEN $3 AND source='storage_scan' THEN 'error' ELSE result END,
			CASE WHEN $3 AND source='storage_scan' THEN 'test_source_error' ELSE NULL END
			FROM reconciliation_check_source_results WHERE report_id=$2`, id, checked.ReportID, sourceError); err != nil {
			t.Fatal(err)
		}
		if sealed {
			if _, err := s.Pool.Exec(ctx, `INSERT INTO reconciliation_check_report_seals(report_id,finding_count,fs_scan_ended_at,completed_at)
				SELECT $1,finding_count,fs_scan_ended_at,completed_at FROM reconciliation_check_report_seals WHERE report_id=$2`, id, checked.ReportID); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := s.Pool.Query(ctx, `SELECT * FROM nmcp_read_repairable_findings($1,0,1)`, id)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if (err == nil) != admitted {
			t.Fatalf("admitted=%v want=%v err=%v", err == nil, admitted, err)
		}
	}
	t.Run("admin-receipts-20", func(t *testing.T) { checkAdmission(t, 20, false, false, true, false, true) })
	t.Run("compatibility-21", func(t *testing.T) { checkAdmission(t, 21, false, false, true, false, true) })
	t.Run("unknown-version", func(t *testing.T) { checkAdmission(t, 999, true, true, true, false, false) })
	t.Run("wrong-name", func(t *testing.T) { checkAdmission(t, 21, true, false, true, false, false) })
	t.Run("wrong-checksum", func(t *testing.T) { checkAdmission(t, 21, false, true, true, false, false) })
	t.Run("unsealed", func(t *testing.T) { checkAdmission(t, 21, false, false, false, false, false) })
	t.Run("source-error", func(t *testing.T) { checkAdmission(t, 21, false, false, true, true, false) })
	t.Run("future-version-only", func(t *testing.T) {
		// Everything except the explicit supported-version whitelist matches:
		// valid applied history/name/hash, sealed report, three clean sources.
		if _, err := s.Pool.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum,transactional,idempotent,dirty)
			VALUES(22,'future_compatibility_fixture',repeat('a',64),true,false,false)`); err != nil {
			t.Fatal(err)
		}
		checkAdmission(t, 22, false, false, true, false, false)
	})
}

func TestAdminIntegrationBeginCommitRejectionHasNoResumeToken(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	certifyJPEG(t, s)
	definition := certifiedProfile(t, s, "test", 1)
	if err := s.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatal(err)
	}
	insertMedia(t, s, 900)
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_admin_begin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'injected begin commit rejection'; END $$;
		CREATE CONSTRAINT TRIGGER reject_admin_begin AFTER INSERT ON admin_batches DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION reject_admin_begin()`); err != nil {
		t.Fatal(err)
	}
	result, err := s.BeginBatch(ctx, BatchRequest{Operation: "regenerate", ProfileKey: "test", All: true}, "", false)
	if err == nil || result.Token != "" || result.Status != "not_committed" || result.Total != 0 {
		t.Fatalf("non-durable begin=%+v err=%v", result, err)
	}
	var out, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"regenerate", "--profile", "test", "--all", "--yes", "--json"}, &out, &diagnostics); code != 1 || strings.Contains(diagnostics.String(), "resume token:") || strings.Contains(out.String(), `"status":"running"`) {
		t.Fatalf("CLI code=%d output=%s diagnostics=%s", code, &out, &diagnostics)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_batches`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected batches=%d err=%v", count, err)
	}
}

func TestAdminIntegrationCleanupExhaustedRunConvergesAndFinishIsAtomic(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Model an older process dying after its final receipt but before finish.
	oldRun := newID()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO admin_cleanup_runs(id,kind) VALUES($1,'renditions')`, oldRun); err != nil {
		t.Fatal(err)
	}
	objectID := dueHistoricalRendition(t, s)
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_cleanup_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.finished_at IS NOT NULL AND OLD.finished_at IS NULL AND OLD.id<>'`+oldRun+`' THEN
		RAISE EXCEPTION 'injected final receipt failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_cleanup_finish BEFORE UPDATE ON admin_cleanup_runs FOR EACH ROW EXECUTE FUNCTION reject_cleanup_finish()`); err != nil {
		t.Fatal(err)
	}
	first, err := s.Cleanup(ctx, store, "renditions", false)
	if !errors.Is(err, ErrPartial) || first.RunID == oldRun || first.Completed != 0 || first.Failed != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var pending bool
	if err := s.Pool.QueryRow(ctx, `SELECT outcome='pending' FROM admin_cleanup_items WHERE run_id=$1 AND object_id=$2`, first.RunID, objectID).Scan(&pending); err != nil || !pending {
		t.Fatalf("last receipt escaped finish transaction pending=%v err=%v", pending, err)
	}
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_cleanup_finish ON admin_cleanup_runs`); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Cleanup(ctx, store, "renditions", false)
	if err != nil || resumed.Completed != 1 || resumed.RunID != first.RunID {
		t.Fatalf("recovery=%+v err=%v", resumed, err)
	}
	var finished bool
	if err := s.Pool.QueryRow(ctx, `SELECT finished_at IS NOT NULL FROM admin_cleanup_runs WHERE id=$1`, first.RunID).Scan(&finished); err != nil || !finished {
		t.Fatalf("final receipt did not finish run: %v %v", finished, err)
	}
}

func TestAdminIntegrationCleanupRestoreRedeleteNeedsNewAuthorization(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := medialifecycle.NewService(s.Pool, "https://files.example.test/files/")
	if err != nil {
		t.Fatal(err)
	}
	mediaID := insertMedia(t, s, 901)
	if _, err := lifecycle.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	items, oldRun, err := s.cleanupSelection(ctx, "media", false)
	if err != nil || len(items) != 1 {
		t.Fatalf("selection=%v err=%v", items, err)
	}
	firstJob, err := lifecycle.EnqueueDuePurge(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE admin_cleanup_items SET job_id=$2 WHERE run_id=$1`, oldRun, firstJob.Job.ID); err != nil {
		t.Fatal(err)
	}
	// Crash before Start, then the real restore API revokes the bound job.
	if _, err := lifecycle.Restore(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.Cleanup(ctx, store, "media", false)
	if err != nil || revoked.RunID != oldRun || revoked.Skipped != 1 || revoked.Completed != 0 {
		t.Fatalf("revoked=%+v err=%v", revoked, err)
	}
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_redelete_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.command='cleanup_result' THEN RAISE EXCEPTION 'late result rejection'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_redelete_audit BEFORE INSERT ON admin_audit FOR EACH ROW EXECUTE FUNCTION reject_redelete_audit()`); err != nil {
		t.Fatal(err)
	}
	second, err := s.Cleanup(ctx, store, "media", false)
	if !errors.Is(err, ErrPartial) || second.RunID == oldRun || second.Failed != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	var boundJob string
	if err := s.Pool.QueryRow(ctx, `SELECT job_id::text FROM admin_cleanup_items WHERE run_id=$1`, second.RunID).Scan(&boundJob); err != nil || boundJob == firstJob.Job.ID {
		t.Fatalf("new authorization bound=%s old=%s err=%v", boundJob, firstJob.Job.ID, err)
	}
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_redelete_audit ON admin_audit`); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Cleanup(ctx, store, "media", false)
	if err != nil || recovered.RunID != second.RunID || recovered.Completed != 1 || recovered.Skipped != 0 {
		t.Fatalf("recovered actual deletion=%+v err=%v", recovered, err)
	}
}

func TestAdminIntegrationConcurrentActivatorsKeepGreatestSuccessfulVersion(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	certifyJPEG(t, s)
	second := certifiedProfile(t, s, "test", 2)
	third := certifiedProfile(t, s, "test", 3)
	insertMedia(t, s, 902)
	type activation struct {
		version int
		err     error
	}
	start := make(chan struct{})
	results := make(chan activation, 2)
	go func() {
		<-start
		results <- activation{2, New(s.Pool).ProfileTransition(ctx, second.ID, true)}
	}()
	go func() {
		<-start
		results <- activation{3, New(s.Pool).ProfileTransition(ctx, third.ID, true)}
	}()
	close(start)
	greatest := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			if result.version > greatest {
				greatest = result.version
			}
		} else if !errors.Is(result.err, ErrConflict) {
			t.Fatal(result.err)
		}
	}
	var active, jobs int
	if err := s.Pool.QueryRow(ctx, `SELECT version,(SELECT count(*) FROM jobs) FROM profiles WHERE key='test' AND status='active'`).Scan(&active, &jobs); err != nil || greatest == 0 || active != greatest || jobs != 0 {
		t.Fatalf("active=%d greatest successful=%d implicit jobs=%d err=%v", active, greatest, jobs, err)
	}
	if active == 3 {
		if err := s.ProfileTransition(ctx, second.ID, true); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale activator accepted: %v", err)
		}
	}
}

func TestAdminIntegrationBatchRealCommittedReplyLossAndResume(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	jobID, _, _ := failedMixedJob(t, s)
	request := BatchRequest{Operation: "jobs_retry", JobID: jobID, AdditionalAttempts: 3}
	s.afterBatchCommit = func(context.Context) error { return errors.New("lost committed reply") }
	begin, err := s.BeginBatch(ctx, request, "", false)
	if err == nil || begin.Token == "" || begin.Status != "unknown" || begin.Total != 0 {
		t.Fatalf("begin lost reply=%+v err=%v", begin, err)
	}
	resumed, err := New(s.Pool).BeginBatch(ctx, request, begin.Token, false)
	if err != nil || resumed.Total != 1 || resumed.Status != "running" {
		t.Fatalf("durable begin=%+v err=%v", resumed, err)
	}
	page, err := s.BatchPage(ctx, begin.Token, 1)
	if err == nil || page.Status != "unknown" || page.Succeeded != 0 || !bytes.Equal(page.Checkpoint, resumed.Checkpoint) {
		t.Fatalf("page lost reply=%+v err=%v", page, err)
	}
	completed, err := New(s.Pool).BatchPage(ctx, begin.Token, 1)
	if err != nil || completed.Succeeded != 1 || completed.Status != "succeeded" {
		t.Fatalf("durable page=%+v err=%v", completed, err)
	}
	var maximum, attempts, audits int
	if err := s.Pool.QueryRow(ctx, `SELECT max_attempts,attempts,(SELECT count(*) FROM admin_audit WHERE command='jobs_retry') FROM jobs WHERE id=$1`, jobID).Scan(&maximum, &attempts, &audits); err != nil || maximum != 6 || attempts != 3 || audits != 1 {
		t.Fatalf("duplicate allowance max=%d attempts=%d audits=%d err=%v", maximum, attempts, audits, err)
	}
}

func TestAdminIntegrationRenditionStaleSnapshotDoesNotPoisonFutureWork(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	objectID := dueHistoricalRendition(t, s)
	items, runID, err := s.cleanupSelection(ctx, "renditions", false)
	if err != nil || len(items) != 1 {
		t.Fatalf("selected=%v err=%v", items, err)
	}
	// Native-ineligible state after authorization: no current replacement.
	if _, err := s.Pool.Exec(ctx, `UPDATE renditions SET is_current=false WHERE is_current`); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Cleanup(ctx, store, "renditions", false)
	if err != nil || stale.RunID != runID || stale.Skipped != 1 || stale.Completed != 0 {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
	var remains bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM renditions WHERE id=$1)`, objectID).Scan(&remains); err != nil || !remains {
		t.Fatalf("ineligible deleted=%v err=%v", !remains, err)
	}
	// Restore the valid current replacement; a new run may now select it.
	if _, err := s.Pool.Exec(ctx, `UPDATE renditions SET is_current=true WHERE id<>$1`, objectID); err != nil {
		t.Fatal(err)
	}
	healthy, err := s.Cleanup(ctx, store, "renditions", false)
	if err != nil || healthy.RunID == runID || healthy.Completed != 1 {
		t.Fatalf("healthy=%+v err=%v", healthy, err)
	}
}
