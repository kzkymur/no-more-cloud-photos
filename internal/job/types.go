package job

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

var (
	ErrNoWork              = errors.New("no job available")
	ErrLeaseLost           = errors.New("job lease lost")
	ErrNotFound            = errors.New("job or target not found")
	ErrConflict            = errors.New("job or target state conflict")
	ErrInvalid             = errors.New("invalid job repository input")
	ErrDatabaseUnavailable = errors.New("job database unavailable")
	ErrInvariant           = errors.New("job database invariant violated")
	ErrMaintenance         = errors.New("job publication paused by maintenance")
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
	FailureMaintenancePaused  FailureCode = "maintenance_paused"
)

var safeFailureMessages = map[FailureCode]string{
	FailureProcessTimeout:     "processing timed out",
	FailureProcessFailed:      "processing failed",
	FailureProcessOutputLimit: "processing output limit exceeded",
	FailureLeaseExpired:       "job lease expired",
	FailureWorkerShutdown:     "worker shut down before completion",
	FailureMaintenancePaused:  "job publication paused by maintenance",
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

// Original is the immutable input descriptor pinned by a transform Job.
// Lease owns this value; callers cannot mutate repository state through it.
type Original struct {
	ID                      string
	MediaID                 string
	RelativePath            string
	MIMEType                string
	SizeBytes               int64
	SHA256                  string
	Width                   *int
	Height                  *int
	DurationMS              *int64
	PrimaryVideoStreamIndex *int
}

type Lease struct {
	ID             string
	Type           Type
	Original       *Original
	MediaID        string
	Token          string
	Attempts       int
	MaxAttempts    int
	LeaseExpiresAt time.Time
	StartedAt      time.Time
	AvailableAt    time.Time
	CreatedAt      time.Time
	Targets        []Target
	GeneratedBytes int64
}

type Rendition struct {
	ID             string
	JobID          string
	LeaseToken     string
	TargetID       string
	OriginalID     string
	MediaID        string
	ProfileID      string
	RelativePath   string
	MIMEType       string
	Width          *int
	Height         *int
	DurationMS     *int64
	SizeBytes      int64
	SHA256         string
	ProcessorAudit json.RawMessage
}

type Publication struct {
	RenditionID string
	Current     bool
	JobFinished bool
	CreatedAt   time.Time
}

// CommitRolledBack proves PostgreSQL rejected COMMIT. The durable rendition
// file is an orphan and must be retained for reconciliation.
type CommitRolledBack struct{ Cause error }

func (e *CommitRolledBack) Error() string { return "rendition publication commit was rolled back" }
func (e *CommitRolledBack) Unwrap() error { return e.Cause }

// CommitOutcomeUnknown means COMMIT may have succeeded. The durable rendition
// file must never be removed based on this result.
type CommitOutcomeUnknown struct{ Cause error }

func (e *CommitOutcomeUnknown) Error() string {
	return "rendition publication commit outcome is unknown"
}
func (e *CommitOutcomeUnknown) Unwrap() error { return e.Cause }

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
	FileBaseURL   string
	Checkpoint    func(context.Context, storage.Boundary, string) error
}

type classifiedError struct {
	kind error
	err  error
}

func (e *classifiedError) Error() string        { return e.kind.Error() }
func (e *classifiedError) Unwrap() error        { return e.err }
func (e *classifiedError) Is(target error) bool { return target == e.kind }
