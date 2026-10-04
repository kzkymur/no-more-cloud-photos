package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

func TestReadHTTPPostgreSQLIntegration(t *testing.T) {
	pool := readHTTPIntegrationPool(t)
	ctx := context.Background()
	const (
		mediaID     = "70000000-0000-4000-8000-000000000001"
		originalID  = "71000000-0000-4000-8000-000000000001"
		jobID       = "72000000-0000-4000-8000-000000000001"
		targetID    = "73000000-0000-4000-8000-000000000001"
		renditionID = "74000000-0000-4000-8000-000000000001"
		sha256      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		key         = "originals/71/71000000-0000-4000-8000-000000000001/original.jpg"
	)
	acceptedAt := time.Date(2026, 10, 4, 12, 0, 0, 123000000, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,created_at)
		VALUES ($1,'image/jpeg','unknown',$2)`, mediaID, acceptedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,original_filename,width,height,created_at)
		VALUES ($1,$2,$3,$4,'image/jpeg',42,'photo.jpg',12,8,$5)`,
		originalID, mediaID, sha256, key, acceptedAt); err != nil {
		t.Fatal(err)
	}

	codec, err := readapi.NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	reads, err := readapi.NewService(pool, codec, "https://files.example.test/files")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Dependencies{Reads: reads})

	t.Run("media list through HTTP", func(t *testing.T) {
		response := serve(handler, http.MethodGet, "/media?profile=standard&limit=1", "pg-http-list")
		assertResponse(t, response, http.StatusOK, "pg-http-list")
		var page readapi.MediaPage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != mediaID || page.Items[0].Rendition != nil || page.NextCursor != nil {
			t.Fatalf("media page = %#v", page)
		}
	})

	t.Run("detail and immutable original URL through HTTP", func(t *testing.T) {
		response := serve(handler, http.MethodGet, "/media/"+mediaID, "pg-http-detail")
		assertResponse(t, response, http.StatusOK, "pg-http-detail")
		var detail readapi.MediaDetail
		if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.ID != mediaID || detail.CurrentRenditions == nil || detail.Jobs == nil {
			t.Fatalf("media detail = %#v", detail)
		}

		response = serve(handler, http.MethodGet, "/media/"+mediaID+"/original", "pg-http-original")
		assertResponse(t, response, http.StatusOK, "pg-http-original")
		var original readapi.Original
		if err := json.Unmarshal(response.Body.Bytes(), &original); err != nil {
			t.Fatal(err)
		}
		wantURL := "https://files.example.test/files/" + key
		if original.ID != originalID || original.FileURL != wantURL || strings.Contains(original.FileURL, "/files/files/") {
			t.Fatalf("original = %#v; want URL %q", original, wantURL)
		}
	})

	t.Run("current historical and job routes preserve 409 404 and schemas", func(t *testing.T) {
		for _, route := range []string{"/media/" + mediaID + "/display", "/media/" + mediaID + "/thumbnail"} {
			response := serve(handler, http.MethodGet, route, "pg-http-not-ready")
			assertResponse(t, response, http.StatusConflict, "pg-http-not-ready")
			assertError(t, response, "rendition_not_ready", "pg-http-not-ready")
		}

		insertReadHTTPRendition(t, pool, mediaID, originalID, jobID, targetID, renditionID, acceptedAt.Add(time.Hour))
		response := serve(handler, http.MethodGet, "/media/"+mediaID+"/display", "pg-http-display")
		assertResponse(t, response, http.StatusOK, "pg-http-display")
		assertReadHTTPKeys(t, response, []string{"created_at", "duration_ms", "file_url", "height", "id", "job_target_id", "media_id", "mime_type", "profile", "sha256", "size_bytes", "width"})
		var rendition readapi.Rendition
		if err := json.Unmarshal(response.Body.Bytes(), &rendition); err != nil || rendition.ID != renditionID {
			t.Fatalf("display rendition = %#v, %v", rendition, err)
		}

		response = serve(handler, http.MethodGet, "/media/"+mediaID+"/thumbnail", "pg-http-thumbnail")
		assertResponse(t, response, http.StatusConflict, "pg-http-thumbnail")
		assertError(t, response, "rendition_not_ready", "pg-http-thumbnail")

		response = serve(handler, http.MethodGet, "/renditions/"+renditionID, "pg-http-rendition")
		assertResponse(t, response, http.StatusOK, "pg-http-rendition")
		assertReadHTTPKeys(t, response, []string{"created_at", "duration_ms", "file_url", "height", "id", "job_target_id", "media_id", "mime_type", "profile", "sha256", "size_bytes", "width"})

		response = serve(handler, http.MethodGet, "/jobs/"+jobID, "pg-http-job")
		assertResponse(t, response, http.StatusOK, "pg-http-job")
		assertReadHTTPKeys(t, response, []string{"attempts", "available_at", "cancel_reason", "cancelled_at", "created_at", "error", "finished_at", "id", "max_attempts", "media_id", "original_id", "started_at", "status", "targets", "type", "updated_at"})
		var job readapi.Job
		if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil || job.ID != jobID || len(job.Targets) != 1 || job.Targets[0].RenditionID == nil || *job.Targets[0].RenditionID != renditionID {
			t.Fatalf("job response = %#v, %v", job, err)
		}

		missingID := "70000000-0000-4000-8000-000000000099"
		for _, test := range []struct {
			path, code string
		}{
			{"/media/" + missingID + "/display", "media_not_found"},
			{"/renditions/" + missingID, "rendition_not_found"},
			{"/jobs/" + missingID, "job_not_found"},
		} {
			response = serve(handler, http.MethodGet, test.path, "pg-http-route-missing")
			assertResponse(t, response, http.StatusNotFound, "pg-http-route-missing")
			assertError(t, response, test.code, "pg-http-route-missing")
		}
	})

	t.Run("job list and bundled profiles through HTTP", func(t *testing.T) {
		response := serve(handler, http.MethodGet, "/jobs?media_id="+mediaID, "pg-http-jobs")
		assertResponse(t, response, http.StatusOK, "pg-http-jobs")
		var jobs readapi.JobPage
		if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil || len(jobs.Items) != 1 || jobs.Items[0].ID != jobID || jobs.NextCursor != nil {
			t.Fatalf("job page = %#v, %v", jobs, err)
		}

		response = serve(handler, http.MethodGet, "/profiles?status=draft", "pg-http-profiles")
		assertResponse(t, response, http.StatusOK, "pg-http-profiles")
		var profiles readapi.ProfilePage
		if err := json.Unmarshal(response.Body.Bytes(), &profiles); err != nil {
			t.Fatal(err)
		}
		if len(profiles.Items) != 2 || profiles.Items[0].Key != "standard" || profiles.Items[1].Key != "thumbnail" {
			t.Fatalf("profiles = %#v", profiles.Items)
		}
	})

	t.Run("database not-found semantics reach HTTP", func(t *testing.T) {
		response := serve(handler, http.MethodGet, "/media/70000000-0000-4000-8000-000000000099", "pg-http-missing")
		assertResponse(t, response, http.StatusNotFound, "pg-http-missing")
		assertError(t, response, "media_not_found", "pg-http-missing")
	})
}

func readHTTPIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	schema := "readhttp_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop read HTTP integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,false)`, schema+",pg_catalog,pg_temp")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
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
	return pool
}

func insertReadHTTPRendition(t *testing.T, pool *pgxpool.Pool, mediaID, originalID, jobID, targetID, renditionID string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	path := "renditions/71/" + originalID + "/" + targetID + "/" + renditionID + ".avif"
	leaseID := "75000000-0000-4000-8000-000000000001"
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at) VALUES ($1,'transform',$2,$3,'queued',3,$4,$4,$4)`, []any{jobID, originalID, mediaID, now}},
		{`INSERT INTO job_targets (id,job_id,profile_id,status,updated_at) VALUES ($1,$2,'60000000-0000-4000-8000-000000000001','pending',$3)`, []any{targetID, jobID, now}},
		{`UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=$3,started_at=$4,updated_at=$4 WHERE id=$1`, []any{jobID, leaseID, now.Add(time.Minute), now}},
		{`UPDATE job_targets SET status='succeeded',attempts=1,updated_at=$2 WHERE id=$1`, []any{targetID, now}},
		{`INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,created_at) VALUES ($1,$2,$3,'standard',true,$4,'image/avif',21,$5,$6)`, []any{renditionID, mediaID, targetID, path, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", now}},
		{`UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=$2,updated_at=$2 WHERE id=$1`, []any{jobID, now}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertReadHTTPKeys(t *testing.T, response *httptest.ResponseRecorder, want []string) {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(value))
	for key := range value {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("JSON keys = %v, want %v; body=%s", got, want, response.Body.String())
	}
}
