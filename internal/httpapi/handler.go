// Package httpapi provides the Core HTTP API handler.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

const jsonContentType = "application/json; charset=utf-8"

// DatabasePinger is the database capability required by the readiness probe.
type DatabasePinger interface {
	Ping(context.Context) error
}

// MigrationChecker is the migration capability required by the readiness probe.
type MigrationChecker interface {
	Status(context.Context) (database.Status, error)
}

// Dependencies contains the services checked by the readiness probe.
type Dependencies struct {
	Database    DatabasePinger
	Migrations  MigrationChecker
	StorageRoot string
}

type handler struct {
	dependencies Dependencies
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

// NewHandler creates a handler for the Core service probes.
func NewHandler(dependencies Dependencies) http.Handler {
	return &handler{dependencies: dependencies}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := requestID(r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Request-ID", requestID)

	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, requestID)
			return
		}
		if !acceptsJSON(r.Header.Get("Accept")) {
			writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
			return
		}
		writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
	case "/readyz":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, requestID)
			return
		}
		if !acceptsJSON(r.Header.Get("Accept")) {
			writeError(w, http.StatusNotAcceptable, "not_acceptable", "JSON response is not acceptable", requestID)
			return
		}
		h.ready(w, r, requestID)
	default:
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

func (h *handler) methodNotAllowed(w http.ResponseWriter, requestID string) {
	w.Header().Set("Allow", http.MethodGet)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", requestID)
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request, requestID string) {
	if h.dependencies.Database == nil || h.dependencies.Migrations == nil || h.dependencies.StorageRoot == "" {
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
	if err := checkStorageRoot(h.dependencies.StorageRoot); err != nil {
		writeUnavailable(w, requestID)
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

func checkStorageRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.ErrInvalid
	}

	file, err := os.CreateTemp(root, ".readyz-")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
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
