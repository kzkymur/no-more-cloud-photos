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

	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

type recordingReadService struct {
	calls       []string
	mediaList   readapi.MediaListRequest
	jobList     readapi.JobListRequest
	profileList readapi.ProfileListRequest
	ids         []string
	profileKey  string
	result      any
	err         error
}

func (f *recordingReadService) ListMedia(_ context.Context, request readapi.MediaListRequest) (readapi.MediaPage, error) {
	f.calls = append(f.calls, "ListMedia")
	f.mediaList = request
	if result, ok := f.result.(readapi.MediaPage); ok {
		return result, f.err
	}
	return readapi.MediaPage{}, f.err
}

func (f *recordingReadService) GetMedia(_ context.Context, id string) (readapi.MediaDetail, error) {
	f.record("GetMedia", id)
	if result, ok := f.result.(readapi.MediaDetail); ok {
		return result, f.err
	}
	return readapi.MediaDetail{}, f.err
}

func (f *recordingReadService) GetOriginal(_ context.Context, id string) (readapi.Original, error) {
	f.record("GetOriginal", id)
	if result, ok := f.result.(readapi.Original); ok {
		return result, f.err
	}
	return readapi.Original{}, f.err
}

func (f *recordingReadService) GetCurrentRendition(_ context.Context, id, profile string) (readapi.Rendition, error) {
	f.record("GetCurrentRendition", id)
	f.profileKey = profile
	if result, ok := f.result.(readapi.Rendition); ok {
		return result, f.err
	}
	return readapi.Rendition{}, f.err
}

func (f *recordingReadService) GetRendition(_ context.Context, id string) (readapi.Rendition, error) {
	f.record("GetRendition", id)
	if result, ok := f.result.(readapi.Rendition); ok {
		return result, f.err
	}
	return readapi.Rendition{}, f.err
}

func (f *recordingReadService) ListJobs(_ context.Context, request readapi.JobListRequest) (readapi.JobPage, error) {
	f.calls = append(f.calls, "ListJobs")
	f.jobList = request
	if result, ok := f.result.(readapi.JobPage); ok {
		return result, f.err
	}
	return readapi.JobPage{}, f.err
}

func (f *recordingReadService) GetJob(_ context.Context, id string) (readapi.Job, error) {
	f.record("GetJob", id)
	if result, ok := f.result.(readapi.Job); ok {
		return result, f.err
	}
	return readapi.Job{}, f.err
}

func (f *recordingReadService) ListProfiles(_ context.Context, request readapi.ProfileListRequest) (readapi.ProfilePage, error) {
	f.calls = append(f.calls, "ListProfiles")
	f.profileList = request
	if result, ok := f.result.(readapi.ProfilePage); ok {
		return result, f.err
	}
	return readapi.ProfilePage{}, f.err
}

func (f *recordingReadService) record(method, id string) {
	f.calls = append(f.calls, method)
	f.ids = append(f.ids, id)
}

func TestReadRoutesDispatchExactly(t *testing.T) {
	const id = "not-adapter-validated"
	tests := []struct {
		path       string
		method     string
		profileKey string
	}{
		{path: "/media", method: "ListMedia"},
		{path: "/media/" + id, method: "GetMedia"},
		{path: "/media/" + id + "/original", method: "GetOriginal"},
		{path: "/media/" + id + "/display", method: "GetCurrentRendition", profileKey: "standard"},
		{path: "/media/" + id + "/thumbnail", method: "GetCurrentRendition", profileKey: "thumbnail"},
		{path: "/renditions/" + id, method: "GetRendition"},
		{path: "/jobs", method: "ListJobs"},
		{path: "/jobs/" + id, method: "GetJob"},
		{path: "/profiles", method: "ListProfiles"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			reads := &recordingReadService{}
			response := serve(NewHandler(Dependencies{Reads: reads}), http.MethodGet, test.path, "route-request")
			assertResponse(t, response, http.StatusOK, "route-request")
			if !reflect.DeepEqual(reads.calls, []string{test.method}) {
				t.Fatalf("calls = %v, want %s", reads.calls, test.method)
			}
			if test.method != "ListMedia" && test.method != "ListJobs" && test.method != "ListProfiles" && !reflect.DeepEqual(reads.ids, []string{id}) {
				t.Fatalf("ids = %v, want %q", reads.ids, id)
			}
			if reads.profileKey != test.profileKey {
				t.Fatalf("profile key = %q, want %q", reads.profileKey, test.profileKey)
			}
		})
	}
}

func TestReadRoutesRejectMethodsAndInexactPaths(t *testing.T) {
	reads := &recordingReadService{}
	handler := NewHandler(Dependencies{Reads: reads})
	for _, path := range []string{"/media/id", "/media/id/original", "/renditions/id", "/jobs", "/jobs/id", "/profiles"} {
		response := serve(handler, http.MethodPost, path, "method-request")
		assertResponse(t, response, http.StatusMethodNotAllowed, "method-request")
		if got := response.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s Allow = %q", path, got)
		}
	}
	response := serve(handler, http.MethodPut, "/media", "media-method-request")
	assertResponse(t, response, http.StatusMethodNotAllowed, "media-method-request")
	if got := response.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("/media Allow = %q", got)
	}

	for _, path := range []string{
		"/media/", "/media//x", "/media/id/", "/media/id/original/", "/media/id/original/extra",
		"/renditions/", "/renditions/id/extra", "/jobs/", "/jobs/id/extra", "/profiles/", "/media/id%2Foriginal",
	} {
		response := serve(handler, http.MethodGet, path, "exact-request")
		assertResponse(t, response, http.StatusNotFound, "exact-request")
		assertError(t, response, "not_found", "exact-request")
	}
	if len(reads.calls) != 0 {
		t.Fatalf("service called for rejected route/method: %v", reads.calls)
	}
}

func TestReadListQueries(t *testing.T) {
	const mediaID = "123e4567-e89b-42d3-a456-426614174000"
	reads := &recordingReadService{}
	handler := NewHandler(Dependencies{Reads: reads})

	response := serve(handler, http.MethodGet, "/media?profile=custom_1&deleted=include&cursor=opaque&limit=200", "media-query")
	assertResponse(t, response, http.StatusOK, "media-query")
	wantMedia := readapi.MediaListRequest{Profile: "custom_1", Deleted: readapi.DeletedInclude, Cursor: "opaque", Limit: 200}
	if reads.mediaList != wantMedia {
		t.Fatalf("media query = %#v, want %#v", reads.mediaList, wantMedia)
	}

	response = serve(handler, http.MethodGet, "/jobs?status=running&media_id="+mediaID+"&cursor=opaque&limit=1", "jobs-query")
	assertResponse(t, response, http.StatusOK, "jobs-query")
	wantJobs := readapi.JobListRequest{Status: readapi.JobRunning, MediaID: mediaID, Cursor: "opaque", Limit: 1}
	if reads.jobList != wantJobs {
		t.Fatalf("job query = %#v, want %#v", reads.jobList, wantJobs)
	}

	response = serve(handler, http.MethodGet, "/profiles?status=retired", "profiles-query")
	assertResponse(t, response, http.StatusOK, "profiles-query")
	if reads.profileList.Status != readapi.ProfileRetired {
		t.Fatalf("profile status = %q", reads.profileList.Status)
	}

	defaults := &recordingReadService{}
	response = serve(NewHandler(Dependencies{Reads: defaults}), http.MethodGet, "/media", "defaults")
	assertResponse(t, response, http.StatusOK, "defaults")
	if defaults.mediaList != readapi.NewMediaListRequest() {
		t.Fatalf("media defaults = %#v", defaults.mediaList)
	}
}

func TestReadQueriesAreStrict(t *testing.T) {
	tests := []struct {
		path   string
		fields map[string]string
	}{
		{path: "/media?unknown=x", fields: map[string]string{"unknown": "invalid"}},
		{path: "/media?profile=", fields: map[string]string{"profile": "invalid"}},
		{path: "/media?profile=UPPER", fields: map[string]string{"profile": "invalid"}},
		{path: "/media?deleted=all", fields: map[string]string{"deleted": "invalid"}},
		{path: "/media?cursor=", fields: map[string]string{"cursor": "invalid"}},
		{path: "/media?limit=1&limit=2", fields: map[string]string{"limit": "invalid"}},
		{path: "/media?limit=0", fields: map[string]string{"limit": "invalid"}},
		{path: "/media?limit=00", fields: map[string]string{"limit": "invalid"}},
		{path: "/media?limit=01", fields: map[string]string{"limit": "invalid"}},
		{path: "/media?limit=+1", fields: map[string]string{"limit": "invalid"}},
		{path: "/media?limit=201", fields: map[string]string{"limit": "out_of_range"}},
		{path: "/media?limit=999999999999999999999999", fields: map[string]string{"limit": "out_of_range"}},
		{path: "/jobs?status=", fields: map[string]string{"status": "invalid"}},
		{path: "/jobs?status=waiting", fields: map[string]string{"status": "invalid"}},
		{path: "/jobs?media_id=NOT-A-UUID", fields: map[string]string{"media_id": "invalid"}},
		{path: "/jobs?media_id=123e4567-e89b-42d3-a456-426614174000&media_id=123e4567-e89b-42d3-a456-426614174000", fields: map[string]string{"media_id": "invalid"}},
		{path: "/profiles?status=active&extra=x", fields: map[string]string{"extra": "invalid"}},
		{path: "/profiles?status=", fields: map[string]string{"status": "invalid"}},
		{path: "/profiles?status=disabled", fields: map[string]string{"status": "invalid"}},
		{path: "/media/id?x=1", fields: map[string]string{"x": "invalid"}},
		{path: "/jobs/id?cursor=x", fields: map[string]string{"cursor": "invalid"}},
		{path: "/profiles?bad;query=x", fields: map[string]string{"query": "invalid"}},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			reads := &recordingReadService{}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("X-Request-ID", "strict-request")
			response := httptest.NewRecorder()
			NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
			assertResponse(t, response, http.StatusBadRequest, "strict-request")
			assertReadFields(t, response, test.fields)
			if len(reads.calls) != 0 {
				t.Fatalf("service called: %v", reads.calls)
			}
		})
	}
}

func TestReadBodyNegotiationAndUnavailablePrecedeService(t *testing.T) {
	reads := &recordingReadService{}
	request := httptest.NewRequest(http.MethodGet, "/media?limit=bad", nil)
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("X-Request-ID", "accept-first")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusNotAcceptable, "accept-first")
	assertError(t, response, "not_acceptable", "accept-first")

	request = httptest.NewRequest(http.MethodGet, "/media", strings.NewReader("x"))
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("X-Request-ID", "body-request")
	response = httptest.NewRecorder()
	NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusBadRequest, "body-request")
	assertReadFields(t, response, map[string]string{"body": "invalid"})
	if response.Header().Get("Connection") != "close" || !request.Close {
		t.Fatalf("body rejection did not close connection: header=%q close=%v", response.Header().Get("Connection"), request.Close)
	}

	request = httptest.NewRequest(http.MethodGet, "/media", nil)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "empty-body")
	response = httptest.NewRecorder()
	NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusOK, "empty-body")

	response = serve(NewHandler(Dependencies{}), http.MethodGet, "/jobs", "nil-service")
	assertResponse(t, response, http.StatusServiceUnavailable, "nil-service")
	assertError(t, response, "unavailable", "nil-service")
	if len(reads.calls) != 1 {
		t.Fatalf("service calls = %v, want only empty body request", reads.calls)
	}
}

func TestReadDeclaredBodiesRejectWithoutReading(t *testing.T) {
	for _, configure := range []func(*http.Request){
		func(request *http.Request) { request.ContentLength = 1 },
		func(request *http.Request) { request.TransferEncoding = []string{"chunked"} },
	} {
		reads := &recordingReadService{}
		request := httptest.NewRequest(http.MethodGet, "/jobs", nil)
		request.Body = panicReadCloser{}
		configure(request)
		request.Header.Set("X-Request-ID", "declared-body")
		response := httptest.NewRecorder()
		NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
		assertResponse(t, response, http.StatusBadRequest, "declared-body")
		assertReadFields(t, response, map[string]string{"body": "invalid"})
		if len(reads.calls) != 0 {
			t.Fatalf("service called: %v", reads.calls)
		}
	}
}

func TestReadSafelyProbesAmbiguousReplayableBody(t *testing.T) {
	reads := &recordingReadService{}
	request := httptest.NewRequest(http.MethodGet, "/profiles", nil)
	request.Body = io.NopCloser(strings.NewReader("ignored actual body"))
	request.ContentLength = 0
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	}
	request.Header.Set("X-Request-ID", "probed-body")
	response := httptest.NewRecorder()
	NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusBadRequest, "probed-body")
	assertReadFields(t, response, map[string]string{"body": "invalid"})
	if len(reads.calls) != 0 {
		t.Fatalf("service called: %v", reads.calls)
	}

	request = httptest.NewRequest(http.MethodGet, "/profiles", nil)
	request.Body = io.NopCloser(strings.NewReader(""))
	request.ContentLength = 0
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "probed-empty")
	response = httptest.NewRecorder()
	NewHandler(Dependencies{Reads: reads}).ServeHTTP(response, request)
	assertResponse(t, response, http.StatusOK, "probed-empty")
}

type panicReadCloser struct{}

func (panicReadCloser) Read([]byte) (int, error) { panic("declared body was read") }
func (panicReadCloser) Close() error             { return nil }

func TestReadTricklingHTTP11BodyRespondsPromptlyAndCloses(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(NewHandler(Dependencies{Reads: &recordingReadService{}}))
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
	request := "GET /media HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\nX-Request-ID: socket-body\r\n\r\n1\r\nx\r\n"
	started := time.Now()
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("response took %v", elapsed)
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
}

func TestReadServiceErrorsAndResponseShapes(t *testing.T) {
	semantic := readapi.NewInvariantError(errors.New("password=secret filesystem=/private"))
	response := serve(NewHandler(Dependencies{Reads: &recordingReadService{err: semantic}}), http.MethodGet, "/media/id", "semantic-error")
	assertResponse(t, response, http.StatusInternalServerError, "semantic-error")
	assertError(t, response, "internal_error", "semantic-error")
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "/private") {
		t.Fatalf("semantic cause leaked: %s", response.Body.String())
	}

	response = serve(NewHandler(Dependencies{Reads: &recordingReadService{err: errors.New("token=secret")}}), http.MethodGet, "/jobs/id", "unknown-error")
	assertResponse(t, response, http.StatusInternalServerError, "unknown-error")
	assertError(t, response, "internal_error", "unknown-error")
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("unknown error leaked: %s", response.Body.String())
	}

	response = serve(NewHandler(Dependencies{Reads: &recordingReadService{err: readapi.NewInvalidID()}}), http.MethodGet, "/renditions/NOT-A-UUID", "invalid-id")
	assertResponse(t, response, http.StatusBadRequest, "invalid-id")
	assertError(t, response, "invalid_id", "invalid-id")

	shapeTests := []struct {
		path string
		want map[string]any
	}{
		{path: "/media", want: map[string]any{"items": []any{}, "next_cursor": nil}},
		{path: "/jobs", want: map[string]any{"items": []any{}, "next_cursor": nil}},
		{path: "/profiles", want: map[string]any{"items": []any{}}},
	}
	for _, test := range shapeTests {
		response := serve(NewHandler(Dependencies{Reads: &recordingReadService{}}), http.MethodGet, test.path, "shape-request")
		assertResponse(t, response, http.StatusOK, "shape-request")
		assertJSON(t, response, test.want)
	}

	response = serve(NewHandler(Dependencies{Reads: &recordingReadService{result: readapi.MediaDetail{}}}), http.MethodGet, "/media/id", "detail-shape")
	assertResponse(t, response, http.StatusOK, "detail-shape")
	var detail map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail["current_renditions"], []any{}) || !reflect.DeepEqual(detail["jobs"], []any{}) {
		t.Fatalf("media detail arrays = %#v/%#v", detail["current_renditions"], detail["jobs"])
	}

	response = serve(NewHandler(Dependencies{Reads: &recordingReadService{result: readapi.Job{}}}), http.MethodGet, "/jobs/id", "job-shape")
	assertResponse(t, response, http.StatusOK, "job-shape")
	var job map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(job["targets"], []any{}) || job["error"] != nil || job["original_id"] != nil {
		t.Fatalf("job null/array shape = %#v", job)
	}

	profile := readapi.Profile{Parameters: json.RawMessage(`{}`)}
	response = serve(NewHandler(Dependencies{Reads: &recordingReadService{result: readapi.ProfilePage{Items: []readapi.Profile{profile}}}}), http.MethodGet, "/profiles", "profile-shape")
	assertResponse(t, response, http.StatusOK, "profile-shape")
	var profiles map[string][]map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(profiles["items"][0]["input_mime_types"], []any{}) || profiles["items"][0]["activated_at"] != nil {
		t.Fatalf("profile null/array shape = %#v", profiles)
	}
}

func assertReadFields(t *testing.T, response *httptest.ResponseRecorder, want map[string]string) {
	t.Helper()
	var body readapi.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode read error: %v; body=%s", err, response.Body.String())
	}
	if body.Error.Code != readapi.CodeInvalidRequest {
		t.Fatalf("error code = %q", body.Error.Code)
	}
	encodedFields, ok := body.Error.Details["fields"].(map[string]any)
	if !ok {
		t.Fatalf("details.fields = %#v", body.Error.Details["fields"])
	}
	got := make(map[string]string, len(encodedFields))
	for key, value := range encodedFields {
		got[key], _ = value.(string)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("details.fields = %#v, want %#v", got, want)
	}
}
