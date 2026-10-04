package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	corestorage "github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/upload"
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

type fakeStorage struct {
	err   error
	calls int
}

type fakeUploadAcceptor struct {
	requests []upload.Request
	accept   func(context.Context, upload.Request) (upload.Outcome, error)
}

func (f *fakeUploadAcceptor) Accept(ctx context.Context, request upload.Request) (upload.Outcome, error) {
	f.requests = append(f.requests, request)
	if f.accept != nil {
		return f.accept(ctx, request)
	}
	_, err := io.Copy(io.Discard, request.Body)
	return upload.Outcome{Status: http.StatusCreated, Body: json.RawMessage(`{"media":{},"job":null}`)}, err
}

func (f *fakeStorage) Probe(context.Context) error {
	f.calls++
	return f.err
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
	store, err := corestorage.Open(root, corestorage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewHandler(Dependencies{Database: db, Migrations: migrations, Storage: store}), db, migrations, root
}

func TestHealth(t *testing.T) {
	db := &fakeDatabase{err: errors.New("must not be called")}
	migrations := &fakeMigrations{err: errors.New("must not be called")}
	h := NewHandler(Dependencies{Database: db, Migrations: migrations, Storage: &fakeStorage{err: errors.New("must not be called")}})

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
				return Dependencies{Migrations: &fakeMigrations{}, Storage: &fakeStorage{}}
			},
		},
		{
			name: "missing migration checker",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Storage: &fakeStorage{}}
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
				return Dependencies{Database: &fakeDatabase{err: errors.New("password=secret host=private")}, Migrations: &fakeMigrations{}, Storage: &fakeStorage{}}
			},
		},
		{
			name: "migration status error",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{err: errors.New("migration SQL secret")}, Storage: &fakeStorage{}}
			},
		},
		{
			name: "pending migrations",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{status: database.Status{Pending: true}}, Storage: &fakeStorage{}}
			},
		},
		{
			name: "migration drift",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{status: database.Status{Drift: true, DriftReason: "sensitive SQL"}}, Storage: &fakeStorage{}}
			},
		},
		{
			name: "missing storage root",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, Storage: &fakeStorage{err: os.ErrNotExist}}
			},
		},
		{
			name: "storage root is file",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, Storage: &fakeStorage{err: os.ErrInvalid}}
			},
		},
		{
			name: "storage root is symlink",
			dependencies: func(t *testing.T) Dependencies {
				return Dependencies{Database: &fakeDatabase{}, Migrations: &fakeMigrations{}, Storage: &fakeStorage{err: corestorage.ErrSymlink}}
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

func TestUploadStreamsNormalizedRequestAndRawOutcome(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	var received []byte
	acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		var err error
		received, err = io.ReadAll(request.Body)
		if err != nil {
			return upload.Outcome{}, err
		}
		return upload.Outcome{
			Status: http.StatusConflict, Replayed: true,
			Body: json.RawMessage(`{"future":{"number":9007199254740993},"nullable":null}`),
		}, nil
	}
	h := NewHandler(Dependencies{Upload: acceptor})
	body := rawMultipart("boundary", `form-data; name="file"; filename="C:\\fakepath\\photo.jpg"`, "image/not-trusted", nil, "content", "")
	request := uploadHTTPReq(body, "multipart/form-data; boundary=boundary", "key-1", "current-request")
	response := httptest.NewRecorder()

	h.ServeHTTP(response, request)

	assertResponse(t, response, http.StatusConflict, "current-request")
	if got, want := response.Body.String(), `{"future":{"number":9007199254740993},"nullable":null}`; got != want {
		t.Fatalf("raw response = %q, want %q", got, want)
	}
	if response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("replay response omitted Idempotency-Replayed")
	}
	if len(acceptor.requests) != 1 || string(received) != "content" {
		t.Fatalf("accept requests/body = %d/%q", len(acceptor.requests), received)
	}
	requestSeen := acceptor.requests[0]
	if requestSeen.IdempotencyKey != "key-1" || requestSeen.RequestID != "current-request" || requestSeen.Filename == nil || *requestSeen.Filename != "photo.jpg" {
		t.Fatalf("service request = %#v", requestSeen)
	}
}

func TestUploadFilenameParsingAndNormalization(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		want        *string
		wantCode    string
	}{
		{name: "absent", disposition: `form-data; name="file"`},
		{name: "empty", disposition: `form-data; name="file"; filename=""`},
		{name: "quoted semicolon and backslash path", disposition: `form-data; name="file"; filename="C:\\fakepath\\semi;name.jpg"`, want: stringPointerTest("semi;name.jpg")},
		{name: "browser single backslash fake path", disposition: `form-data; name="file"; filename="C:\fakepath\photo.jpg"`, want: stringPointerTest("photo.jpg")},
		{name: "literal percent fallback", disposition: `form-data; name="file"; filename="100%.jpg"`, want: stringPointerTest("100%.jpg")},
		{name: "extended takes precedence", disposition: `form-data; name="file"; filename="fallback.jpg"; filename*=UTF-8''preferred.jpg`, want: stringPointerTest("preferred.jpg")},
		{name: "extended path and NFC", disposition: `form-data; name="file"; filename*=UTF-8''dir%2Fsub%2Fe%CC%81.jpg`, want: stringPointerTest("é.jpg")},
		{name: "invalid charset never falls back", disposition: `form-data; name="file"; filename="safe.jpg"; filename*=ISO-8859-1''bad.jpg`, wantCode: "invalid_filename"},
		{name: "bad percent never falls back", disposition: `form-data; name="file"; filename="safe.jpg"; filename*=UTF-8''bad%ZZ.jpg`, wantCode: "invalid_filename"},
		{name: "invalid UTF-8", disposition: `form-data; name="file"; filename*=UTF-8''bad%C3%28.jpg`, wantCode: "invalid_filename"},
		{name: "encoded control", disposition: `form-data; name="file"; filename*=UTF-8''bad%00.jpg`, wantCode: "invalid_filename"},
		{name: "quoted control", disposition: "form-data; name=\"file\"; filename=\"bad\tname.jpg\"", wantCode: "invalid_filename"},
		{name: "quoted extended rejected", disposition: `form-data; name="file"; filename*="UTF-8''name.jpg"`, wantCode: "invalid_filename"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acceptor := &fakeUploadAcceptor{}
			h := NewHandler(Dependencies{Upload: acceptor})
			request := uploadHTTPReq(rawMultipart("b", test.disposition, "", nil, "x", ""), "multipart/form-data; boundary=b", "key", "filename-request")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if test.wantCode != "" {
				assertResponse(t, response, http.StatusBadRequest, "filename-request")
				assertError(t, response, test.wantCode, "filename-request")
				if len(acceptor.requests) != 0 {
					t.Fatalf("service called %d times", len(acceptor.requests))
				}
				return
			}
			assertResponse(t, response, http.StatusCreated, "filename-request")
			if len(acceptor.requests) != 1 || !reflect.DeepEqual(acceptor.requests[0].Filename, test.want) {
				t.Fatalf("filename = %#v, want %#v", acceptor.requests, test.want)
			}
		})
	}
}

func TestUploadRejectsMalformedRequestsBeforeService(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		key         *string
		body        string
		code        string
	}{
		{name: "missing key", contentType: "multipart/form-data; boundary=b", body: rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), code: "missing_idempotency_key"},
		{name: "empty key", contentType: "multipart/form-data; boundary=b", key: stringPointerTest(""), body: rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), code: "invalid_idempotency_key"},
		{name: "whitespace key", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("bad key"), body: rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), code: "invalid_idempotency_key"},
		{name: "missing content type", key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "wrong content type", contentType: "application/json", key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "missing boundary", contentType: "multipart/form-data", key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "extended boundary", contentType: "multipart/form-data; boundary*=UTF-8''b", key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "empty boundary", contentType: `multipart/form-data; boundary=""`, key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "invalid boundary", contentType: `multipart/form-data; boundary="line\nbreak"`, key: stringPointerTest("key"), code: "invalid_multipart"},
		{name: "missing part", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("key"), body: "--b--\r\n", code: "invalid_multipart"},
		{name: "unexpected part", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("key"), body: rawMultipart("b", `form-data; name="caption"`, "", nil, "x", ""), code: "invalid_multipart"},
		{name: "transfer encoding", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("key"), body: rawMultipart("b", `form-data; name="file"`, "", []string{"Content-Transfer-Encoding: binary"}, "x", ""), code: "invalid_multipart"},
		{name: "empty transfer encoding", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("key"), body: rawMultipart("b", `form-data; name="file"`, "", []string{"Content-Transfer-Encoding:"}, "x", ""), code: "invalid_multipart"},
		{name: "nested multipart", contentType: "multipart/form-data; boundary=b", key: stringPointerTest("key"), body: rawMultipart("b", `form-data; name="file"`, "multipart/mixed; boundary=inner", nil, "x", ""), code: "invalid_multipart"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acceptor := &fakeUploadAcceptor{}
			h := NewHandler(Dependencies{Upload: acceptor})
			request := httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(test.body))
			request.Header.Set("X-Request-ID", "malformed-request")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.key != nil {
				request.Header.Set("Idempotency-Key", *test.key)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			assertResponse(t, response, http.StatusBadRequest, "malformed-request")
			assertError(t, response, test.code, "malformed-request")
			if len(acceptor.requests) != 0 {
				t.Fatalf("service called %d times", len(acceptor.requests))
			}
		})
	}
}

func TestUploadTrailingPartFailsDuringServiceRead(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		content, err := io.ReadAll(request.Body)
		if string(content) != "first" {
			t.Fatalf("service read %q", content)
		}
		if err == nil {
			t.Fatal("service reached EOF without trailing-part error")
		}
		return upload.Outcome{}, err
	}
	body := "--b\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nfirst\r\n" +
		"--b\r\nContent-Disposition: form-data; name=\"other\"\r\n\r\nsecond\r\n--b--\r\n"
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, uploadHTTPReq(body, "multipart/form-data; boundary=b", "key", "trailing-request"))
	assertResponse(t, response, http.StatusBadRequest, "trailing-request")
	assertError(t, response, "invalid_multipart", "trailing-request")
	if len(acceptor.requests) != 1 {
		t.Fatalf("service calls = %d, want 1", len(acceptor.requests))
	}
}

func TestUploadTrailingPartOversizeTakesPrecedence(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		content, err := io.ReadAll(request.Body)
		if string(content) != "first" {
			t.Fatalf("service read %q", content)
		}
		if err == nil {
			t.Fatal("service reached EOF without trailing-part error")
		}
		return upload.Outcome{}, err
	}
	prefix := "--b\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nfirst\r\n" +
		"--b\r\nContent-Disposition: form-data; name=\"other\"\r\n\r\n"
	body := prefix + strings.Repeat("x", 1024) + "\r\n--b--\r\n"
	h := NewHandler(Dependencies{Upload: acceptor}).(*handler)
	h.maxBodyBytes = int64(len(prefix) + 16)
	request := uploadHTTPReq(body, "multipart/form-data; boundary=b", "key", "trailing-large-request")
	response := httptest.NewRecorder()

	h.ServeHTTP(response, request)

	assertResponse(t, response, http.StatusRequestEntityTooLarge, "trailing-large-request")
	assertError(t, response, "upload_too_large", "trailing-large-request")
	if response.Header().Get("Connection") != "close" || !request.Close {
		t.Fatalf("413 connection state = header %q, request.Close %v", response.Header().Get("Connection"), request.Close)
	}
}

func TestUploadTrailingPartTimeoutTakesPrecedence(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		content, err := io.ReadAll(request.Body)
		if string(content) != "first" {
			t.Fatalf("service read %q", content)
		}
		if err == nil {
			t.Fatal("service reached EOF without trailing-part error")
		}
		return upload.Outcome{}, err
	}
	prefix := "--b\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nfirst\r\n" +
		"--b\r\nContent-Disposition: form-data; name=\"other\"\r\n\r\npartial"
	request := httptest.NewRequest(http.MethodPost, "/media", io.MultiReader(strings.NewReader(prefix), timeoutReader{}))
	request.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	request.Header.Set("Idempotency-Key", "key")
	request.Header.Set("X-Request-ID", "trailing-timeout-request")
	response := httptest.NewRecorder()

	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)

	assertResponse(t, response, http.StatusRequestTimeout, "trailing-timeout-request")
	assertError(t, response, "upload_timeout", "trailing-timeout-request")
}

func TestUploadTotalBodyLimitIncludesEpilogue(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	h := NewHandler(Dependencies{Upload: acceptor}).(*handler)
	base := rawMultipart("b", `form-data; name="file"`, "", nil, "x", "")
	h.maxBodyBytes = int64(len(base) + 2)
	request := uploadHTTPReq(base+"oversized epilogue", "multipart/form-data; boundary=b", "key", "large-request")
	response := httptest.NewRecorder()

	h.ServeHTTP(response, request)

	assertResponse(t, response, http.StatusRequestEntityTooLarge, "large-request")
	assertError(t, response, "upload_too_large", "large-request")
	if response.Header().Get("Connection") != "close" {
		t.Fatalf("413 Connection = %q, want close", response.Header().Get("Connection"))
	}
	if !request.Close {
		t.Fatal("413 did not mark request connection for closure")
	}
}

func TestUploadServiceTooLargeClosesConnectionBeforeHeaders(t *testing.T) {
	const maxFileBytes = int64(4)
	var bytesRead int64
	acceptor := &fakeUploadAcceptor{accept: func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		count, err := io.CopyN(io.Discard, request.Body, maxFileBytes+1)
		bytesRead = count
		if err != nil {
			return upload.Outcome{}, err
		}
		return upload.Outcome{}, &upload.Failure{
			Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: "uploaded file exceeds the size limit",
		}
	}}
	body := rawMultipart("b", `form-data; name="file"`, "", nil, "content remains unread", "")
	request := uploadHTTPReq(body, "multipart/form-data; boundary=b", "key", "service-large-request")
	response := &closeCheckingRecorder{ResponseRecorder: httptest.NewRecorder(), request: request}

	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)

	assertResponse(t, response.ResponseRecorder, http.StatusRequestEntityTooLarge, "service-large-request")
	assertError(t, response.ResponseRecorder, "upload_too_large", "service-large-request")
	if bytesRead != maxFileBytes+1 {
		t.Fatalf("service read %d bytes, want %d", bytesRead, maxFileBytes+1)
	}
	if !response.closedBeforeHeaders {
		t.Fatal("413 response headers were written before request connection was marked for closure")
	}
	if response.Header().Get("Connection") != "close" || !request.Close {
		t.Fatalf("413 connection state = header %q, request.Close %v", response.Header().Get("Connection"), request.Close)
	}
	for _, deadline := range response.readDeadlines {
		if deadline.IsZero() {
			t.Fatalf("incomplete request body cleared read deadline: %v", response.readDeadlines)
		}
	}
}

func TestUploadFailureUsesServiceBodyWithoutCause(t *testing.T) {
	acceptor := &fakeUploadAcceptor{accept: func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		_, _ = io.Copy(io.Discard, request.Body)
		return upload.Outcome{}, &upload.Failure{
			Status: http.StatusConflict, Code: "idempotency_conflict", Message: "request conflicts",
			Details: map[string]any{"original_request_hash": "hash"}, Cause: errors.New("password=secret"),
		}
	}}
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, uploadHTTPReq(rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), "multipart/form-data; boundary=b", "key", "failure-request"))
	assertResponse(t, response, http.StatusConflict, "failure-request")
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("response exposed cause: %s", response.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "idempotency_conflict" || body.Error.RequestID != "failure-request" || body.Error.Details["original_request_hash"] != "hash" {
		t.Fatalf("service error body = %#v", body)
	}
}

func TestUploadMethodsAndNegotiation(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	h := NewHandler(Dependencies{Upload: acceptor})
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodHead} {
		response := serve(h, method, "/media", "method-request")
		assertResponse(t, response, http.StatusMethodNotAllowed, "method-request")
		if response.Header().Get("Allow") != http.MethodGet+", "+http.MethodPost {
			t.Fatalf("Allow = %q", response.Header().Get("Allow"))
		}
	}
	request := uploadHTTPReq("", "multipart/form-data; boundary=b", "key", "accept-request")
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	assertResponse(t, response, http.StatusNotAcceptable, "accept-request")
	assertError(t, response, "not_acceptable", "accept-request")
	if len(acceptor.requests) != 0 {
		t.Fatalf("service called %d times", len(acceptor.requests))
	}
	unknown := serve(h, http.MethodPost, "/media/", "exact-path-request")
	assertResponse(t, unknown, http.StatusNotFound, "exact-path-request")
}

func TestUploadRejectsDuplicateIdempotencyKey(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	request := uploadHTTPReq(rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), "multipart/form-data; boundary=b", "first", "duplicate-key-request")
	request.Header.Add("Idempotency-Key", "second")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusBadRequest, "duplicate-key-request")
	assertError(t, response, "invalid_idempotency_key", "duplicate-key-request")
	if len(acceptor.requests) != 0 {
		t.Fatalf("service called %d times", len(acceptor.requests))
	}
}

func TestUploadEarlyRejectionsCloseTricklingHTTP11Connection(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		accept      string
		key         *string
		contentType string
		nilUpload   bool
		wantStatus  int
	}{
		{name: "wrong method", method: http.MethodPut, key: stringPointerTest("key"), contentType: "multipart/form-data; boundary=b", wantStatus: http.StatusMethodNotAllowed},
		{name: "unacceptable response", method: http.MethodPost, accept: "text/plain", key: stringPointerTest("key"), contentType: "multipart/form-data; boundary=b", wantStatus: http.StatusNotAcceptable},
		{name: "missing key", method: http.MethodPost, contentType: "multipart/form-data; boundary=b", wantStatus: http.StatusBadRequest},
		{name: "invalid key", method: http.MethodPost, key: stringPointerTest("bad key"), contentType: "multipart/form-data; boundary=b", wantStatus: http.StatusBadRequest},
		{name: "invalid content type", method: http.MethodPost, key: stringPointerTest("key"), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing boundary", method: http.MethodPost, key: stringPointerTest("key"), contentType: "multipart/form-data", wantStatus: http.StatusBadRequest},
		{name: "nil upload", method: http.MethodPost, key: stringPointerTest("key"), contentType: "multipart/form-data; boundary=b", nilUpload: true, wantStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := Dependencies{Upload: &fakeUploadAcceptor{}}
			if test.nilUpload {
				dependencies.Upload = nil
			}
			closed := make(chan struct{}, 1)
			server := httptest.NewUnstartedServer(NewHandler(dependencies))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			server.Start()
			t.Cleanup(server.Close)

			connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}

			var request strings.Builder
			request.WriteString(test.method + " /media HTTP/1.1\r\n")
			request.WriteString("Host: example.test\r\n")
			request.WriteString("Transfer-Encoding: chunked\r\n")
			request.WriteString("X-Request-ID: trickle-request\r\n")
			if test.accept != "" {
				request.WriteString("Accept: " + test.accept + "\r\n")
			}
			if test.key != nil {
				request.WriteString("Idempotency-Key: " + *test.key + "\r\n")
			}
			if test.contentType != "" {
				request.WriteString("Content-Type: " + test.contentType + "\r\n")
			}
			request.WriteString("\r\n1\r\nx\r\n") // Deliberately omit the terminal chunk.

			started := time.Now()
			if _, err := io.WriteString(connection, request.String()); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			response, err := http.ReadResponse(reader, &http.Request{Method: test.method})
			if err != nil {
				t.Fatalf("read early response: %v", err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				t.Fatalf("read response body: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("early response took %v", elapsed)
			}
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if !response.Close {
				t.Fatal("HTTP/1.1 response did not signal connection close")
			}
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("connection remained open after early response: %v", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("server connection did not reach closed state")
			}
		})
	}
}

func TestUploadHardTimeoutDoesNotChargePostBodyWork(t *testing.T) {
	now := time.Unix(1_000, 0)
	acceptor := &fakeUploadAcceptor{accept: func(ctx context.Context, request upload.Request) (upload.Outcome, error) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return upload.Outcome{}, err
		}
		now = now.Add(time.Hour + time.Second)
		if err := ctx.Err(); err != nil {
			t.Fatalf("accept context expired after body completion: %v", err)
		}
		return upload.Outcome{Status: http.StatusCreated, Body: json.RawMessage(`{"media":{},"job":null}`)}, nil
	}}
	h := NewHandler(Dependencies{Upload: acceptor}).(*handler)
	h.hardTimeout = time.Hour
	h.now = func() time.Time { return now }
	request := uploadHTTPReq(rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), "multipart/form-data; boundary=b", "key", "timeout-request")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	assertResponse(t, response, http.StatusCreated, "timeout-request")
}

func TestUploadPreservesClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	acceptor := &fakeUploadAcceptor{accept: func(ctx context.Context, request upload.Request) (upload.Outcome, error) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return upload.Outcome{}, err
		}
		cancel()
		<-ctx.Done()
		return upload.Outcome{}, ctx.Err()
	}}
	request := uploadHTTPReq(rawMultipart("b", `form-data; name="file"`, "", nil, "x", ""), "multipart/form-data; boundary=b", "key", "canceled-request")
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()

	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)

	assertResponse(t, response, http.StatusRequestTimeout, "canceled-request")
	assertError(t, response, "upload_timeout", "canceled-request")
}

func TestUploadElapsedBodyDeadlineReturnsRequestTimeout(t *testing.T) {
	now := time.Unix(1_000, 0)
	body := rawMultipart("b", `form-data; name="file"`, "", nil, "content", "")
	request := uploadHTTPReq(body, "multipart/form-data; boundary=b", "key", "elapsed-timeout-request")
	request.Body = &advancingReadCloser{
		Reader: strings.NewReader(body),
		afterRead: func() {
			now = now.Add(time.Hour + time.Second)
		},
	}
	h := NewHandler(Dependencies{Upload: &fakeUploadAcceptor{}}).(*handler)
	h.now = func() time.Time { return now }
	h.hardTimeout = time.Hour
	h.idleTimeout = 2 * time.Hour
	response := httptest.NewRecorder()

	h.ServeHTTP(response, request)

	assertResponse(t, response, http.StatusRequestTimeout, "elapsed-timeout-request")
	assertError(t, response, "upload_timeout", "elapsed-timeout-request")
}

type closeCheckingRecorder struct {
	*httptest.ResponseRecorder
	request             *http.Request
	closedBeforeHeaders bool
	readDeadlines       []time.Time
}

func (r *closeCheckingRecorder) WriteHeader(status int) {
	if status == http.StatusRequestEntityTooLarge {
		r.closedBeforeHeaders = r.request.Close && r.Header().Get("Connection") == "close"
	}
	r.ResponseRecorder.WriteHeader(status)
}

func (r *closeCheckingRecorder) SetReadDeadline(deadline time.Time) error {
	r.readDeadlines = append(r.readDeadlines, deadline)
	return nil
}

type advancingReadCloser struct {
	io.Reader
	afterRead func()
}

func (r *advancingReadCloser) Read(value []byte) (int, error) {
	count, err := r.Reader.Read(value)
	if r.afterRead != nil {
		r.afterRead()
		r.afterRead = nil
	}
	return count, err
}

func (*advancingReadCloser) Close() error { return nil }

type timeoutReader struct{}

func (timeoutReader) Read([]byte) (int, error) { return 0, timeoutReadError{} }

type timeoutReadError struct{}

func (timeoutReadError) Error() string   { return "read timed out" }
func (timeoutReadError) Timeout() bool   { return true }
func (timeoutReadError) Temporary() bool { return true }

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (r *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	r.readDeadlines = append(r.readDeadlines, deadline)
	return nil
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.writeDeadlines = append(r.writeDeadlines, deadline)
	return nil
}

func TestUploadRenewsAndTransitionsDeadlines(t *testing.T) {
	acceptor := &fakeUploadAcceptor{}
	h := NewHandler(Dependencies{Upload: acceptor}).(*handler)
	now := time.Unix(1_000, 0)
	h.now = func() time.Time { return now }
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := uploadHTTPReq(rawMultipart("b", `form-data; name="file"`, "", nil, "content", ""), "multipart/form-data; boundary=b", "key", "deadline-request")

	h.ServeHTTP(response, request)

	assertResponse(t, response.ResponseRecorder, http.StatusCreated, "deadline-request")
	if len(response.readDeadlines) < 3 {
		t.Fatalf("read deadline updates = %v, want initial, renewal, and clear", response.readDeadlines)
	}
	cleared := false
	for _, deadline := range response.readDeadlines {
		if deadline.IsZero() {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("read deadline was not cleared: %v", response.readDeadlines)
	}
	if len(response.writeDeadlines) != 1 || !response.writeDeadlines[0].Equal(now.Add(uploadWriteTimeout)) {
		t.Fatalf("write deadlines = %v", response.writeDeadlines)
	}
}

func rawMultipart(boundary, disposition, contentType string, extraHeaders []string, content, epilogue string) string {
	var body strings.Builder
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: " + disposition + "\r\n")
	if contentType != "" {
		body.WriteString("Content-Type: " + contentType + "\r\n")
	}
	for _, header := range extraHeaders {
		body.WriteString(header + "\r\n")
	}
	body.WriteString("\r\n" + content + "\r\n--" + boundary + "--\r\n" + epilogue)
	return body.String()
}

func uploadHTTPReq(body, contentType, key, requestID string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/media", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Request-ID", requestID)
	return request
}

func stringPointerTest(value string) *string { return &value }

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
