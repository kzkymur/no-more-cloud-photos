package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

type readRouteKind uint8

const (
	readMediaList readRouteKind = iota + 1
	readMedia
	readOriginal
	readDisplay
	readThumbnail
	readRendition
	readJobList
	readJob
	readProfileList
)

type readRoute struct {
	kind readRouteKind
	id   string
}

func matchReadRoute(path string) (readRoute, bool) {
	switch path {
	case "/jobs":
		return readRoute{kind: readJobList}, true
	case "/profiles":
		return readRoute{kind: readProfileList}, true
	}

	segments := strings.Split(path, "/")
	if len(segments) == 3 && segments[0] == "" && segments[2] != "" {
		switch segments[1] {
		case "media":
			return readRoute{kind: readMedia, id: segments[2]}, true
		case "renditions":
			return readRoute{kind: readRendition, id: segments[2]}, true
		case "jobs":
			return readRoute{kind: readJob, id: segments[2]}, true
		}
	}
	if len(segments) == 4 && segments[0] == "" && segments[1] == "media" && segments[2] != "" {
		route := readRoute{id: segments[2]}
		switch segments[3] {
		case "original":
			route.kind = readOriginal
		case "display":
			route.kind = readDisplay
		case "thumbnail":
			route.kind = readThumbnail
		default:
			return readRoute{}, false
		}
		return route, true
	}
	return readRoute{}, false
}

func (h *handler) read(w http.ResponseWriter, r *http.Request, requestID string, route readRoute) {
	if !acceptsJSON(r.Header.Get("Accept")) {
		closeIfDeclaredBody(w, r)
		writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
		return
	}
	if readRequestHasBody(r) {
		closeUploadConnection(w, r)
		h.writeReadError(w, requestID, readapi.NewInvalidRequest(map[string]string{"body": "invalid"}))
		return
	}

	query, err := parseReadQuery(r.URL.RawQuery, route.kind)
	if err != nil {
		h.writeReadError(w, requestID, err)
		return
	}
	if h.dependencies.Reads == nil {
		h.writeReadError(w, requestID, readapi.NewDatabaseUnavailableError(nil))
		return
	}

	var response any
	switch route.kind {
	case readMediaList:
		result, serviceErr := h.dependencies.Reads.ListMedia(r.Context(), query.media)
		response, err = readapi.NewMediaPage(result.Items, result.NextCursor), serviceErr
	case readMedia:
		result, serviceErr := h.dependencies.Reads.GetMedia(r.Context(), route.id)
		result.InitializeArrays()
		response, err = result, serviceErr
	case readOriginal:
		response, err = h.dependencies.Reads.GetOriginal(r.Context(), route.id)
	case readDisplay:
		response, err = h.dependencies.Reads.GetCurrentRendition(r.Context(), route.id, "standard")
	case readThumbnail:
		response, err = h.dependencies.Reads.GetCurrentRendition(r.Context(), route.id, "thumbnail")
	case readRendition:
		response, err = h.dependencies.Reads.GetRendition(r.Context(), route.id)
	case readJobList:
		result, serviceErr := h.dependencies.Reads.ListJobs(r.Context(), query.jobs)
		response, err = readapi.NewJobPage(result.Items, result.NextCursor), serviceErr
	case readJob:
		result, serviceErr := h.dependencies.Reads.GetJob(r.Context(), route.id)
		result.InitializeArrays()
		response, err = result, serviceErr
	case readProfileList:
		result, serviceErr := h.dependencies.Reads.ListProfiles(r.Context(), query.profiles)
		response, err = readapi.NewProfilePage(result.Items), serviceErr
	}
	if err != nil {
		h.writeReadError(w, requestID, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func readRequestHasBody(r *http.Request) bool {
	if requestDeclaresBody(r) {
		return true
	}
	if r.Body == nil || r.Body == http.NoBody || r.GetBody == nil {
		return false
	}
	body, err := r.GetBody()
	if err != nil {
		return true
	}
	defer body.Close()
	var one [1]byte
	count, err := body.Read(one[:])
	return count != 0 || err != nil && !errors.Is(err, io.EOF)
}

func requestDeclaresBody(r *http.Request) bool {
	return r.ContentLength > 0 || len(r.TransferEncoding) != 0
}

func closeIfDeclaredBody(w http.ResponseWriter, r *http.Request) {
	if requestDeclaresBody(r) {
		closeUploadConnection(w, r)
	}
}

type readQuery struct {
	media    readapi.MediaListRequest
	jobs     readapi.JobListRequest
	profiles readapi.ProfileListRequest
}

var readProfileKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func parseReadQuery(raw string, kind readRouteKind) (readQuery, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return readQuery{}, readapi.NewInvalidRequest(map[string]string{"query": "invalid"})
	}

	allowed := map[string]bool{}
	switch kind {
	case readMediaList:
		allowed = map[string]bool{"profile": true, "deleted": true, "cursor": true, "limit": true}
	case readJobList:
		allowed = map[string]bool{"status": true, "media_id": true, "cursor": true, "limit": true}
	case readProfileList:
		allowed = map[string]bool{"status": true}
	}
	fields := make(map[string]string)
	for key, occurrences := range values {
		if !allowed[key] || len(occurrences) != 1 || occurrences[0] == "" {
			fields[key] = "invalid"
		}
	}
	if len(fields) != 0 {
		return readQuery{}, readapi.NewInvalidRequest(fields)
	}

	query := readQuery{
		media: readapi.NewMediaListRequest(),
		jobs:  readapi.NewJobListRequest(),
	}
	switch kind {
	case readMediaList:
		if value, ok := singleValue(values, "profile"); ok {
			query.media.Profile = value
			if !readProfileKeyPattern.MatchString(value) {
				fields["profile"] = "invalid"
			}
		}
		if value, ok := singleValue(values, "deleted"); ok {
			parsed, parseErr := readapi.ParseDeletedFilter(value)
			if parseErr != nil {
				fields["deleted"] = "invalid"
			} else {
				query.media.Deleted = parsed
			}
		}
		query.media.Cursor, _ = singleValue(values, "cursor")
		if value, ok := singleValue(values, "limit"); ok {
			query.media.Limit, fields["limit"] = parseLimit(value)
		}
	case readJobList:
		if value, ok := singleValue(values, "status"); ok {
			parsed, parseErr := readapi.ParseJobStatus(value)
			if parseErr != nil {
				fields["status"] = "invalid"
			} else {
				query.jobs.Status = parsed
			}
		}
		query.jobs.MediaID, _ = singleValue(values, "media_id")
		if query.jobs.MediaID != "" && !readapi.IsUUIDv4(query.jobs.MediaID) {
			fields["media_id"] = "invalid"
		}
		query.jobs.Cursor, _ = singleValue(values, "cursor")
		if value, ok := singleValue(values, "limit"); ok {
			query.jobs.Limit, fields["limit"] = parseLimit(value)
		}
	case readProfileList:
		if value, ok := singleValue(values, "status"); ok {
			parsed, parseErr := readapi.ParseProfileStatus(value)
			if parseErr != nil {
				fields["status"] = "invalid"
			} else {
				query.profiles.Status = parsed
			}
		}
	}
	for field, reason := range fields {
		if reason == "" {
			delete(fields, field)
		}
	}
	if len(fields) != 0 {
		return readQuery{}, readapi.NewInvalidRequest(fields)
	}
	return query, nil
}

func singleValue(values url.Values, key string) (string, bool) {
	entries, ok := values[key]
	if !ok {
		return "", false
	}
	return entries[0], true
}

func parseLimit(value string) (int, string) {
	if value == "" || value[0] == '0' {
		return 0, "invalid"
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, "invalid"
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed > readapi.MaxLimit {
		return 0, "out_of_range"
	}
	return int(parsed), ""
}

func (h *handler) writeReadError(w http.ResponseWriter, requestID string, err error) {
	var semantic *readapi.SemanticError
	if errors.As(err, &semantic) {
		writeJSON(w, semantic.HTTPStatus(), semantic.Response(requestID))
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", requestID)
}
