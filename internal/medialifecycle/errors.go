package medialifecycle

import (
	"errors"
	"fmt"
	"net/http"
)

type ErrorCode string

const (
	CodeInvalidID           ErrorCode = "invalid_id"
	CodeMediaNotFound       ErrorCode = "media_not_found"
	CodeMediaNotDeleted     ErrorCode = "media_not_deleted"
	CodePurgeAlreadyStarted ErrorCode = "purge_already_started"
	CodePurgeFailedUseRetry ErrorCode = "purge_failed_use_retry"
	CodeUnavailable         ErrorCode = "unavailable"
	CodeInternalError       ErrorCode = "internal_error"
)

type ErrorKind uint8

const (
	KindInvalidRequest ErrorKind = iota + 1
	KindNotFound
	KindConflict
	KindDatabaseUnavailable
	KindInvariant
)

type SemanticError struct {
	kind    ErrorKind
	status  int
	code    ErrorCode
	message string
	details map[string]any
	cause   error
}

func (e *SemanticError) Error() string {
	if e == nil {
		return "media lifecycle error"
	}
	return fmt.Sprintf("media lifecycle failed with status %d (%s)", e.status, e.code)
}

func (e *SemanticError) Unwrap() error   { return e.cause }
func (e *SemanticError) Kind() ErrorKind { return e.kind }
func (e *SemanticError) HTTPStatus() int { return e.status }
func (e *SemanticError) Code() ErrorCode { return e.code }
func (e *SemanticError) Message() string { return e.message }
func (e *SemanticError) Details() map[string]any {
	result := make(map[string]any, len(e.details))
	for key, value := range e.details {
		result[key] = value
	}
	return result
}

func IsKind(err error, kind ErrorKind) bool {
	var semantic *SemanticError
	return errors.As(err, &semantic) && semantic.kind == kind
}

func newInvalidID() *SemanticError {
	return semanticError(KindInvalidRequest, http.StatusBadRequest, CodeInvalidID, "identifier is invalid", nil, nil)
}

func newMediaNotFound() *SemanticError {
	return semanticError(KindNotFound, http.StatusNotFound, CodeMediaNotFound, "media was not found", nil, nil)
}

func newMediaNotDeleted() *SemanticError {
	return semanticError(KindConflict, http.StatusConflict, CodeMediaNotDeleted, "media is not deleted", nil, nil)
}

func newPurgeAlreadyStarted(jobID string) *SemanticError {
	return semanticError(KindConflict, http.StatusConflict, CodePurgeAlreadyStarted, "media purge has already started", map[string]any{"job_id": jobID}, nil)
}

func newPurgeFailed(jobID string) *SemanticError {
	return semanticError(KindConflict, http.StatusConflict, CodePurgeFailedUseRetry, "failed purge must be retried administratively", map[string]any{"job_id": jobID}, nil)
}

func newUnavailable(cause error) *SemanticError {
	return semanticError(KindDatabaseUnavailable, http.StatusServiceUnavailable, CodeUnavailable, "service is temporarily unavailable", nil, cause)
}

func newInvariant(cause error) *SemanticError {
	return semanticError(KindInvariant, http.StatusInternalServerError, CodeInternalError, "internal server error", nil, cause)
}

func semanticError(kind ErrorKind, status int, code ErrorCode, message string, details map[string]any, cause error) *SemanticError {
	copied := make(map[string]any, len(details))
	for key, value := range details {
		copied[key] = value
	}
	return &SemanticError{kind: kind, status: status, code: code, message: message, details: copied, cause: cause}
}

// CommitRolledBack means PostgreSQL proved that COMMIT did not succeed.
type CommitRolledBack struct{ Cause error }

func (e *CommitRolledBack) Error() string { return "media lifecycle commit was rolled back" }
func (e *CommitRolledBack) Unwrap() error { return e.Cause }
func (e *CommitRolledBack) HTTPStatus() int {
	return http.StatusServiceUnavailable
}

// CommitOutcomeUnknown means the COMMIT response was lost and the mutation may
// or may not be durable. Callers must not reinterpret it as a state conflict.
type CommitOutcomeUnknown struct{ Cause error }

func (e *CommitOutcomeUnknown) Error() string {
	return "media lifecycle commit outcome is unknown"
}
func (e *CommitOutcomeUnknown) Unwrap() error { return e.Cause }
func (e *CommitOutcomeUnknown) HTTPStatus() int {
	return http.StatusServiceUnavailable
}
