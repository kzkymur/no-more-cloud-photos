package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

type fakeDatabase struct {
	err   error
	calls int
}

func (f *fakeDatabase) Ping(context.Context) error {
	f.calls++
	return f.err
}

type fakeMigrations struct {
	status database.Status
	err    error
	calls  int
}

func (f *fakeMigrations) Status(context.Context) (database.Status, error) {
	f.calls++
	return f.status, f.err
}

func readyHandler(t *testing.T) (http.Handler, *fakeDatabase, *fakeMigrations, string) {
	t.Helper()
	db := &fakeDatabase{}
	migrations := &fakeMigrations{}
	root := t.TempDir()
	return NewHandler(Dependencies{Database: db, Migrations: migrations, StorageRoot: root}), db, migrations, root
}

func TestHealth(t *testing.T) {
	db := &fakeDatabase{err: errors.New("must not be called")}
	migrations := &fakeMigrations{err: errors.New("must not be called")}
	h := NewHandler(Dependencies{Database: db, Migrations: migrations, StorageRoot: "/missing"})

	response := serve(h, http.MethodGet, "/healthz", "health-request")

	assertResponse(t, response, http.StatusOK, "health-request")
	assertJSON(t, response, map[string]any{"status": "ok"})
	if db.calls != 0 || migrations.calls != 0 {
		t.Fatalf("health probe queried dependencies: db=%d migrations=%d", db.calls, migrations.calls)
	}
}

func TestReady(t *testing.T) {
	h, db, migrations, root := readyHandler(t)

	response := serve(h, http.MethodGet, "/readyz", "ready-request")

	assertResponse(t, response, http.StatusOK, "ready-request")
	assertJSON(t, response, map[string]any{"status": "ready"})
	if db.calls != 1 || migrations.calls != 1 {
		t.Fatalf("readiness calls: db=%d migrations=%d, want one each", db.calls, migrations.calls)
	}
	assertDirectoryEmpty(t, root)
}

func TestReadyDependencyFailures(t *testing.T) {
	tests := []struct {
		name         string
		dependencies func(t *testing.T) Dependencies
	}{
		{
			name: "missing database",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Migrations: &fakeMigrations{}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "missing migration checker",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "missing storage configuration",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}}
			},
		},
		{
			name: "database ping",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{err: errors.New("password=secret host=private")}, Migrations: &fakeMigrations{}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "migration status error",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{err: errors.New("migration SQL secret")}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "pending migrations",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{status: database.Status{Pending: true}}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "migration drift",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{status: database.Status{Drift: true, DriftReason: "sensitive SQL"}}, StorageRoot: t.TempDir()}
			},
		},
		{
			name: "missing storage root",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, StorageRoot: filepath.Join(t.TempDir(), "missing")}
			},
		},
		{
			name: "storage root is file",
			dependencies: func(t *testing.T) Dependencies {
				root := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, StorageRoot: root}
			},
		},
		{
			name: "storage root is symlink",
			dependencies: func(t *testing.T) Dependencies {
				directory := t.TempDir()
				root := filepath.Join(t.TempDir(), "root")
				if err := os.Symlink(directory, root); err != nil {
					t.Fatal(err)
				}
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, StorageRoot: root}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := NewHandler(test.dependencies(t))
			response := serve(h, http.MethodGet, "/readyz", "failure-request")

			assertResponse(t, response, http.StatusServiceUnavailable, "failure-request")
			assertJSON(t, response, map[string]any{
				"error": map[string]any{
					"code":       "unavailable",
					"message":    "service is unavailable",
					"request_id": "failure-request",
					"details":    map[string]any{},
				},
			})
			body := response.Body.String()
			for _, sensitive := range []string{"secret", "private", "migration SQL", "sensitive SQL"} {
				if strings.Contains(body, sensitive) {
					t.Fatalf("response exposed dependency detail %q: %s", sensitive, body)
				}
			}
		})
	}
}

func TestMethodsAndUnknownRoutes(t *testing.T) {
	h, _, _, _ := readyHandler(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodDelete} {
			t.Run(method+" "+path, func(t *testing.T) {
				response := serve(h, method, path, "method-request")
				assertResponse(t, response, http.StatusMethodNotAllowed, "method-request")
				if got := response.Header().Get("Allow"); got != http.MethodGet {
					t.Fatalf("Allow = %q, want %q", got, http.MethodGet)
				}
				assertError(t, response, "method_not_allowed", "method-request")
			})
		}
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method+" unknown", func(t *testing.T) {
			response := serve(h, method, "/unknown", "unknown-request")
			assertResponse(t, response, http.StatusNotFound, "unknown-request")
			assertError(t, response, "not_found", "unknown-request")
			if allow := response.Header().Get("Allow"); allow != "" {
				t.Fatalf("unknown route Allow = %q, want empty", allow)
			}
		})
	}
}

func TestAcceptNegotiation(t *testing.T) {
	h, _, _, _ := readyHandler(t)
	tests := []struct {
		name   string
		accept string
		status int
	}{
		{name: "absent", status: http.StatusOK},
		{name: "JSON", accept: "application/json", status: http.StatusOK},
		{name: "JSON parameters", accept: "application/json; charset=utf-8", status: http.StatusOK},
		{name: "application wildcard", accept: "application/*", status: http.StatusOK},
		{name: "all wildcard", accept: "*/*", status: http.StatusOK},
		{name: "explicit rejection beats wildcard", accept: "application/json;q=0, */*;q=1", status: http.StatusNotAcceptable},
		{name: "text only", accept: "text/plain", status: http.StatusNotAcceptable},
		{name: "invalid quality", accept: "application/json;q=2", status: http.StatusNotAcceptable},
		{name: "malformed", accept: "not a media type", status: http.StatusNotAcceptable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			request.Header.Set("X-Request-ID", "accept-request")
			if test.accept != "" {
				request.Header.Set("Accept", test.accept)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			assertResponse(t, response, test.status, "accept-request")
			if test.status == http.StatusNotAcceptable {
				assertError(t, response, "not_acceptable", "accept-request")
			}
		})
	}
}

func TestRequestIDs(t *testing.T) {
	h, _, _, _ := readyHandler(t)

	for _, value := range []string{"!", "valid-ID_123~", strings.Repeat("a", 64)} {
		t.Run("accept "+value, func(t *testing.T) {
			response := serve(h, http.MethodGet, "/missing", value)
			assertResponse(t, response, http.StatusNotFound, value)
			assertError(t, response, "not_found", value)
		})
	}

	invalid := []string{"", "contains space", "contains\tcontrol", "contains\x7fdelete", strings.Repeat("a", 65), "non-ascii-é"}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			response := serve(h, http.MethodGet, "/missing", value)
			generated := response.Header().Get("X-Request-ID")
			if !validRequestID(generated) {
				t.Fatalf("generated invalid request ID %q", generated)
			}
			if value != "" && generated == value {
				t.Fatalf("invalid request ID was accepted: %q", value)
			}
			assertError(t, response, "not_found", generated)
		})
	}
}

func TestStorageProbeLeavesExistingFilesAndNoArtifact(t *testing.T) {
	h, _, _, root := readyHandler(t)
	existing := filepath.Join(root, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	response := serve(h, http.MethodGet, "/readyz", "cleanup-request")

	assertResponse(t, response, http.StatusOK, "cleanup-request")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing" {
		t.Fatalf("storage entries after probe = %v, want only existing", entries)
	}
}

func serve(h http.Handler, method, path, requestID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func assertResponse(t *testing.T, response *httptest.ResponseRecorder, status int, requestID string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != jsonContentType {
		t.Fatalf("Content-Type = %q, want %q", got, jsonContentType)
	}
	if got := response.Header().Get("X-Request-ID"); got != requestID {
		t.Fatalf("X-Request-ID = %q, want %q", got, requestID)
	}
}

func assertJSON(t *testing.T, response *httptest.ResponseRecorder, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response JSON: %v; body=%q", err, response.Body.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, code, requestID string) {
	t.Helper()
	var body struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error JSON: %v", err)
	}
	if body.Error.Code != code || body.Error.RequestID != requestID || body.Error.Message == "" || body.Error.Details == nil || len(body.Error.Details) != 0 {
		t.Fatalf("invalid error response: %#v", body.Error)
	}
}

func assertDirectoryEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("storage probe left artifacts: %v", entries)
	}
}
