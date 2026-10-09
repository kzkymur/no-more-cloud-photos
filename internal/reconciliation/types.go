package reconciliation

import "time"

type Scope string

const (
	ScopeFiles Scope = "files"
	ScopeDB    Scope = "db"
	ScopeAll   Scope = "all"
)

type Reference struct {
	Kind, SubjectID, MediaID                 string
	OriginalID, JobID, TargetID              *string
	RelativeKey, ExpectedState, SHA256       *string
	SizeBytes                                *int64
	IsCurrent, ProvenanceValid, CurrentValid *bool
	DueAt                                    *time.Time
}

type AttemptOwner struct {
	AttemptID, Kind, Coverage, OriginalID string
	JobID, TempRelativeKey                *string
	EventType                             string
	LeaseExpiresAt, EventOccurredAt       *time.Time
}

type Snapshot struct {
	StartedAt, CutoffAt, EndedAt time.Time
	References                   []Reference
	Attempts                     []AttemptOwner
}

// SnapshotCapture preserves the outcome of each independently readable source
// inside one repeatable-read database snapshot.
type SnapshotCapture struct {
	Snapshot
	DatabaseReferencesError error
	AttemptOwnersError      error
}

type Finding struct {
	ID, Kind, Reason, SubjectType string
	SubjectID, MediaID, JobID     *string
	TargetID, AttemptID           *string
	RelativeKey, ExpectedState    *string
	ExpectedSize                  *int64
	ExpectedSHA                   *string
	ObservedType                  string
	ObservedSize                  *int64
	ObservedSHA                   *string
	ObservedMTime, ObservedCTime  *time.Time
	ObservedAt                    time.Time
}

type Result struct {
	ReportID     string
	Sealed       bool
	FindingCount int64
	StartedAt    time.Time
	EndedAt      time.Time
}

type ReportOutcome struct {
	DatabaseResult, DatabaseError *string
	AttemptResult, AttemptError   *string
	StorageResult, StorageError   *string
	FindingCount                  int64
	Sealed                        bool
	SealedFindingCount            *int64
	ScanEndedAt                   *time.Time
}
