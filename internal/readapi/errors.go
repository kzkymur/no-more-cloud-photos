package readapi

import (
	"errors"
	"fmt"
	"net/http"
)

type ErrorCode string

const (
	CodeInvalidID         ErrorCode = "invalid_id"
	CodeInvalidRequest    ErrorCode = "invalid_request"
	CodeInvalidProfile    ErrorCode = "invalid_profile"
	CodeMediaNotFound     ErrorCode = "media_not_found"
	CodeOriginalNotFound  ErrorCode = "original_not_found"
	CodeRenditionNotFound ErrorCode = "rendition_not_found"
	CodeJobNotFound       ErrorCode = "job_not_found"
	CodeRenditionNotReady ErrorCode = "rendition_not_ready"
	CodeInternalError     ErrorCode = "internal_error"
	CodeUnavailable       ErrorCode = "unavailable"
)

type ErrorKind uint8

const (
	KindInvalidRequest ErrorKind = iota + 1
	KindNotFound
	KindRenditionNotReady
	KindInvalidProfile
	KindInvariant
	KindDatabaseUnavailable
)

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

// SemanticError contains the public HTTP semantics and a private diagnostic
// cause. The cause is available to trusted logging through errors.Unwrap, but
// cannot be serialized as part of ErrorResponse.
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
		return "read API error"
	}
	return fmt.Sprintf("read API failed with status %d (%s)", e.status, e.code)
}

func (e *SemanticError) Unwrap() error   { return e.cause }
func (e *SemanticError) Kind() ErrorKind { return e.kind }
func (e *SemanticError) HTTPStatus() int { return e.status }
func (e *SemanticError) Code() ErrorCode { return e.code }

func (e *SemanticError) Response(requestID string) ErrorResponse {
	details := cloneDetails(e.details)
	return ErrorResponse{Error: ErrorDetail{
		Code: e.code, Message: e.message, RequestID: requestID, Details: details,
	}}
}

func NewInvalidID() *SemanticError {
	return semanticError(KindInvalidRequest, http.StatusBadRequest, CodeInvalidID, "identifier is invalid", nil, nil)
}

func NewInvalidRequest(fields map[string]string) *SemanticError {
	details := map[string]any{}
	if len(fields) != 0 {
		copied := make(map[string]string, len(fields))
		for field, reason := range fields {
			copied[field] = reason
		}
		details["fields"] = copied
	}
	return semanticError(KindInvalidRequest, http.StatusBadRequest, CodeInvalidRequest, "request is invalid", details, nil)
}

func NewInvalidProfile(profile string) *SemanticError {
	details := map[string]any{}
	if profile != "" {
		details["profile"] = profile
	}
	return semanticError(KindInvalidProfile, http.StatusBadRequest, CodeInvalidProfile, "profile does not exist", details, nil)
}

func NewMediaNotFound() *SemanticError {
	return notFound(CodeMediaNotFound, "media was not found")
}

func NewOriginalNotFound() *SemanticError {
	return notFound(CodeOriginalNotFound, "original was not found")
}

func NewRenditionNotFound() *SemanticError {
	return notFound(CodeRenditionNotFound, "rendition was not found")
}

func NewJobNotFound() *SemanticError {
	return notFound(CodeJobNotFound, "job was not found")
}

func NewRenditionNotReady() *SemanticError {
	return semanticError(KindRenditionNotReady, http.StatusConflict, CodeRenditionNotReady, "rendition is not ready", nil, nil)
}

func NewInvariantError(cause error) *SemanticError {
	return semanticError(KindInvariant, http.StatusInternalServerError, CodeInternalError, "internal server error", nil, cause)
}

func NewDatabaseUnavailableError(cause error) *SemanticError {
	return semanticError(KindDatabaseUnavailable, http.StatusServiceUnavailable, CodeUnavailable, "service is temporarily unavailable", nil, cause)
}

func IsKind(err error, kind ErrorKind) bool {
	var semantic *SemanticError
	return errors.As(err, &semantic) && semantic.kind == kind
}

func notFound(code ErrorCode, message string) *SemanticError {
	return semanticError(KindNotFound, http.StatusNotFound, code, message, nil, nil)
}

func semanticError(kind ErrorKind, status int, code ErrorCode, message string, details map[string]any, cause error) *SemanticError {
	return &SemanticError{kind: kind, status: status, code: code, message: message, details: cloneDetails(details), cause: cause}
}

func cloneDetails(details map[string]any) map[string]any {
	if details == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(details))
	for key, value := range details {
		cloned[key] = value
	}
	return cloned
}
