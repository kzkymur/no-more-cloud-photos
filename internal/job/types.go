package job

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNoWork              = errors.New("no job available")
	ErrLeaseLost           = errors.New("job lease lost")
	ErrNotFound            = errors.New("job or target not found")
	ErrConflict            = errors.New("job or target state conflict")
	ErrInvalid             = errors.New("invalid job repository input")
	ErrDatabaseUnavailable = errors.New("job database unavailable")
	ErrInvariant           = errors.New("job database invariant violated")
)

type Type string

const (
	TypeTransform Type = "transform"
	TypePurge     Type = "purge"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type TargetStatus string

const (
	TargetPending   TargetStatus = "pending"
	TargetSucceeded TargetStatus = "succeeded"
	TargetFailed    TargetStatus = "failed"
)

type FailureCode string

const (
	FailureProcessTimeout     FailureCode = "process_timeout"
	FailureProcessFailed      FailureCode = "process_failed"
	FailureProcessOutputLimit FailureCode = "process_output_limit"
	FailureLeaseExpired       FailureCode = "lease_expired"
	FailureWorkerShutdown     FailureCode = "worker_shutdown"
)

var safeFailureMessages = map[FailureCode]string{
	FailureProcessTimeout:     "processing timed out",
	FailureProcessFailed:      "processing failed",
	FailureProcessOutputLimit: "processing output limit exceeded",
	FailureLeaseExpired:       "job lease expired",
	FailureWorkerShutdown:     "worker shut down before completion",
}

type Profile struct {
	ID                      string
	Key                     string
	Version                 int
	Processor               string
	ParametersSchemaVersion int
	InputMIMETypes          []string
	Parameters              json.RawMessage
}

type Target struct {
	ID           string
	Status       TargetStatus
	Attempts     int
	ErrorCode    *FailureCode
	ErrorMessage *string
	Profile      Profile
	UpdatedAt    time.Time
}

type Lease struct {
	ID             string
	Type           Type
	OriginalID     *string
	MediaID        string
	Token          string
	Attempts       int
	MaxAttempts    int
	LeaseExpiresAt time.Time
	StartedAt      time.Time
	AvailableAt    time.Time
	CreatedAt      time.Time
	Targets        []Target
}

type AdminAudit struct {
	ID                 string
	Actor              string
	Host               string
	SanitizedArguments json.RawMessage
}

type Options struct {
	LeaseDuration time.Duration
	ReclaimBatch  int
	Jitter        func(time.Duration) time.Duration
	UUID          func() (string, error)
}

type classifiedError struct {
	kind error
	err  error
}

func (e *classifiedError) Error() string        { return e.kind.Error() }
func (e *classifiedError) Unwrap() error        { return e.err }
func (e *classifiedError) Is(target error) bool { return target == e.kind }
