package storage

import (
	"errors"
	"fmt"
)

var (
	ErrSymlink          = errors.New("storage symlink refused")
	ErrCollision        = errors.New("storage object collision")
	ErrNoSpace          = errors.New("storage has no space available")
	ErrQuota            = errors.New("storage quota exceeded")
	ErrPermission       = errors.New("storage permission denied")
	ErrReadOnly         = errors.New("storage is read-only")
	ErrValidation       = errors.New("storage validation failed")
	ErrDurability       = errors.New("storage durability operation failed")
	ErrOutcomeUncertain = errors.New("storage outcome is uncertain")
	ErrUnexpectedType   = errors.New("unexpected storage object type")
	ErrUnstableScan     = errors.New("storage scan observed concurrent namespace mutation")
	ErrScanBoundary     = errors.New("storage scan crossed its pinned filesystem boundary")
	ErrScanLimit        = errors.New("storage scan resource limit exceeded")
	ErrClosed           = errors.New("storage is closed")
)

type PublishError struct {
	Published bool
	Uncertain bool
	operation string
	cause     error
}

func (err *PublishError) Error() string {
	return fmt.Sprintf("storage publish %s failed", err.operation)
}

func (err *PublishError) Unwrap() error { return err.cause }

type QuarantineError struct {
	Moved     bool
	Uncertain bool
	operation string
	cause     error
}

func (err *QuarantineError) Error() string {
	return fmt.Sprintf("storage quarantine %s failed", err.operation)
}

func (err *QuarantineError) Unwrap() error { return err.cause }

type RepairCause string

const (
	RepairCausePermission         RepairCause = "permission"
	RepairCauseReadOnly           RepairCause = "read_only"
	RepairCauseCrossDevice        RepairCause = "cross_device"
	RepairCauseNoSpace            RepairCause = "no_space"
	RepairCauseQuota              RepairCause = "quota"
	RepairCauseCollision          RepairCause = "collision"
	RepairCauseWrongType          RepairCause = "wrong_type"
	RepairCauseSourceMissing      RepairCause = "source_missing"
	RepairCauseSourceChanged      RepairCause = "source_changed"
	RepairCauseSourceRestored     RepairCause = "source_restored"
	RepairCauseManifestMismatch   RepairCause = "manifest_mismatch"
	RepairCauseQuarantineMismatch RepairCause = "quarantine_mismatch"
	RepairCauseNamespaceChanged   RepairCause = "namespace_changed"
	RepairCauseDurability         RepairCause = "durability"
	RepairCauseOutcomeUncertain   RepairCause = "outcome_uncertain"
	RepairCauseCancelled          RepairCause = "cancelled"
	RepairCauseInternal           RepairCause = "internal"
)

type RepairStage string

const (
	RepairStagePrepare         RepairStage = "prepare"
	RepairStageHash            RepairStage = "hash"
	RepairStageRevalidate      RepairStage = "revalidate"
	RepairStageRename          RepairStage = "rename"
	RepairStageSourceSync      RepairStage = "source_directory_fsync"
	RepairStageDestinationSync RepairStage = "destination_directory_fsync"
	RepairStageCanonicalVerify RepairStage = "canonical_destination_verify"
)

type RepairError struct {
	Cause    RepairCause
	Stage    RepairStage
	Progress RepairProgress
	Err      error
}

func (err *RepairError) Error() string {
	return fmt.Sprintf("storage repair %s failed: %s", err.Stage, err.Cause)
}

func (err *RepairError) Unwrap() error { return err.Err }
