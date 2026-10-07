package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

const maxLifecycleJSONBodyBytes = 1 << 20
const lifecycleJSONBodyTimeout = 30 * time.Second

type lifecycleRouteKind uint8

const (
	lifecycleDelete lifecycleRouteKind = iota + 1
	lifecycleRestore
	lifecyclePurge
)

type lifecycleRoute struct {
	kind   lifecycleRouteKind
	id     string
	method string
}

func matchLifecycleRoute(path string) (lifecycleRoute, bool) {
	segments := strings.Split(path, "/")
	if len(segments) != 4 || segments[0] != "" || segments[1] != "media" || segments[2] == "" {
		return lifecycleRoute{}, false
	}
	route := lifecycleRoute{id: segments[2]}
	switch segments[3] {
	case "restore":
		route.kind, route.method = lifecycleRestore, http.MethodPost
	case "purge":
		route.kind, route.method = lifecyclePurge, http.MethodDelete
	default:
		return lifecycleRoute{}, false
	}
	return route, true
}

func (h *handler) lifecycle(w http.ResponseWriter, r *http.Request, requestID string, kind lifecycleRouteKind, id string) {
	if kind == lifecycleRestore {
		if !h.validRestoreBody(w, r, requestID) {
			return
		}
	} else if readRequestHasBody(w, r) {
		closeUploadConnection(w, r)
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"body": "invalid"}))
		return
	}
	if !acceptsJSON(acceptHeader(r)) {
		writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"query": "invalid"}))
		return
	}
	if h.dependencies.Lifecycle == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is temporarily unavailable", requestID)
		return
	}

	var response any
	var err error
	switch kind {
	case lifecycleDelete:
		var result medialifecycle.DeleteResult
		result, err = h.dependencies.Lifecycle.Delete(r.Context(), id)
		result.Media.InitializeArrays()
		response = result.Media
	case lifecycleRestore:
		var result medialifecycle.RestoreResult
		result, err = h.dependencies.Lifecycle.Restore(r.Context(), id)
		result.Media.InitializeArrays()
		response = result.Media
	case lifecyclePurge:
		var result medialifecycle.EnqueueResult
		result, err = h.dependencies.Lifecycle.EnqueuePurge(r.Context(), id)
		result.Job.InitializeArrays()
		response = result.Job
	}
	if err != nil {
		h.writeLifecycleError(w, requestID, err)
		return
	}
	status := http.StatusOK
	if kind == lifecyclePurge {
		status = http.StatusAccepted
	}
	writeJSON(w, status, response)
}

func (h *handler) validRestoreBody(w http.ResponseWriter, r *http.Request, requestID string) bool {
	if r.ContentLength > maxLifecycleJSONBodyBytes {
		closeUploadConnection(w, r)
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"body": "invalid"}))
		return false
	}
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	contentTypeIsJSON := hasJSONContentType(r.Header.Values("Content-Type"))
	if (r.ContentLength > 0 || len(r.TransferEncoding) != 0) && !contentTypeIsJSON {
		closeUploadConnection(w, r)
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", requestID)
		return false
	}
	if !contentTypeIsJSON {
		// HTTP/2 can represent an END_STREAM request with an unknown-length
		// internal body wrapper. Probe without waiting: EOF proves the body is
		// absent; data or a stream which might later produce data requires JSON.
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now())
		var one [1]byte
		count, err := r.Body.Read(one[:])
		if count == 0 && errors.Is(err, io.EOF) {
			_ = controller.SetReadDeadline(time.Time{})
			return true
		}
		closeUploadConnection(w, r)
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", requestID)
		return false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(lifecycleJSONBodyTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLifecycleJSONBodyBytes+1))
	if err != nil || len(body) > maxLifecycleJSONBodyBytes {
		closeUploadConnection(w, r)
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"body": "invalid"}))
		return false
	}
	_ = controller.SetReadDeadline(time.Time{})
	if len(body) == 0 {
		return true
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil || len(object) != 0 {
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"body": "invalid"}))
		return false
	}
	return true
}

func hasJSONContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func (h *handler) writeLifecycleError(w http.ResponseWriter, requestID string, err error) {
	var semantic *medialifecycle.SemanticError
	if errors.As(err, &semantic) {
		writeJSON(w, semantic.HTTPStatus(), errorResponse{Error: apiError{
			Code: string(semantic.Code()), Message: semantic.Message(), RequestID: requestID, Details: semantic.Details(),
		}})
		return
	}
	var unknown *medialifecycle.CommitOutcomeUnknown
	if errors.As(err, &unknown) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is temporarily unavailable", requestID)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", requestID)
}
