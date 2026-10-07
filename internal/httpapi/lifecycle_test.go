package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

const lifecycleTestMediaID = "123e4567-e89b-42d3-a456-426614174000"

type recordingLifecycleService struct {
	calls         []string
	ids           []string
	deleteResult  medialifecycle.DeleteResult
	restoreResult medialifecycle.RestoreResult
	purgeResult   medialifecycle.EnqueueResult
	err           error
}

func (s *recordingLifecycleService) Delete(_ context.Context, id string) (medialifecycle.DeleteResult, error) {
	s.record("Delete", id)
	return s.deleteResult, s.err
}

func (s *recordingLifecycleService) Restore(_ context.Context, id string) (medialifecycle.RestoreResult, error) {
	s.record("Restore", id)
	return s.restoreResult, s.err
}

func (s *recordingLifecycleService) EnqueuePurge(_ context.Context, id string) (medialifecycle.EnqueueResult, error) {
	s.record("EnqueuePurge", id)
	return s.purgeResult, s.err
}

func (s *recordingLifecycleService) record(method, id string) {
	s.calls = append(s.calls, method)
	s.ids = append(s.ids, id)
}

func TestLifecycleRoutesDispatchAndResponseShapes(t *testing.T) {
	service := &recordingLifecycleService{
		deleteResult:  medialifecycle.DeleteResult{Media: readapi.MediaDetail{ID: lifecycleTestMediaID}},
		restoreResult: medialifecycle.RestoreResult{Media: readapi.MediaDetail{ID: lifecycleTestMediaID}},
		purgeResult:   medialifecycle.EnqueueResult{Job: readapi.Job{ID: "223e4567-e89b-42d3-a456-426614174000"}},
	}
	handler := NewHandler(Dependencies{Lifecycle: service})
	tests := []struct {
		method, path, call string
		status             int
	}{
		{method: http.MethodDelete, path: "/media/" + lifecycleTestMediaID, call: "Delete", status: http.StatusOK},
		{method: http.MethodPost, path: "/media/" + lifecycleTestMediaID + "/restore", call: "Restore", status: http.StatusOK},
		{method: http.MethodDelete, path: "/media/" + lifecycleTestMediaID + "/purge", call: "EnqueuePurge", status: http.StatusAccepted},
	}
	for _, test := range tests {
		t.Run(test.call, func(t *testing.T) {
			service.calls, service.ids = nil, nil
			response := serve(handler, test.method, test.path, "lifecycle-success")
			assertResponse(t, response, test.status, "lifecycle-success")
			if !reflect.DeepEqual(service.calls, []string{test.call}) || !reflect.DeepEqual(service.ids, []string{lifecycleTestMediaID}) {
				t.Fatalf("calls/ids = %v/%v", service.calls, service.ids)
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if test.call == "EnqueuePurge" {
				if !reflect.DeepEqual(body["targets"], []any{}) || body["error"] != nil || body["original_id"] != nil {
					t.Fatalf("purge job shape = %#v", body)
				}
			} else if !reflect.DeepEqual(body["current_renditions"], []any{}) || !reflect.DeepEqual(body["jobs"], []any{}) {
				t.Fatalf("media detail shape = %#v", body)
			}
		})
	}
}

func TestLifecycleExactMethodsPathsAndQueries(t *testing.T) {
	service := &recordingLifecycleService{}
	handler := NewHandler(Dependencies{Lifecycle: service, Reads: &recordingReadService{}})
	methodTests := []struct {
		path, allow string
	}{
		{path: "/media/" + lifecycleTestMediaID, allow: "GET, DELETE"},
		{path: "/media/" + lifecycleTestMediaID + "/restore", allow: "POST"},
		{path: "/media/" + lifecycleTestMediaID + "/purge", allow: "DELETE"},
	}
	for _, test := range methodTests {
		for _, method := range []string{http.MethodHead, http.MethodPatch} {
			response := serve(handler, method, test.path, "lifecycle-method")
			assertResponse(t, response, http.StatusMethodNotAllowed, "lifecycle-method")
			if response.Header().Get("Allow") != test.allow {
				t.Fatalf("%s %s Allow = %q", method, test.path, response.Header().Get("Allow"))
			}
		}
	}

	for _, path := range []string{
		"/media/", "/media//restore", "/media/" + lifecycleTestMediaID + "/",
		"/media/" + lifecycleTestMediaID + "/restore/", "/media/" + lifecycleTestMediaID + "/purge/extra",
		"/media/" + lifecycleTestMediaID + "%2Frestore", "/media/" + lifecycleTestMediaID + "%2Fpurge",
	} {
		response := serve(handler, http.MethodDelete, path, "lifecycle-path")
		assertResponse(t, response, http.StatusNotFound, "lifecycle-path")
		assertError(t, response, "not_found", "lifecycle-path")
	}

	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/media/" + lifecycleTestMediaID + "?x=1"},
		{http.MethodGet, "/media/" + lifecycleTestMediaID + "?"},
		{http.MethodDelete, "/media/" + lifecycleTestMediaID + "?x=1"},
		{http.MethodPost, "/media/" + lifecycleTestMediaID + "/restore?x=1"},
		{http.MethodDelete, "/media/" + lifecycleTestMediaID + "/purge?x=1&x=2"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("X-Request-ID", "lifecycle-query")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertResponse(t, response, http.StatusBadRequest, "lifecycle-query")
		assertReadFields(t, response, map[string]string{"query": "invalid"})
	}
	if len(service.calls) != 0 {
		t.Fatalf("lifecycle called for rejected transport: %v", service.calls)
	}
}

func TestLifecycleDeleteRoutesRequireAbsentBody(t *testing.T) {
	for _, path := range []string{"/media/" + lifecycleTestMediaID, "/media/" + lifecycleTestMediaID + "/purge"} {
		for _, configure := range []func(*http.Request){
			func(request *http.Request) { request.ContentLength = 1 },
			func(request *http.Request) { request.TransferEncoding = []string{"chunked"} },
		} {
			service := &recordingLifecycleService{}
			request := httptest.NewRequest(http.MethodDelete, path, nil)
			request.Body = panicReadCloser{}
			configure(request)
			request.Header.Set("X-Request-ID", "lifecycle-body")
			response := httptest.NewRecorder()
			NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
			assertResponse(t, response, http.StatusBadRequest, "lifecycle-body")
			assertReadFields(t, response, map[string]string{"body": "invalid"})
			if len(service.calls) != 0 || response.Header().Get("Connection") != "close" || !request.Close {
				t.Fatalf("calls/connection = %v/%q/%v", service.calls, response.Header().Get("Connection"), request.Close)
			}
		}
	}
}

func TestLifecycleRestoreBodyContract(t *testing.T) {
	tests := []struct {
		name, body, contentType string
		wantStatus              int
	}{
		{name: "absent", wantStatus: http.StatusOK},
		{name: "empty object", body: `{}`, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "empty object whitespace", body: " \n{}\t", contentType: "application/json; charset=utf-8", wantStatus: http.StatusOK},
		{name: "whitespace only", body: " \n\t", contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "null", body: `null`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "array", body: `[]`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "scalar", body: `1`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"x":1}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "trailing value", body: `{} {}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "malformed", body: `{`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing content type", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "wrong content type", body: `{}`, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingLifecycleService{}
			var body io.Reader
			if test.body != "" {
				body = strings.NewReader(test.body)
			}
			request := httptest.NewRequest(http.MethodPost, "/media/"+lifecycleTestMediaID+"/restore", body)
			request.Header.Set("X-Request-ID", "restore-body")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
			assertResponse(t, response, test.wantStatus, "restore-body")
			if test.wantStatus == http.StatusOK {
				if !reflect.DeepEqual(service.calls, []string{"Restore"}) {
					t.Fatalf("calls = %v", service.calls)
				}
			} else {
				if len(service.calls) != 0 {
					t.Fatalf("service called: %v", service.calls)
				}
				if test.wantStatus == http.StatusUnsupportedMediaType {
					assertError(t, response, "unsupported_media_type", "restore-body")
				} else {
					assertReadFields(t, response, map[string]string{"body": "invalid"})
				}
			}
		})
	}
}

func TestLifecycleRestoreRejectsDuplicateContentTypeAndOversize(t *testing.T) {
	service := &recordingLifecycleService{}
	request := httptest.NewRequest(http.MethodPost, "/media/"+lifecycleTestMediaID+"/restore", strings.NewReader(`{}`))
	request.Header["Content-Type"] = []string{"application/json", "application/json"}
	request.Header.Set("X-Request-ID", "restore-content-type")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusUnsupportedMediaType, "restore-content-type")
	assertError(t, response, "unsupported_media_type", "restore-content-type")

	request = httptest.NewRequest(http.MethodPost, "/media/"+lifecycleTestMediaID+"/restore", strings.NewReader(strings.Repeat(" ", maxLifecycleJSONBodyBytes+1)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "restore-oversize")
	response = httptest.NewRecorder()
	NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusBadRequest, "restore-oversize")
	assertReadFields(t, response, map[string]string{"body": "invalid"})
	if len(service.calls) != 0 {
		t.Fatalf("service called: %v", service.calls)
	}
}

func TestLifecycleRestoreAcceptsUnknownLengthAbsentBody(t *testing.T) {
	service := &recordingLifecycleService{}
	request := httptest.NewRequest(http.MethodPost, "/media/"+lifecycleTestMediaID+"/restore", nil)
	request.Body = io.NopCloser(strings.NewReader(""))
	request.ContentLength = -1
	request.Header.Set("X-Request-ID", "restore-unknown-empty")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusOK, "restore-unknown-empty")
	if !reflect.DeepEqual(service.calls, []string{"Restore"}) {
		t.Fatalf("calls = %v", service.calls)
	}
}

func TestLifecycleNegotiationPrecedesQueryValidation(t *testing.T) {
	service := &recordingLifecycleService{}
	request := httptest.NewRequest(http.MethodDelete, "/media/"+lifecycleTestMediaID+"?invalid=1", nil)
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("X-Request-ID", "lifecycle-precedence")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusNotAcceptable, "lifecycle-precedence")
	assertError(t, response, "not_acceptable", "lifecycle-precedence")
	if len(service.calls) != 0 {
		t.Fatalf("service called: %v", service.calls)
	}
}

func TestLifecycleNegotiationNilDependencyAndErrors(t *testing.T) {
	service := &recordingLifecycleService{}
	request := httptest.NewRequest(http.MethodDelete, "/media/"+lifecycleTestMediaID, nil)
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("X-Request-ID", "lifecycle-accept")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Lifecycle: service}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusNotAcceptable, "lifecycle-accept")
	assertError(t, response, "not_acceptable", "lifecycle-accept")
	if len(service.calls) != 0 {
		t.Fatalf("service called: %v", service.calls)
	}

	response = serve(NewHandler(Dependencies{}), http.MethodDelete, "/media/"+lifecycleTestMediaID, "lifecycle-nil")
	assertResponse(t, response, http.StatusServiceUnavailable, "lifecycle-nil")
	assertError(t, response, "unavailable", "lifecycle-nil")

	service.err = &medialifecycle.CommitOutcomeUnknown{Cause: errors.New("password=secret")}
	response = serve(NewHandler(Dependencies{Lifecycle: service}), http.MethodDelete, "/media/"+lifecycleTestMediaID, "lifecycle-unknown-commit")
	assertResponse(t, response, http.StatusServiceUnavailable, "lifecycle-unknown-commit")
	assertError(t, response, "unavailable", "lifecycle-unknown-commit")
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("commit cause leaked: %s", response.Body.String())
	}

	service.err = errors.New("filesystem=/private token=secret")
	response = serve(NewHandler(Dependencies{Lifecycle: service}), http.MethodDelete, "/media/"+lifecycleTestMediaID, "lifecycle-internal")
	assertResponse(t, response, http.StatusInternalServerError, "lifecycle-internal")
	assertError(t, response, "internal_error", "lifecycle-internal")
	if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("internal cause leaked: %s", response.Body.String())
	}

	service.err = medialifecycle.ErrNoPurgeWork
	response = serve(NewHandler(Dependencies{Lifecycle: service}), http.MethodDelete, "/media/"+lifecycleTestMediaID+"/purge", "lifecycle-internal-only")
	assertResponse(t, response, http.StatusInternalServerError, "lifecycle-internal-only")
	assertError(t, response, "internal_error", "lifecycle-internal-only")
}

func TestLifecycleDeleteTricklingHTTP11BodyRespondsPromptlyAndCloses(t *testing.T) {
	service := &recordingLifecycleService{}
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(NewHandler(Dependencies{Lifecycle: service}))
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
	request := "DELETE /media/" + lifecycleTestMediaID + " HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\nX-Request-ID: lifecycle-socket\r\n\r\n1\r\nx\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodDelete})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || !response.Close {
		t.Fatalf("status/close = %d/%v; body=%s", response.StatusCode, response.Close, body)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("connection remained reusable: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("server connection did not close")
	}
	if len(service.calls) != 0 {
		t.Fatalf("service called: %v", service.calls)
	}
}

func TestLifecycleHTTP2UnknownLengthBodies(t *testing.T) {
	service := &recordingLifecycleService{}
	server := httptest.NewUnstartedServer(NewHandler(Dependencies{Lifecycle: service}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodDelete, server.URL+"/media/"+lifecycleTestMediaID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Body = io.NopCloser(strings.NewReader("x"))
	request.ContentLength = -1
	request.Header.Set("X-Request-ID", "lifecycle-http2-delete")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("protocol/status = %s/%d; body=%s", response.Proto, response.StatusCode, body)
	}
	if len(service.calls) != 0 {
		t.Fatalf("delete body reached service: %v", service.calls)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/media/"+lifecycleTestMediaID+"/restore", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Body = io.NopCloser(strings.NewReader(`{}`))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "lifecycle-http2-restore")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr = io.Copy(io.Discard, response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
		t.Fatalf("restore protocol/status = %s/%d", response.Proto, response.StatusCode)
	}
	if !reflect.DeepEqual(service.calls, []string{"Restore"}) {
		t.Fatalf("calls = %v", service.calls)
	}
}
