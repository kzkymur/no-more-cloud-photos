package reconciliation

import (
	"crypto/sha256"
	"errors"
	"time"
)

var (
	ErrRepairInvalid        = errors.New("invalid repair request")
	ErrRepairAdmission      = errors.New("repair report is not admissible")
	ErrRepairConflict       = errors.New("repair replay conflicts")
	ErrRepairMaintenance    = errors.New("repair requires maintenance")
	ErrRepairIncomplete     = errors.New("repair run is incomplete")
	ErrRepairOutcomeUnknown = errors.New("repair outcome is unknown")
)

type RepairSourceType string

const (
	RepairOriginalFinal        RepairSourceType = "original_final"
	RepairRenditionFinal       RepairSourceType = "rendition_final"
	RepairOriginalAttemptTemp  RepairSourceType = "original_attempt_temp"
	RepairRenditionAttemptTemp RepairSourceType = "rendition_attempt_temp"
)

type RepairFinding struct {
	Ordinal                                    int64
	FindingID, SubjectID                       string
	SourceType                                 RepairSourceType
	MediaID, JobID, TargetID, StorageAttemptID *string
	SourceRelativeKey                          string
	ExpectedSize                               int64
	ExpectedSHA256                             [sha256.Size]byte
	ObservedMTime, ObservedCTime               time.Time
}

type RepairManifestItem struct {
	RepairFinding
	RunID, ItemID, QuarantineID, OwnerSnapshot, DestinationRelativeKey string
	EffectiveOutcome, ActiveAttemptID                                  *string
	LatestAttemptNumber                                                *int
}

type RepairRun struct {
	RunID, ReportID, AuthorizationID, Actor string
	StartedAt                               time.Time
	PlanID, PlanDigest, Result              *string
	PlanVersion                             *int16
	PlanItemCount                           *int64
	Quarantined, Stale, Resolved            *int64
	Failed, Unknown                         *int64
	CompletedAt                             *time.Time
}

type RepairOwner struct {
	AttemptID, Kind, Coverage, EventType *string
	LeaseExpiresAt, EventOccurredAt      *time.Time
}

type RepairAuthority struct {
	Decision, Reason, Destination string
	Finding                       RepairFinding
	Owners                        []RepairOwner
}

type RepairHistoryEvent struct {
	AttemptNumber          int
	AttemptID              string
	StartedAt              time.Time
	Sequence               int
	EventID, EventType     string
	OutcomeCode            string
	ErrorCode              *string
	ObservedSize           *int64
	ObservedSHA            *string
	OccurredAt             time.Time
	PhysicalEffectPossible bool
}

type RepairPlanRequest struct {
	ReportID, AuthorizationID, Actor string
}

type RepairPlan struct {
	Run      RepairRun
	Items    []RepairManifestItem
	Replayed bool
}

type RepairPreviewItem struct {
	FindingID, SourceKey, PreviewDestination, Decision, Reason string
}

type RepairPreview struct {
	ReportID string
	Items    []RepairPreviewItem
}

type RepairExecutionResult struct {
	RunID                                  string
	Result                                 string
	Manifest, Quarantined, Stale, Resolved int64
	Failed, Unknown                        int64
}

type RepairEvent struct {
	ID, AttemptID, Type, Outcome string
	ErrorCode                    *string
	ObservedSize                 *int64
	ObservedSHA                  *string
}
