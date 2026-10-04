package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
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
		mediaID    = "70000000-0000-4000-8000-000000000001"
		originalID = "71000000-0000-4000-8000-000000000001"
		sha256     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		key        = "originals/71/71000000-0000-4000-8000-000000000001/original.jpg"
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

	t.Run("empty jobs and bundled profiles through HTTP", func(t *testing.T) {
		response := serve(handler, http.MethodGet, "/jobs?media_id="+mediaID, "pg-http-jobs")
		assertResponse(t, response, http.StatusOK, "pg-http-jobs")
		assertJSON(t, response, map[string]any{"items": []any{}, "next_cursor": nil})

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
