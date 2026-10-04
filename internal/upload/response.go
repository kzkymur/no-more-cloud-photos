package upload

import (
	"encoding/json"
	"fmt"
	"time"
)

// Outcome is a complete semantic HTTP result. Body is kept raw so an
// idempotency replay cannot discard fields added by a newer writer.
type Outcome struct {
	Status   int
	Body     json.RawMessage
	Replayed bool
}

// Failure is a transient or caller-caused result that must not be persisted as
// an idempotency result. Cause is intentionally excluded from the response.
type Failure struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
	Cause   error
}

func (f *Failure) Error() string {
	if f == nil {
		return "upload failure"
	}
	return fmt.Sprintf("upload failed with status %d (%s)", f.Status, f.Code)
}

func (f *Failure) Unwrap() error   { return f.Cause }
func (f *Failure) HTTPStatus() int { return f.Status }

// ResponseBody builds the public error body with the current request ID.
func (f *Failure) ResponseBody(requestID string) json.RawMessage {
	details := f.Details
	if details == nil {
		details = map[string]any{}
	}
	body, err := json.Marshal(errorResponse{Error: errorDetail{
		Code: f.Code, Message: f.Message, RequestID: requestID, Details: details,
	}})
	if err != nil {
		body, _ = json.Marshal(errorResponse{Error: errorDetail{
			Code: f.Code, Message: f.Message, RequestID: requestID, Details: map[string]any{},
		}})
	}
	return body
}

// OutcomeUnknown means the database COMMIT response was lost or failed. Any
// published final file must remain for retry/reconciliation.
type OutcomeUnknown struct{ Cause error }

func (e *OutcomeUnknown) Error() string   { return "upload commit outcome is unknown" }
func (e *OutcomeUnknown) Unwrap() error   { return e.Cause }
func (e *OutcomeUnknown) HTTPStatus() int { return 503 }
func (e *OutcomeUnknown) ResponseBody(requestID string) json.RawMessage {
	return (&Failure{Status: 503, Code: "unavailable", Message: "service is temporarily unavailable", Cause: e.Cause}).ResponseBody(requestID)
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

type uploadResponse struct {
	Media mediaSummary `json:"media"`
	Job   *jobResponse `json:"job"`
}

type mediaSummary struct {
	ID               string     `json:"id"`
	MIMEType         string     `json:"mime_type"`
	OriginalFilename *string    `json:"original_filename"`
	SizeBytes        int64      `json:"size_bytes"`
	Width            *int       `json:"width"`
	Height           *int       `json:"height"`
	DurationMS       *int64     `json:"duration_ms"`
	TakenAt          *time.Time `json:"taken_at"`
	TakenAtSource    string     `json:"taken_at_source"`
	TakenAtTimezone  *string    `json:"taken_at_timezone"`
	CreatedAt        time.Time  `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at"`
	PurgeAfter       *time.Time `json:"purge_after"`
	Rendition        any        `json:"rendition"`
}

type mediaDetailEvent struct {
	ID                string     `json:"id"`
	MIMEType          string     `json:"mime_type"`
	OriginalFilename  *string    `json:"original_filename"`
	SizeBytes         int64      `json:"size_bytes"`
	Width             *int       `json:"width"`
	Height            *int       `json:"height"`
	DurationMS        *int64     `json:"duration_ms"`
	TakenAt           *time.Time `json:"taken_at"`
	TakenAtSource     string     `json:"taken_at_source"`
	TakenAtTimezone   *string    `json:"taken_at_timezone"`
	CreatedAt         time.Time  `json:"created_at"`
	DeletedAt         *time.Time `json:"deleted_at"`
	PurgeAfter        *time.Time `json:"purge_after"`
	CurrentRenditions []any      `json:"current_renditions"`
}

type jobResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Status       string           `json:"status"`
	MediaID      string           `json:"media_id"`
	OriginalID   *string          `json:"original_id"`
	Attempts     int              `json:"attempts"`
	MaxAttempts  int              `json:"max_attempts"`
	AvailableAt  time.Time        `json:"available_at"`
	StartedAt    *time.Time       `json:"started_at"`
	FinishedAt   *time.Time       `json:"finished_at"`
	Error        any              `json:"error"`
	CancelledAt  *time.Time       `json:"cancelled_at"`
	CancelReason *string          `json:"cancel_reason"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Targets      []targetResponse `json:"targets"`
}

type targetResponse struct {
	ID          string          `json:"id"`
	Profile     profileResponse `json:"profile"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	Error       any             `json:"error"`
	RenditionID *string         `json:"rendition_id"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type profileResponse struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}
