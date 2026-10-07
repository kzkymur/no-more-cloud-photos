// Package httpapi provides the Core HTTP API handler.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/upload"
)

const (
	jsonContentType    = "application/json; charset=utf-8"
	maxUploadBodyBytes = upload.MaxOriginalBytes + 1<<20
	uploadHardTimeout  = 24 * time.Hour
	uploadIdleTimeout  = 120 * time.Second
	uploadWriteTimeout = 30 * time.Second
)

// DatabasePinger is the database capability required by the readiness probe.
type DatabasePinger interface {
	Ping(context.Context) error
}

// MigrationChecker is the migration capability required by the readiness probe.
type MigrationChecker interface {
	Status(context.Context) (database.Status, error)
}

// StorageProber is the secure storage capability required by readiness.
type StorageProber interface {
	Probe(context.Context) error
}

// UploadAcceptor is the upload capability required by POST /media.
type UploadAcceptor interface {
	Accept(context.Context, upload.Request) (upload.Outcome, error)
}

// ReadService is the read capability required by the HTTP API.
type ReadService interface {
	ListMedia(context.Context, readapi.MediaListRequest) (readapi.MediaPage, error)
	GetMedia(context.Context, string) (readapi.MediaDetail, error)
	GetOriginal(context.Context, string) (readapi.Original, error)
	GetCurrentRendition(context.Context, string, string) (readapi.Rendition, error)
	GetRendition(context.Context, string) (readapi.Rendition, error)
	ListJobs(context.Context, readapi.JobListRequest) (readapi.JobPage, error)
	GetJob(context.Context, string) (readapi.Job, error)
	ListProfiles(context.Context, readapi.ProfileListRequest) (readapi.ProfilePage, error)
}

// MediaLifecycle is the public mutation capability required by the HTTP API.
type MediaLifecycle interface {
	Delete(context.Context, string) (medialifecycle.DeleteResult, error)
	Restore(context.Context, string) (medialifecycle.RestoreResult, error)
	EnqueuePurge(context.Context, string) (medialifecycle.EnqueueResult, error)
}

// Dependencies contains the services used by the HTTP API.
type Dependencies struct {
	Database   DatabasePinger
	Migrations MigrationChecker
	Storage    StorageProber
	Upload     UploadAcceptor
	Reads      ReadService
	Lifecycle  MediaLifecycle
}

type handler struct {
	dependencies Dependencies
	maxBodyBytes int64
	hardTimeout  time.Duration
	idleTimeout  time.Duration
	writeTimeout time.Duration
	now          func() time.Time
}

type statusResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

// NewHandler creates a handler for the Core HTTP API.
func NewHandler(dependencies Dependencies) http.Handler {
	return &handler{
		dependencies: dependencies,
		maxBodyBytes: maxUploadBodyBytes,
		hardTimeout:  uploadHardTimeout,
		idleTimeout:  uploadIdleTimeout,
		writeTimeout: uploadWriteTimeout,
		now:          time.Now,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := requestID(r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Request-ID", requestID)

	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, requestID, http.MethodGet)
			return
		}
		if !acceptsJSON(acceptHeader(r)) {
			writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
			return
		}
		writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
	case "/readyz":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, requestID, http.MethodGet)
			return
		}
		if !acceptsJSON(acceptHeader(r)) {
			writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
			return
		}
		h.ready(w, r, requestID)
	case "/media":
		if r.URL.RawPath != "" {
			closeIfDeclaredBody(w, r)
			writeError(w, http.StatusNotFound, "not_found", "route not found", requestID)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.read(w, r, requestID, readRoute{kind: readMediaList})
			return
		case http.MethodPost:
		case http.MethodHead:
			fallthrough
		default:
			closeUploadConnection(w, r)
			h.methodNotAllowed(w, requestID, http.MethodGet+", "+http.MethodPost)
			return
		}
		if !acceptsJSON(acceptHeader(r)) {
			closeUploadConnection(w, r)
			writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
			return
		}
		h.upload(w, r, requestID)
	default:
		if route, ok := matchReadRoute(r.URL.Path); ok && r.URL.RawPath == "" {
			if route.kind == readMedia {
				switch r.Method {
				case http.MethodGet:
					h.read(w, r, requestID, route)
				case http.MethodDelete:
					h.lifecycle(w, r, requestID, lifecycleDelete, route.id)
				default:
					closeIfDeclaredBody(w, r)
					h.methodNotAllowed(w, requestID, http.MethodGet+", "+http.MethodDelete)
				}
				return
			}
			if r.Method != http.MethodGet {
				closeUploadConnection(w, r)
				h.methodNotAllowed(w, requestID, http.MethodGet)
				return
			}
			h.read(w, r, requestID, route)
			return
		}
		if route, ok := matchLifecycleRoute(r.URL.Path); ok && r.URL.RawPath == "" {
			if r.Method != route.method {
				closeIfDeclaredBody(w, r)
				h.methodNotAllowed(w, requestID, route.method)
				return
			}
			h.lifecycle(w, r, requestID, route.kind, route.id)
			return
		}
		closeIfDeclaredBody(w, r)
		writeError(w, http.StatusNotFound, "not_found", "route not found", requestID)
	}
}

func acceptsJSON(header string) bool {
	if strings.TrimSpace(header) == "" {
		return true
	}
	bestSpecificity := -1
	bestQuality := -1.0
	for _, part := range strings.Split(header, ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			return false
		}
		quality := 1.0
		if raw, exists := parameters["q"]; exists {
			quality, err = strconv.ParseFloat(raw, 64)
			if err != nil || quality < 0 || quality > 1 {
				return false
			}
		}
		specificity := -1
		switch strings.ToLower(mediaType) {
		case "application/json":
			specificity = 2
		case "application/*":
			specificity = 1
		case "*/*":
			specificity = 0
		}
		if specificity > bestSpecificity || specificity == bestSpecificity && quality > bestQuality {
			bestSpecificity = specificity
			bestQuality = quality
		}
	}
	return bestSpecificity >= 0 && bestQuality > 0
}

func acceptHeader(r *http.Request) string {
	return strings.Join(r.Header.Values("Accept"), ",")
}

func (h *handler) methodNotAllowed(w http.ResponseWriter, requestID, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", requestID)
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request, requestID string) {
	keyValues, keyPresent := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]
	if !keyPresent {
		closeUploadConnection(w, r)
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusBadRequest, Code: "missing_idempotency_key", Message: "idempotency key is required",
		})
		return
	}
	if len(keyValues) != 1 || upload.ValidateIdempotencyKey(keyValues[0]) != nil {
		closeUploadConnection(w, r)
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusBadRequest, Code: "invalid_idempotency_key", Message: "idempotency key is invalid",
		})
		return
	}

	boundary, err := multipartBoundary(r.Header.Values("Content-Type"))
	if err != nil {
		closeUploadConnection(w, r)
		writeUploadFailure(w, requestID, invalidMultipart("Content-Type must be multipart/form-data with a valid boundary", err))
		return
	}
	if h.dependencies.Upload == nil {
		closeUploadConnection(w, r)
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "service is temporarily unavailable",
		})
		return
	}

	hardDeadline := h.now().Add(h.hardTimeout)
	controller := http.NewResponseController(w)
	deadlineBody := &renewingBody{
		ReadCloser: r.Body,
		controller: controller,
		idle:       h.idleTimeout,
		hard:       hardDeadline,
		now:        h.now,
	}
	deadlineBody.renewDeadline()
	limitedBody := http.MaxBytesReader(w, deadlineBody, h.maxBodyBytes)
	multipartReader := multipart.NewReader(limitedBody, boundary)

	part, err := multipartReader.NextRawPart()
	if err != nil {
		h.writeUploadResult(w, r, controller, requestID, upload.Outcome{}, invalidMultipart("request must contain one file part", err))
		return
	}
	filename, err := validateFilePart(part)
	if err != nil {
		_ = part.Close()
		h.writeUploadResult(w, r, controller, requestID, upload.Outcome{}, err)
		return
	}

	body := &singlePartReader{
		part: part, multipart: multipartReader, source: limitedBody,
		complete: func() { _ = controller.SetReadDeadline(time.Time{}) },
	}
	outcome, acceptErr := h.dependencies.Upload.Accept(r.Context(), upload.Request{
		Body:           body,
		Filename:       filename,
		IdempotencyKey: keyValues[0],
		RequestID:      requestID,
	})
	h.writeUploadResult(w, r, controller, requestID, outcome, acceptErr)
}

func (h *handler) writeUploadResult(w http.ResponseWriter, r *http.Request, controller *http.ResponseController, requestID string, outcome upload.Outcome, err error) {
	_ = controller.SetWriteDeadline(h.now().Add(h.writeTimeout))
	if err == nil {
		closeUploadConnectionOnTooLarge(w, r, outcome.Status)
		if outcome.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeRawJSON(w, outcome.Status, outcome.Body)
		return
	}

	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		closeUploadConnectionOnTooLarge(w, r, http.StatusRequestEntityTooLarge)
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: "upload exceeds the size limit",
		})
		return
	}
	if r.Context().Err() != nil || errors.Is(err, errBodyReadTimeout) {
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusRequestTimeout, Code: "upload_timeout", Message: "upload timed out",
		})
		return
	}
	var responseError interface {
		error
		HTTPStatus() int
		ResponseBody(string) json.RawMessage
	}
	if errors.As(err, &responseError) {
		closeUploadConnectionOnTooLarge(w, r, responseError.HTTPStatus())
		writeRawJSON(w, responseError.HTTPStatus(), responseError.ResponseBody(requestID))
		return
	}
	if errors.Is(err, errInvalidMultipartBody) {
		writeUploadFailure(w, requestID, invalidMultipart("upload body could not be read", err))
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeUploadFailure(w, requestID, &upload.Failure{
			Status: http.StatusRequestTimeout, Code: "upload_timeout", Message: "upload timed out",
		})
		return
	}
	writeUploadFailure(w, requestID, &upload.Failure{
		Status: http.StatusInternalServerError, Code: "internal_error", Message: "an internal error occurred",
	})
}

func closeUploadConnectionOnTooLarge(w http.ResponseWriter, r *http.Request, status int) {
	if status == http.StatusRequestEntityTooLarge {
		closeUploadConnection(w, r)
	}
}

func closeUploadConnection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")
	r.Close = true
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
}

func multipartBoundary(values []string) (string, error) {
	if len(values) != 1 {
		return "", errors.New("Content-Type must occur exactly once")
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return "", errors.New("invalid multipart Content-Type")
	}
	if !hasMediaParameter(values[0], "boundary") {
		return "", errors.New("multipart boundary parameter is missing")
	}
	boundary := parameters["boundary"]
	if boundary == "" {
		return "", errors.New("multipart boundary is empty")
	}
	if !validMultipartBoundary(boundary) {
		return "", errors.New("multipart boundary is invalid")
	}
	return boundary, nil
}

func hasMediaParameter(value, want string) bool {
	quoted, escaped, start := false, false, 0
	for position := 0; position <= len(value); position++ {
		atEnd := position == len(value)
		if !atEnd {
			character := value[position]
			if escaped {
				escaped = false
				continue
			}
			if quoted && character == '\\' {
				escaped = true
				continue
			}
			if character == '"' {
				quoted = !quoted
				continue
			}
			if character != ';' || quoted {
				continue
			}
		}
		if start > 0 {
			parameter := strings.TrimSpace(value[start:position])
			if equals := strings.IndexByte(parameter, '='); equals >= 0 && strings.EqualFold(strings.TrimSpace(parameter[:equals]), want) {
				return true
			}
		}
		start = position + 1
	}
	return false
}

func validMultipartBoundary(boundary string) bool {
	if len(boundary) < 1 || len(boundary) > 70 || boundary[len(boundary)-1] == ' ' {
		return false
	}
	for _, character := range []byte(boundary) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("'()+_,-./:=? ", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func validateFilePart(part *multipart.Part) (*string, error) {
	if len(part.Header.Values("Content-Disposition")) != 1 {
		return nil, invalidMultipart("file part has invalid Content-Disposition", nil)
	}
	disposition, parameters, err := parseDisposition(part.Header.Get("Content-Disposition"))
	if err != nil || !strings.EqualFold(disposition, "form-data") || parameters["name"].value != "file" {
		return nil, invalidMultipart("request must contain exactly one part named file", err)
	}
	if _, exists := part.Header["Content-Transfer-Encoding"]; exists {
		return nil, invalidMultipart("Content-Transfer-Encoding is not allowed", nil)
	}
	if contentTypes := part.Header.Values("Content-Type"); len(contentTypes) > 0 {
		for _, contentType := range contentTypes {
			mediaType, _, parseErr := mime.ParseMediaType(contentType)
			looksMultipart := strings.HasPrefix(strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])), "multipart/")
			if looksMultipart || parseErr == nil && strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
				return nil, invalidMultipart("nested multipart content is not allowed", nil)
			}
		}
	}

	var filename *string
	if extended, exists := parameters["filename*"]; exists {
		if extended.quoted {
			return nil, invalidFilename(errors.New("filename* must not be quoted"))
		}
		decoded, decodeErr := decodeExtendedFilename(extended.value)
		if decodeErr != nil {
			return nil, invalidFilename(decodeErr)
		}
		filename = &decoded
	} else if fallback, exists := parameters["filename"]; exists {
		filename = &fallback.value
	}
	normalized, err := upload.NormalizeFilename(filename)
	if err != nil {
		return nil, invalidFilename(err)
	}
	return normalized, nil
}

type dispositionParameter struct {
	value  string
	quoted bool
}

func parseDisposition(value string) (string, map[string]dispositionParameter, error) {
	value = strings.TrimSpace(value)
	position := 0
	disposition := consumeToken(value, &position)
	if disposition == "" {
		return "", nil, errors.New("missing disposition")
	}
	parameters := make(map[string]dispositionParameter)
	for {
		consumeWhitespace(value, &position)
		if position == len(value) {
			return disposition, parameters, nil
		}
		if value[position] != ';' {
			return "", nil, errors.New("invalid disposition separator")
		}
		position++
		consumeWhitespace(value, &position)
		name := strings.ToLower(consumeToken(value, &position))
		if name == "" {
			return "", nil, errors.New("missing disposition parameter name")
		}
		consumeWhitespace(value, &position)
		if position == len(value) || value[position] != '=' {
			return "", nil, errors.New("missing disposition parameter value")
		}
		position++
		consumeWhitespace(value, &position)
		parameter := dispositionParameter{}
		if position < len(value) && value[position] == '"' {
			parameter.quoted = true
			position++
			var decoded strings.Builder
			closed := false
			for position < len(value) {
				character := value[position]
				position++
				if character == '"' {
					closed = true
					break
				}
				if character == '\\' {
					if position == len(value) {
						return "", nil, errors.New("unterminated quoted escape")
					}
					next := value[position]
					// Browsers commonly send a Windows fake path with literal,
					// singly escaped backslashes. Preserve those separators while
					// still decoding the two quoted-string escapes we emit/accept.
					if next == '\\' || next == '"' {
						character = next
						position++
					} else {
						decoded.WriteByte('\\')
						character = next
						position++
					}
				}
				if character == '\r' || character == '\n' {
					return "", nil, errors.New("newline in quoted parameter")
				}
				decoded.WriteByte(character)
			}
			if !closed {
				return "", nil, errors.New("unterminated quoted parameter")
			}
			parameter.value = decoded.String()
		} else {
			parameter.value = consumeToken(value, &position)
			if parameter.value == "" {
				return "", nil, errors.New("empty token parameter")
			}
		}
		if _, duplicate := parameters[name]; duplicate {
			return "", nil, errors.New("duplicate disposition parameter")
		}
		parameters[name] = parameter
	}
}

func consumeToken(value string, position *int) string {
	start := *position
	for *position < len(value) && isMIMETokenByte(value[*position]) {
		*position++
	}
	return value[start:*position]
}

func consumeWhitespace(value string, position *int) {
	for *position < len(value) && (value[*position] == ' ' || value[*position] == '\t') {
		*position++
	}
}

func isMIMETokenByte(value byte) bool {
	if value <= ' ' || value >= 0x7f {
		return false
	}
	return !strings.ContainsRune(`()<>@,;:\"/[]?=`, rune(value))
}

func decodeExtendedFilename(value string) (string, error) {
	first := strings.IndexByte(value, '\'')
	if first <= 0 {
		return "", errors.New("filename* is missing charset")
	}
	secondRelative := strings.IndexByte(value[first+1:], '\'')
	if secondRelative < 0 {
		return "", errors.New("filename* is missing language separator")
	}
	second := first + 1 + secondRelative
	if !strings.EqualFold(value[:first], "UTF-8") {
		return "", errors.New("filename* charset must be UTF-8")
	}
	encoded := value[second+1:]
	decoded := make([]byte, 0, len(encoded))
	for position := 0; position < len(encoded); position++ {
		if encoded[position] == '%' {
			if position+2 >= len(encoded) {
				return "", errors.New("filename* contains incomplete percent encoding")
			}
			high, highOK := hexDigit(encoded[position+1])
			low, lowOK := hexDigit(encoded[position+2])
			if !highOK || !lowOK {
				return "", errors.New("filename* contains invalid percent encoding")
			}
			decoded = append(decoded, high<<4|low)
			position += 2
			continue
		}
		if !isRFC8187AttrByte(encoded[position]) {
			return "", fmt.Errorf("filename* contains invalid unescaped byte 0x%02x", encoded[position])
		}
		decoded = append(decoded, encoded[position])
	}
	if !utf8.Valid(decoded) {
		return "", errors.New("filename* is not valid UTF-8")
	}
	return string(decoded), nil
}

func hexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isRFC8187AttrByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' ||
		strings.ContainsRune("!#$&+-.^_`|~", rune(value))
}

var (
	errBodyReadTimeout      = errors.New("request body read timed out")
	errInvalidMultipartBody = errors.New("invalid multipart body")
)

type renewingBody struct {
	io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration
	hard       time.Time
	now        func() time.Time
	deadline   time.Time
}

func (r *renewingBody) Read(value []byte) (int, error) {
	if !r.now().Before(r.deadline) {
		return 0, errBodyReadTimeout
	}
	count, err := r.ReadCloser.Read(value)
	if !r.now().Before(r.deadline) {
		return count, errors.Join(errBodyReadTimeout, err)
	}
	if count > 0 {
		r.renewDeadline()
	}
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return count, errors.Join(errBodyReadTimeout, err)
		}
	}
	return count, err
}

func (r *renewingBody) renewDeadline() {
	r.deadline = r.nextDeadline()
	_ = r.controller.SetReadDeadline(r.deadline)
}

func (r *renewingBody) nextDeadline() time.Time {
	idleDeadline := r.now().Add(r.idle)
	if r.hard.Before(idleDeadline) {
		return r.hard
	}
	return idleDeadline
}

type singlePartReader struct {
	part      *multipart.Part
	multipart *multipart.Reader
	source    io.Reader
	complete  func()
	done      bool
}

func (r *singlePartReader) Read(value []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	count, err := r.part.Read(value)
	if !errors.Is(err, io.EOF) {
		if err != nil && !errors.Is(err, errBodyReadTimeout) {
			var maxBytesError *http.MaxBytesError
			if !errors.As(err, &maxBytesError) {
				err = errors.Join(errInvalidMultipartBody, err)
			}
		}
		return count, err
	}
	next, nextErr := r.multipart.NextRawPart()
	if nextErr == nil {
		_, readErr := io.Copy(io.Discard, next)
		closeErr := next.Close()
		if trailingErr := errors.Join(readErr, closeErr); trailingErr != nil {
			return count, multipartBodyReadError(trailingErr)
		}
		if _, drainErr := io.Copy(io.Discard, r.source); drainErr != nil {
			return count, multipartBodyReadError(drainErr)
		}
		return count, fmt.Errorf("%w: request contains an unexpected trailing part", errInvalidMultipartBody)
	}
	if !errors.Is(nextErr, io.EOF) {
		return count, multipartBodyReadError(nextErr)
	}
	if _, drainErr := io.Copy(io.Discard, r.source); drainErr != nil {
		return count, multipartBodyReadError(drainErr)
	}
	r.done = true
	r.complete()
	return count, io.EOF
}

func multipartBodyReadError(err error) error {
	if errors.Is(err, errBodyReadTimeout) {
		return err
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return err
	}
	return errors.Join(errInvalidMultipartBody, err)
}

func invalidMultipart(message string, cause error) *upload.Failure {
	return &upload.Failure{Status: http.StatusBadRequest, Code: "invalid_multipart", Message: message, Cause: cause}
}

func invalidFilename(cause error) *upload.Failure {
	return &upload.Failure{Status: http.StatusBadRequest, Code: "invalid_filename", Message: "filename is invalid", Cause: cause}
}

func writeUploadFailure(w http.ResponseWriter, requestID string, failure *upload.Failure) {
	writeRawJSON(w, failure.Status, failure.ResponseBody(requestID))
}

func writeRawJSON(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request, requestID string) {
	if h.dependencies.Database == nil || h.dependencies.Migrations == nil || h.dependencies.Storage == nil {
		writeUnavailable(w, requestID)
		return
	}
	if err := h.dependencies.Database.Ping(r.Context()); err != nil {
		writeUnavailable(w, requestID)
		return
	}
	status, err := h.dependencies.Migrations.Status(r.Context())
	if err != nil || !status.Ready() {
		writeUnavailable(w, requestID)
		return
	}
	if err := h.dependencies.Storage.Probe(r.Context()); err != nil {
		writeUnavailable(w, requestID)
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

func writeUnavailable(w http.ResponseWriter, requestID string) {
	writeError(w, http.StatusServiceUnavailable, "unavailable", "service is unavailable", requestID)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, errorResponse{Error: apiError{
		Code:      code,
		Message:   message,
		RequestID: requestID,
		Details:   map[string]any{},
	}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

var fallbackRequestID atomic.Uint64

func requestID(candidate string) string {
	if validRequestID(candidate) {
		return candidate
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}

	var fallback [8]byte
	value := fallbackRequestID.Add(1)
	for i := len(fallback) - 1; i >= 0; i-- {
		fallback[i] = byte(value)
		value >>= 8
	}
	return hex.EncodeToString(fallback[:])
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
