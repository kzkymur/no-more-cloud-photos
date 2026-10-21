package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/httpapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func integrationService(t *testing.T) *Service {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set: PostgreSQL integration NOT executed")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, url)
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
	cfg, err := pgxpool.ParseConfig(url)
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
	t.Setenv("NMCP_DATABASE_URL", cfg.ConnString())
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
	if _, err := s.BatchPage(ctx, batch.Token, 1); err == nil {
		t.Fatal("expected injected commit rejection")
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
	if _, err := s.Pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,max_attempts=1,started_at=clock_timestamp(),
		lease_token=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, jobID, newID()); err != nil {
		t.Fatal(err)
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
	if status != "failed" || attempts != 1 || maximum != 1 || retryAudits != 0 {
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
	if status != "queued" || attempts != 1 || maximum != 4 || retryAudits != 1 {
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
