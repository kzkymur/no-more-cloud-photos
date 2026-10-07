package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

func TestLifecycleHTTPPostgreSQLIntegration(t *testing.T) {
	pool := readHTTPIntegrationPool(t)
	const (
		mediaID    = "76000000-0000-4000-8000-000000000001"
		originalID = "77000000-0000-4000-8000-000000000001"
	)
	insertLifecycleHTTPMedia(t, pool, mediaID, originalID)
	service, err := medialifecycle.NewService(pool, "https://files.example.test/files")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Dependencies{Lifecycle: service})

	response := serve(handler, http.MethodDelete, "/media/"+mediaID, "lifecycle-pg-delete")
	assertResponse(t, response, http.StatusOK, "lifecycle-pg-delete")
	var detail readapi.MediaDetail
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || detail.ID != mediaID || detail.DeletedAt == nil || detail.CurrentRenditions == nil || detail.Jobs == nil {
		t.Fatalf("delete detail = %#v, %v", detail, err)
	}

	response = serve(handler, http.MethodDelete, "/media/"+mediaID+"/purge", "lifecycle-pg-purge")
	assertResponse(t, response, http.StatusAccepted, "lifecycle-pg-purge")
	var job readapi.Job
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil || job.Type != readapi.JobPurge || job.MediaID != mediaID || job.Targets == nil || len(job.Targets) != 0 {
		t.Fatalf("purge job = %#v, %v", job, err)
	}
	response = serve(handler, http.MethodDelete, "/media/"+mediaID+"/purge", "lifecycle-pg-purge-replay")
	assertResponse(t, response, http.StatusAccepted, "lifecycle-pg-purge-replay")
	var replayedJob readapi.Job
	if err := json.Unmarshal(response.Body.Bytes(), &replayedJob); err != nil || replayedJob.ID != job.ID {
		t.Fatalf("replayed purge job = %#v, %v; want ID %s", replayedJob, err, job.ID)
	}

	response = serve(handler, http.MethodPost, "/media/"+mediaID+"/restore", "lifecycle-pg-restore")
	assertResponse(t, response, http.StatusOK, "lifecycle-pg-restore")
	detail = readapi.MediaDetail{}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || detail.DeletedAt != nil || len(detail.Jobs) != 1 || detail.Jobs[0].Status != readapi.JobCancelled {
		t.Fatalf("restore detail = %#v, %v", detail, err)
	}

	response = serve(handler, http.MethodPost, "/media/"+mediaID+"/restore", "lifecycle-pg-conflict")
	assertResponse(t, response, http.StatusConflict, "lifecycle-pg-conflict")
	assertError(t, response, "media_not_deleted", "lifecycle-pg-conflict")

	response = serve(handler, http.MethodDelete, "/media/"+mediaID+"/purge", "lifecycle-pg-active-purge")
	assertResponse(t, response, http.StatusConflict, "lifecycle-pg-active-purge")
	assertError(t, response, "media_not_deleted", "lifecycle-pg-active-purge")

	response = serve(handler, http.MethodDelete, "/media/"+mediaID, "lifecycle-pg-redelete")
	assertResponse(t, response, http.StatusOK, "lifecycle-pg-redelete")
	response = serve(handler, http.MethodDelete, "/media/"+mediaID+"/purge", "lifecycle-pg-second-purge")
	assertResponse(t, response, http.StatusAccepted, "lifecycle-pg-second-purge")
	job = readapi.Job{}
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='running',attempts=1,started_at=clock_timestamp(),lease_token='78000000-0000-4000-8000-000000000001',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	response = serve(handler, http.MethodPost, "/media/"+mediaID+"/restore", "lifecycle-pg-started")
	assertResponse(t, response, http.StatusConflict, "lifecycle-pg-started")
	assertLifecycleJobError(t, response, "purge_already_started", job.ID)
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='failed',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp(),error_code='failed',error_message='failed' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	response = serve(handler, http.MethodDelete, "/media/"+mediaID+"/purge", "lifecycle-pg-failed")
	assertResponse(t, response, http.StatusConflict, "lifecycle-pg-failed")
	assertLifecycleJobError(t, response, "purge_failed_use_retry", job.ID)

	response = serve(handler, http.MethodDelete, "/media/not-a-uuid", "lifecycle-pg-invalid")
	assertResponse(t, response, http.StatusBadRequest, "lifecycle-pg-invalid")
	assertError(t, response, "invalid_id", "lifecycle-pg-invalid")
	response = serve(handler, http.MethodDelete, "/media/76000000-0000-4000-8000-000000000099", "lifecycle-pg-missing")
	assertResponse(t, response, http.StatusNotFound, "lifecycle-pg-missing")
	assertError(t, response, "media_not_found", "lifecycle-pg-missing")
}

func assertLifecycleJobError(t *testing.T, response *httptest.ResponseRecorder, code, jobID string) {
	t.Helper()
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || body.Error.Details["job_id"] != jobID {
		t.Fatalf("lifecycle error = %#v", body.Error)
	}
}

func insertLifecycleHTTPMedia(t *testing.T, pool *pgxpool.Pool, mediaID, originalID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',10,1,1)`, originalID, mediaID,
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"originals/77/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
}
