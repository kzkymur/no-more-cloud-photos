//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const repairResolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV

type RepairExpectation struct {
	Size   int64
	SHA256 [sha256.Size]byte
	MTime  time.Time
	CTime  time.Time
}

type RepairDisposition string

const (
	RepairSourceReady      RepairDisposition = "source_ready"
	RepairDestinationExact RepairDisposition = "destination_exact"
	RepairBothAbsent       RepairDisposition = "both_absent"
)

type RepairProgress struct {
	Disposition                  RepairDisposition
	SourcePresent                bool
	DestinationPresent           bool
	Revalidated                  bool
	RenameObserved               bool
	RenamePerformedByThisCall    bool
	SourceDirectoryDurable       bool
	DestinationDirectoryDurable  bool
	CanonicalDestinationVerified bool
	ObservedSize                 int64
	ObservedSHA256               [sha256.Size]byte
}

type repairNamespaceIdentity struct {
	DeviceMajor uint32
	DeviceMinor uint32
	Inode       uint64
	MountID     uint64
	Mode        uint16
	UID         uint32
	GID         uint32
	Links       uint32
	Size        uint64
	MTimeSec    int64
	MTimeNsec   uint32
	CTimeSec    int64
	CTimeNsec   uint32
}

type repairChainEntry struct {
	path     string
	identity repairNamespaceIdentity
}

type RepairQuarantine struct {
	store           *Store
	rootFD          int
	sourceParentFD  int
	quarantineFD    int
	sourceKey       string
	sourceParent    string
	sourceName      string
	destinationPath string
	destinationName string
	expectation     RepairExpectation
	sourceChain     []repairChainEntry
	quarantineID    repairNamespaceIdentity
	sourceIdentity  repairNamespaceIdentity
	progress        RepairProgress
	recovery        bool
	closed          bool
}

func (store *Store) InspectRepairOriginal(ctx context.Context, source OriginalKey, destination QuarantineKey, expectation RepairExpectation) (RepairProgress, error) {
	if _, err := ParseOriginalKey(source.String()); err != nil {
		return RepairProgress{}, ErrInvalidKey
	}
	return store.inspectRepair(ctx, source.String(), destination, expectation)
}

func (store *Store) InspectRepairRendition(ctx context.Context, source RenditionKey, destination QuarantineKey, expectation RepairExpectation) (RepairProgress, error) {
	if _, err := ParseRenditionKey(source.String()); err != nil {
		return RepairProgress{}, ErrInvalidKey
	}
	return store.inspectRepair(ctx, source.String(), destination, expectation)
}

func (store *Store) InspectRepairAttemptTemp(ctx context.Context, source AttemptTempKey, destination QuarantineKey, expectation RepairExpectation) (RepairProgress, error) {
	if _, err := ParseAttemptTempKey(source.String()); err != nil {
		return RepairProgress{}, ErrInvalidKey
	}
	return store.inspectRepair(ctx, source.String(), destination, expectation)
}

func (store *Store) BeginRepairOriginal(ctx context.Context, source OriginalKey, destination QuarantineKey, expectation RepairExpectation) (*RepairQuarantine, RepairProgress, error) {
	if _, err := ParseOriginalKey(source.String()); err != nil {
		return nil, RepairProgress{}, ErrInvalidKey
	}
	return store.beginRepair(ctx, source.String(), destination, expectation, false)
}

func (store *Store) BeginRepairRendition(ctx context.Context, source RenditionKey, destination QuarantineKey, expectation RepairExpectation) (*RepairQuarantine, RepairProgress, error) {
	if _, err := ParseRenditionKey(source.String()); err != nil {
		return nil, RepairProgress{}, ErrInvalidKey
	}
	return store.beginRepair(ctx, source.String(), destination, expectation, false)
}

func (store *Store) BeginRepairAttemptTemp(ctx context.Context, source AttemptTempKey, destination QuarantineKey, expectation RepairExpectation) (*RepairQuarantine, RepairProgress, error) {
	if _, err := ParseAttemptTempKey(source.String()); err != nil {
		return nil, RepairProgress{}, ErrInvalidKey
	}
	return store.beginRepair(ctx, source.String(), destination, expectation, false)
}

func (store *Store) inspectRepair(ctx context.Context, source string, destination QuarantineKey, expectation RepairExpectation) (RepairProgress, error) {
	operation, progress, err := store.beginRepair(ctx, source, destination, expectation, true)
	if operation != nil {
		_ = operation.closeDescriptors()
	}
	return progress, err
}

func (store *Store) beginRepair(ctx context.Context, source string, destination QuarantineKey, expectation RepairExpectation, dryRun bool) (*RepairQuarantine, RepairProgress, error) {
	progress := RepairProgress{}
	if ctx == nil || expectation.Size < 0 || expectation.MTime.IsZero() || expectation.CTime.IsZero() {
		return nil, progress, repairFailure(RepairCauseManifestMismatch, RepairStagePrepare, progress, ErrValidation)
	}
	destinationPath := destination.String()
	if _, err := ParseQuarantineKey(destinationPath); err != nil {
		return nil, progress, ErrInvalidKey
	}
	if err := store.inject(ctx, BoundaryRepairCanonicalRoot, Before, source, 0); err != nil {
		return nil, progress, repairContextFailure(RepairStagePrepare, progress, err)
	}
	rootFD, err := store.duplicateRepairRoot()
	if err != nil {
		return nil, progress, repairFailure(RepairCauseInternal, RepairStagePrepare, progress, err)
	}
	operation := &RepairQuarantine{store: store, rootFD: rootFD, sourceParentFD: -1, quarantineFD: -1,
		sourceKey: source, destinationPath: destinationPath, expectation: expectation}
	fail := func(cause RepairCause, stage RepairStage, causeErr error) (*RepairQuarantine, RepairProgress, error) {
		_ = operation.closeDescriptors()
		return nil, operation.progress, repairFailure(cause, stage, operation.progress, causeErr)
	}
	if err := operation.verifyCanonicalRoot(ctx); err != nil {
		return fail(RepairCauseNamespaceChanged, RepairStagePrepare, err)
	}
	directories, sourceName := splitKey(source)
	operation.sourceParent = strings.Join(directories, "/")
	operation.sourceName = sourceName
	_, operation.destinationName = splitKey(destinationPath)
	parentFD, chain, err := operation.openCanonicalChain(ctx, operation.sourceParent)
	if err != nil {
		return fail(repairCauseForError(err, RepairCauseNamespaceChanged), RepairStagePrepare, err)
	}
	operation.sourceParentFD = parentFD
	operation.sourceChain = chain

	var sourceValidationErr error
	sourceFD := -1
	sourceErr := operation.store.inject(ctx, BoundaryRepairSourceOpen, Before, operation.sourceKey, 0)
	if sourceErr == nil {
		sourceFD, sourceErr = repairOpenat2(parentFD, sourceName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_NOATIME|unix.O_CLOEXEC)
	}
	if sourceErr == nil {
		operation.progress.SourcePresent = true
		defer unix.Close(sourceFD)
		identity, digest, validateErr := operation.validateFile(ctx, parentFD, sourceName, sourceFD, expectation, BoundaryRepairSourceHash, RepairStageHash, RepairCauseManifestMismatch, true)
		if validateErr != nil {
			sourceValidationErr = validateErr
		} else {
			operation.sourceIdentity = identity
			operation.progress.ObservedSize = expectation.Size
			operation.progress.ObservedSHA256 = digest
		}
	} else if !errors.Is(sourceErr, unix.ENOENT) {
		operation.progress.SourcePresent = true
		sourceValidationErr = repairFailure(repairInitialSourceOpenCause(sourceErr), RepairStagePrepare, operation.progress, sourceErr)
	}

	quarantineFD, quarantineExists, quarantineErr := operation.openCanonicalQuarantine(ctx, !dryRun && sourceErr == nil && sourceValidationErr == nil)
	if quarantineErr != nil {
		return fail(repairCauseForError(quarantineErr, RepairCauseNamespaceChanged), RepairStagePrepare, quarantineErr)
	}
	operation.quarantineFD = quarantineFD
	if quarantineExists {
		identity, identityErr := repairIdentityAt(quarantineFD, "", unix.AT_EMPTY_PATH)
		if identityErr != nil {
			return fail(RepairCauseNamespaceChanged, RepairStagePrepare, identityErr)
		}
		operation.quarantineID = identity
	}
	destinationExists := false
	if quarantineExists {
		destinationFD, destinationErr := repairOpenat2(quarantineFD, operation.destinationName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_NOATIME|unix.O_CLOEXEC)
		if destinationErr == nil {
			destinationExists = true
			operation.progress.DestinationPresent = true
			identity, digest, validateErr := operation.validateFile(ctx, quarantineFD, operation.destinationName, destinationFD, expectation, BoundaryRepairDestinationHash, RepairStagePrepare, RepairCauseQuarantineMismatch, false)
			_ = unix.Close(destinationFD)
			if validateErr != nil {
				return fail(repairErrorCause(validateErr), RepairStagePrepare, validateErr)
			}
			_ = identity
			operation.progress.ObservedSize = expectation.Size
			operation.progress.ObservedSHA256 = digest
		} else if !errors.Is(destinationErr, unix.ENOENT) {
			return fail(RepairCauseQuarantineMismatch, RepairStagePrepare, destinationErr)
		}
	}
	if sourceValidationErr != nil {
		stage := RepairStageHash
		if sourceErr != nil {
			stage = RepairStagePrepare
		}
		return fail(repairErrorCause(sourceValidationErr), stage, sourceValidationErr)
	}

	sourceExists := sourceErr == nil
	switch {
	case sourceExists && destinationExists:
		return fail(RepairCauseCollision, RepairStagePrepare, ErrCollision)
	case sourceExists:
		operation.progress.Disposition = RepairSourceReady
	case destinationExists:
		operation.progress.Disposition = RepairDestinationExact
		operation.progress.RenameObserved = true
		operation.recovery = true
	default:
		operation.progress.Disposition = RepairBothAbsent
	}
	if err := store.inject(ctx, BoundaryRepairCanonicalRoot, After, source, 0); err != nil {
		return fail(RepairCauseNamespaceChanged, RepairStagePrepare, err)
	}
	return operation, operation.progress, nil
}

func (operation *RepairQuarantine) Progress() RepairProgress { return operation.progress }

func (operation *RepairQuarantine) Revalidate(ctx context.Context) (RepairProgress, error) {
	if err := operation.usable(); err != nil {
		return operation.progress, err
	}
	if err := operation.store.inject(ctx, BoundaryRepairRevalidate, Before, operation.sourceKey, 0); err != nil {
		return operation.progress, repairContextFailure(RepairStageRevalidate, operation.progress, err)
	}
	if err := operation.verifyCanonicalState(ctx); err != nil {
		return operation.progress, repairFailure(RepairCauseNamespaceChanged, RepairStageRevalidate, operation.progress, err)
	}
	if operation.recovery {
		if err := operation.verifyExactDestination(ctx); err != nil {
			return operation.progress, err
		}
		if err := operation.requireSourceAbsent(); err != nil {
			return operation.progress, repairFailure(RepairCauseSourceRestored, RepairStageRevalidate, operation.progress, err)
		}
	} else if operation.progress.Disposition == RepairSourceReady {
		if err := operation.store.inject(ctx, BoundaryRepairSourceOpen, Before, operation.sourceKey, 0); err != nil {
			return operation.progress, repairFailure(repairSourceObservationCause(err), RepairStageRevalidate, operation.progress, err)
		}
		fd, err := repairOpenat2(operation.sourceParentFD, operation.sourceName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC)
		if err != nil {
			return operation.progress, repairFailure(repairSourceObservationCause(err), RepairStageRevalidate, operation.progress, err)
		}
		identity, digest, err := operation.validateFile(ctx, operation.sourceParentFD, operation.sourceName, fd, operation.expectation, BoundaryRepairSourceHash, RepairStageRevalidate, RepairCauseSourceChanged, true)
		_ = unix.Close(fd)
		if err != nil {
			return operation.progress, repairFailure(repairErrorCause(err), RepairStageRevalidate, operation.progress, err)
		}
		if identity != operation.sourceIdentity {
			return operation.progress, repairFailure(RepairCauseSourceChanged, RepairStageRevalidate, operation.progress, ErrValidation)
		}
		operation.progress.ObservedSHA256 = digest
	} else {
		return operation.progress, repairFailure(RepairCauseSourceMissing, RepairStageRevalidate, operation.progress, os.ErrNotExist)
	}
	operation.progress.Revalidated = true
	if err := operation.store.inject(ctx, BoundaryRepairRevalidate, After, operation.sourceKey, 0); err != nil {
		return operation.progress, repairContextFailure(RepairStageRevalidate, operation.progress, err)
	}
	return operation.progress, nil
}

func (operation *RepairQuarantine) Rename(ctx context.Context) (RepairProgress, error) {
	if err := operation.usable(); err != nil {
		return operation.progress, err
	}
	if !operation.progress.Revalidated {
		return operation.progress, repairFailure(RepairCauseInternal, RepairStageRename, operation.progress, ErrValidation)
	}
	if operation.recovery {
		operation.progress.RenameObserved = true
		return operation.progress, nil
	}
	if err := operation.store.inject(ctx, BoundaryRepairRename, Before, operation.destinationPath, 0); err != nil {
		return operation.progress, repairContextFailure(RepairStageRename, operation.progress, err)
	}
	if err := operation.verifyCanonicalState(ctx); err != nil {
		return operation.progress, repairFailure(RepairCauseNamespaceChanged, RepairStageRename, operation.progress, err)
	}
	// Hash and namespace validation is deliberately repeated immediately before
	// the only mutation syscall.
	if err := operation.store.inject(ctx, BoundaryRepairSourceOpen, Before, operation.sourceKey, 0); err != nil {
		return operation.progress, repairFailure(repairSourceObservationCause(err), RepairStageRename, operation.progress, err)
	}
	fd, err := repairOpenat2(operation.sourceParentFD, operation.sourceName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	if err != nil {
		return operation.progress, repairFailure(repairSourceObservationCause(err), RepairStageRename, operation.progress, err)
	}
	identity, _, validateErr := operation.validateFile(ctx, operation.sourceParentFD, operation.sourceName, fd, operation.expectation, BoundaryRepairSourceHash, RepairStageRename, RepairCauseSourceChanged, true)
	_ = unix.Close(fd)
	if validateErr != nil {
		return operation.progress, repairFailure(repairErrorCause(validateErr), RepairStageRename, operation.progress, validateErr)
	}
	if identity != operation.sourceIdentity {
		return operation.progress, repairFailure(RepairCauseSourceChanged, RepairStageRename, operation.progress, ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return operation.progress, repairContextFailure(RepairStageRename, operation.progress, err)
	}
	if err := operation.store.inject(ctx, BoundaryRepairRenameCommit, Before, operation.destinationPath, 0); err != nil {
		return operation.progress, repairContextFailure(RepairStageRename, operation.progress, err)
	}
	if err := operation.store.ops.renameat2(operation.sourceParentFD, operation.sourceName, operation.quarantineFD, operation.destinationName, unix.RENAME_NOREPLACE); err != nil {
		return operation.progress, repairFailure(repairCauseForError(err, RepairCauseCollision), RepairStageRename, operation.progress, classifyError("repair rename", err))
	}
	operation.progress.RenameObserved = true
	operation.progress.RenamePerformedByThisCall = true
	movedIdentity, identityErr := repairIdentityAt(operation.quarantineFD, operation.destinationName, unix.AT_SYMLINK_NOFOLLOW)
	if identityErr != nil || !sameRepairMovedFile(movedIdentity, operation.sourceIdentity) {
		// renameat2 cannot predicate on the inode validated immediately before
		// the syscall. If a same-owner writer replaced the leaf in that last
		// window, put the unexpected inode back without replacement and make the
		// rollback durable before reporting a definite source change.
		rollbackErr := operation.store.ops.renameat2(operation.quarantineFD, operation.destinationName, operation.sourceParentFD, operation.sourceName, unix.RENAME_NOREPLACE)
		if rollbackErr == nil {
			sourceSyncErr := operation.store.ops.fsync(operation.sourceParentFD)
			destinationSyncErr := operation.store.ops.fsync(operation.quarantineFD)
			if sourceSyncErr == nil && destinationSyncErr == nil {
				operation.progress.RenameObserved = false
				operation.progress.RenamePerformedByThisCall = false
				operation.progress.DestinationPresent = false
				operation.progress.SourcePresent = true
				return operation.progress, repairFailure(RepairCauseSourceChanged, RepairStageRename, operation.progress, errors.Join(ErrValidation, identityErr))
			}
			rollbackErr = errors.Join(sourceSyncErr, destinationSyncErr)
		}
		return operation.progress, repairFailure(RepairCauseOutcomeUncertain, RepairStageRename, operation.progress, errors.Join(ErrOutcomeUncertain, identityErr, rollbackErr))
	}
	if _, _, err := operation.normalizeDestinationMetadata(ctx); err != nil {
		return operation.progress, repairFailure(repairErrorCause(err), RepairStageRename, operation.progress, err)
	}
	if err := operation.store.inject(ctx, BoundaryRepairRename, After, operation.destinationPath, 0); err != nil {
		return operation.progress, repairFailure(RepairCauseOutcomeUncertain, RepairStageRename, operation.progress, errors.Join(ErrOutcomeUncertain, err))
	}
	return operation.progress, nil
}

func (operation *RepairQuarantine) SyncSourceDirectory(ctx context.Context) (RepairProgress, error) {
	return operation.syncDirectory(ctx, operation.sourceParentFD, BoundaryRepairSourceDirectorySync, RepairStageSourceSync, true)
}

func (operation *RepairQuarantine) SyncDestinationDirectory(ctx context.Context) (RepairProgress, error) {
	return operation.syncDirectory(ctx, operation.quarantineFD, BoundaryRepairDestinationDirectorySync, RepairStageDestinationSync, false)
}

func (operation *RepairQuarantine) syncDirectory(ctx context.Context, fd int, boundary Boundary, stage RepairStage, source bool) (RepairProgress, error) {
	if err := operation.usable(); err != nil {
		return operation.progress, err
	}
	if !operation.progress.RenameObserved {
		return operation.progress, repairFailure(RepairCauseInternal, stage, operation.progress, ErrValidation)
	}
	if err := operation.store.inject(ctx, boundary, Before, operation.destinationPath, 0); err != nil {
		return operation.progress, repairContextFailure(stage, operation.progress, err)
	}
	if err := operation.verifyCanonicalRoot(ctx); err != nil {
		return operation.progress, repairFailure(RepairCauseNamespaceChanged, stage, operation.progress, err)
	}
	if err := operation.store.ops.fsync(fd); err != nil {
		return operation.progress, repairFailure(RepairCauseDurability, stage, operation.progress, errors.Join(ErrDurability, classifyError("repair directory sync", err)))
	}
	if source {
		operation.progress.SourceDirectoryDurable = true
	} else {
		operation.progress.DestinationDirectoryDurable = true
	}
	if err := operation.store.inject(ctx, boundary, After, operation.destinationPath, 0); err != nil {
		return operation.progress, repairFailure(RepairCauseDurability, stage, operation.progress, err)
	}
	return operation.progress, nil
}

func (operation *RepairQuarantine) VerifyCanonicalDestination(ctx context.Context) (RepairProgress, error) {
	if err := operation.usable(); err != nil {
		return operation.progress, err
	}
	if !operation.progress.RenameObserved {
		return operation.progress, repairFailure(RepairCauseInternal, RepairStageCanonicalVerify, operation.progress, ErrValidation)
	}
	if err := operation.store.inject(ctx, BoundaryRepairCanonicalVerify, Before, operation.destinationPath, 0); err != nil {
		return operation.progress, repairContextFailure(RepairStageCanonicalVerify, operation.progress, err)
	}
	if err := operation.verifyCanonicalState(ctx); err != nil {
		return operation.progress, repairFailure(RepairCauseNamespaceChanged, RepairStageCanonicalVerify, operation.progress, err)
	}
	if err := operation.verifyExactDestination(ctx); err != nil {
		return operation.progress, err
	}
	if err := operation.requireSourceAbsent(); err != nil {
		return operation.progress, repairFailure(RepairCauseSourceRestored, RepairStageCanonicalVerify, operation.progress, err)
	}
	operation.progress.CanonicalDestinationVerified = true
	if err := operation.store.inject(ctx, BoundaryRepairCanonicalVerify, After, operation.destinationPath, 0); err != nil {
		return operation.progress, repairFailure(RepairCauseOutcomeUncertain, RepairStageCanonicalVerify, operation.progress, err)
	}
	return operation.progress, nil
}

func (operation *RepairQuarantine) Close() error {
	if operation == nil || operation.closed {
		return nil
	}
	err := operation.closeDescriptors()
	if operation.progress.RenameObserved && (!operation.progress.SourceDirectoryDurable || !operation.progress.DestinationDirectoryDurable || !operation.progress.CanonicalDestinationVerified) {
		return errors.Join(err, repairFailure(RepairCauseOutcomeUncertain, RepairStageCanonicalVerify, operation.progress, ErrOutcomeUncertain))
	}
	return err
}

func (operation *RepairQuarantine) closeDescriptors() error {
	if operation == nil || operation.closed {
		return nil
	}
	operation.closed = true
	var result error
	for _, fd := range []int{operation.sourceParentFD, operation.quarantineFD, operation.rootFD} {
		if fd >= 0 {
			result = errors.Join(result, unix.Close(fd))
		}
	}
	return result
}

func (operation *RepairQuarantine) usable() error {
	if operation == nil || operation.closed {
		return ErrClosed
	}
	return nil
}

func (store *Store) duplicateRepairRoot() (int, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return -1, ErrClosed
	}
	fd, err := unix.Dup(store.rootFD)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	return fd, err
}

func (operation *RepairQuarantine) verifyCanonicalRoot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := openRootWithoutSymlinks(operation.store.rootPath)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	identity, err := repairIdentityAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return err
	}
	if !sameRepairNamespace(identity, operation.store.rootIdentity) {
		return ErrValidation
	}
	if !safeRepairDirectory(identity, operation.store.rootIdentity) {
		return ErrValidation
	}
	if identity.UID != uint32(os.Geteuid()) || identity.GID != uint32(os.Getegid()) {
		return ErrValidation
	}
	return nil
}

func (operation *RepairQuarantine) openCanonicalChain(ctx context.Context, path string) (int, []repairChainEntry, error) {
	current, err := unix.Dup(operation.rootFD)
	if err != nil {
		return -1, nil, err
	}
	unix.CloseOnExec(current)
	entries := make([]repairChainEntry, 0)
	currentPath := ""
	for depth, component := range strings.Split(path, "/") {
		if component == "" {
			continue
		}
		if err := operation.store.inject(ctx, BoundaryRepairCanonicalAncestor, Before, operation.sourceKey, depth); err != nil {
			_ = unix.Close(current)
			return -1, nil, err
		}
		next, openErr := repairOpenat2(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
		_ = unix.Close(current)
		if openErr != nil {
			return -1, nil, openErr
		}
		current = next
		identity, identityErr := repairIdentityAt(current, "", unix.AT_EMPTY_PATH)
		if identityErr != nil {
			_ = unix.Close(current)
			return -1, nil, identityErr
		}
		if identity.MountID != operation.store.rootIdentity.MountID {
			_ = unix.Close(current)
			return -1, nil, unix.EXDEV
		}
		if !safeRepairDirectory(identity, operation.store.rootIdentity) {
			_ = unix.Close(current)
			return -1, nil, ErrValidation
		}
		if currentPath == "" {
			currentPath = component
		} else {
			currentPath += "/" + component
		}
		entries = append(entries, repairChainEntry{path: currentPath, identity: identity})
		if err := operation.store.inject(ctx, BoundaryRepairCanonicalAncestor, After, operation.sourceKey, depth); err != nil {
			_ = unix.Close(current)
			return -1, nil, err
		}
	}
	return current, entries, nil
}

func (operation *RepairQuarantine) verifyCanonicalState(ctx context.Context) error {
	if err := operation.verifyCanonicalRoot(ctx); err != nil {
		return err
	}
	fd, entries, err := operation.openCanonicalChain(ctx, operation.sourceParent)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if len(entries) != len(operation.sourceChain) {
		return ErrValidation
	}
	for index := range entries {
		if entries[index].path != operation.sourceChain[index].path ||
			!sameRepairNamespace(entries[index].identity, operation.sourceChain[index].identity) {
			return ErrValidation
		}
	}
	if operation.quarantineFD >= 0 {
		fd, err := repairOpenat2(operation.rootFD, ".quarantine", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
		if err != nil {
			return err
		}
		identity, identityErr := repairIdentityAt(fd, "", unix.AT_EMPTY_PATH)
		_ = unix.Close(fd)
		if identityErr != nil || !sameRepairNamespace(identity, operation.quarantineID) {
			return ErrValidation
		}
	}
	return nil
}

func (operation *RepairQuarantine) openCanonicalQuarantine(ctx context.Context, create bool) (int, bool, error) {
	if err := operation.store.inject(ctx, BoundaryRepairCanonicalQuarantine, Before, operation.destinationPath, 0); err != nil {
		return -1, false, err
	}
	fd, err := repairOpenat2(operation.rootFD, ".quarantine", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	if errors.Is(err, unix.ENOENT) && create {
		createdFD, createErr := operation.store.openQuarantineDirectory(ctx, operation.destinationPath)
		if createErr != nil {
			return -1, false, createErr
		}
		_ = unix.Close(createdFD)
		fd, err = repairOpenat2(operation.rootFD, ".quarantine", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	}
	if errors.Is(err, unix.ENOENT) {
		return -1, false, nil
	}
	if err != nil {
		return -1, false, err
	}
	identity, err := repairIdentityAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	if identity.MountID != operation.store.rootIdentity.MountID || identity.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, false, unix.EXDEV
	}
	if identity.Mode&0o077 != 0 || identity.UID != operation.store.rootIdentity.UID || identity.GID != operation.store.rootIdentity.GID {
		_ = unix.Close(fd)
		return -1, false, ErrValidation
	}
	if err := operation.store.inject(ctx, BoundaryRepairCanonicalQuarantine, After, operation.destinationPath, 0); err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	return fd, true, nil
}

func (operation *RepairQuarantine) validateFile(ctx context.Context, parentFD int, name string, fd int, expectation RepairExpectation, boundary Boundary, stage RepairStage, mismatch RepairCause, compareReportTime bool) (repairNamespaceIdentity, [sha256.Size]byte, error) {
	identity, err := repairIdentityAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return identity, [sha256.Size]byte{}, err
	}
	if identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Links != 1 {
		return identity, [sha256.Size]byte{}, repairFailure(RepairCauseWrongType, stage, operation.progress, ErrUnexpectedType)
	}
	if identity.UID != operation.store.rootIdentity.UID || identity.GID != operation.store.rootIdentity.GID {
		return identity, [sha256.Size]byte{}, repairFailure(RepairCausePermission, stage, operation.progress, ErrPermission)
	}
	if identity.MountID != operation.store.rootIdentity.MountID || identity.Size != uint64(expectation.Size) {
		return identity, [sha256.Size]byte{}, repairFailure(mismatch, stage, operation.progress, ErrValidation)
	}
	if compareReportTime && (!repairIdentityTime(identity.MTimeSec, identity.MTimeNsec).Equal(expectation.MTime.UTC().Truncate(time.Microsecond)) ||
		!repairIdentityTime(identity.CTimeSec, identity.CTimeNsec).Equal(expectation.CTime.UTC().Truncate(time.Microsecond))) {
		return identity, [sha256.Size]byte{}, repairFailure(RepairCauseSourceChanged, stage, operation.progress, ErrValidation)
	}
	if err := operation.store.inject(ctx, boundary, Before, operation.sourceKey, 0); err != nil {
		return identity, [sha256.Size]byte{}, repairFailure(repairCauseForError(err, RepairCauseInternal), stage, operation.progress, err)
	}
	digest, err := repairHashFD(ctx, fd)
	if err != nil {
		return identity, digest, repairFailure(repairCauseForError(err, RepairCauseInternal), stage, operation.progress, err)
	}
	if err := operation.store.inject(ctx, boundary, After, operation.sourceKey, 0); err != nil {
		return identity, digest, repairFailure(repairCauseForError(err, RepairCauseInternal), stage, operation.progress, err)
	}
	if boundary == BoundaryRepairSourceHash {
		if injectedErr := operation.store.inject(ctx, BoundaryRepairSourceDescriptorStat, Before, operation.sourceKey, 0); injectedErr != nil {
			return identity, digest, repairFailure(repairCauseForError(injectedErr, RepairCauseInternal), stage, operation.progress, injectedErr)
		}
	}
	after, err := repairIdentityAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return identity, digest, repairFailure(repairCauseForError(err, RepairCauseInternal), stage, operation.progress, err)
	}
	if boundary == BoundaryRepairSourceHash {
		if injectedErr := operation.store.inject(ctx, BoundaryRepairSourceLeafStat, Before, operation.sourceKey, 0); injectedErr != nil {
			return identity, digest, repairFailure(repairSourceLeafCause(injectedErr), stage, operation.progress, injectedErr)
		}
	}
	leaf, err := repairIdentityAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return identity, digest, repairFailure(repairSourceLeafCause(err), stage, operation.progress, err)
	}
	if identity != after || identity != leaf {
		return identity, digest, repairFailure(RepairCauseSourceChanged, stage, operation.progress, ErrValidation)
	}
	if digest != expectation.SHA256 {
		return identity, digest, repairFailure(mismatch, stage, operation.progress, ErrValidation)
	}
	return identity, digest, nil
}

func (operation *RepairQuarantine) verifyExactDestination(ctx context.Context) error {
	identity, digest, err := operation.normalizeDestinationMetadata(ctx)
	if err != nil {
		return err
	}
	if operation.progress.RenamePerformedByThisCall && !sameRepairFileObject(identity, operation.sourceIdentity) {
		return repairFailure(RepairCauseNamespaceChanged, RepairStageCanonicalVerify, operation.progress, ErrValidation)
	}
	operation.progress.ObservedSize = operation.expectation.Size
	operation.progress.ObservedSHA256 = digest
	return nil
}

func (operation *RepairQuarantine) normalizeDestinationMetadata(ctx context.Context) (repairNamespaceIdentity, [sha256.Size]byte, error) {
	fd, err := repairOpenat2(operation.quarantineFD, operation.destinationName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_NOATIME|unix.O_CLOEXEC)
	if err != nil {
		return repairNamespaceIdentity{}, [sha256.Size]byte{}, repairFailure(repairCauseForError(err, RepairCauseQuarantineMismatch), RepairStageCanonicalVerify, operation.progress, err)
	}
	defer unix.Close(fd)
	identity, digest, err := operation.validateFile(ctx, operation.quarantineFD, operation.destinationName, fd, operation.expectation, BoundaryRepairDestinationHash, RepairStageCanonicalVerify, RepairCauseQuarantineMismatch, false)
	if err != nil {
		return identity, digest, err
	}
	if operation.progress.RenamePerformedByThisCall && !sameRepairFileObject(identity, operation.sourceIdentity) {
		return identity, digest, repairFailure(RepairCauseNamespaceChanged, RepairStageCanonicalVerify, operation.progress, ErrValidation)
	}
	if err := operation.store.inject(ctx, BoundaryRepairDestinationMetadataSync, Before, operation.destinationPath, 0); err != nil {
		return identity, digest, repairContextFailure(RepairStageCanonicalVerify, operation.progress, err)
	}
	if identity.Mode&0o7777 != 0o600 {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return identity, digest, repairFailure(repairCauseForError(err, RepairCauseInternal), RepairStageCanonicalVerify, operation.progress, err)
		}
	}
	if err := unix.Fsync(fd); err != nil {
		return identity, digest, repairFailure(repairCauseForError(err, RepairCauseDurability), RepairStageCanonicalVerify, operation.progress, err)
	}
	if err := operation.store.inject(ctx, BoundaryRepairDestinationMetadataSync, After, operation.destinationPath, 0); err != nil {
		return identity, digest, repairFailure(RepairCauseDurability, RepairStageCanonicalVerify, operation.progress, err)
	}
	return identity, digest, nil
}

func (operation *RepairQuarantine) requireSourceAbsent() error {
	var stat unix.Statx_t
	err := unix.Statx(operation.sourceParentFD, operation.sourceName, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE, &stat)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrCollision
}

func repairOpenat2(dirfd int, path string, flags uint64) (int, error) {
	return unix.Openat2(dirfd, path, &unix.OpenHow{Flags: flags, Resolve: repairResolve})
}

func repairIdentityAt(dirfd int, path string, flags int) (repairNamespaceIdentity, error) {
	var stat unix.Statx_t
	mask := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_MNT_ID | unix.STATX_SIZE | unix.STATX_NLINK | unix.STATX_MTIME | unix.STATX_CTIME | unix.STATX_UID | unix.STATX_GID
	if err := unix.Statx(dirfd, path, flags, mask, &stat); err != nil {
		return repairNamespaceIdentity{}, err
	}
	return repairNamespaceIdentity{DeviceMajor: stat.Dev_major, DeviceMinor: stat.Dev_minor,
		Inode: stat.Ino, MountID: stat.Mnt_id, Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid, Links: stat.Nlink,
		Size: stat.Size, MTimeSec: stat.Mtime.Sec, MTimeNsec: stat.Mtime.Nsec,
		CTimeSec: stat.Ctime.Sec, CTimeNsec: stat.Ctime.Nsec}, nil
}

func repairIdentityTime(seconds int64, nanoseconds uint32) time.Time {
	return time.Unix(seconds, int64(nanoseconds)).UTC().Truncate(time.Microsecond)
}

func sameRepairNamespace(left, right repairNamespaceIdentity) bool {
	return left.DeviceMajor == right.DeviceMajor && left.DeviceMinor == right.DeviceMinor &&
		left.Inode == right.Inode && left.MountID == right.MountID &&
		left.Mode == right.Mode && left.UID == right.UID && left.GID == right.GID
}

func safeRepairDirectory(identity, owner repairNamespaceIdentity) bool {
	return identity.Mode&unix.S_IFMT == unix.S_IFDIR && identity.Mode&0o022 == 0 &&
		identity.UID == owner.UID && identity.GID == owner.GID
}

func sameRepairFileObject(left, right repairNamespaceIdentity) bool {
	return left.DeviceMajor == right.DeviceMajor && left.DeviceMinor == right.DeviceMinor &&
		left.Inode == right.Inode && left.MountID == right.MountID
}

func sameRepairMovedFile(left, right repairNamespaceIdentity) bool {
	return sameRepairFileObject(left, right) && left.Mode == right.Mode &&
		left.UID == right.UID && left.GID == right.GID && left.Links == right.Links &&
		left.Size == right.Size && left.MTimeSec == right.MTimeSec && left.MTimeNsec == right.MTimeNsec
}

func repairHashFD(ctx context.Context, fd int) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return result, err
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		count, err := unix.Read(fd, buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return result, err
		}
		if count == 0 {
			break
		}
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func repairFailure(cause RepairCause, stage RepairStage, progress RepairProgress, err error) error {
	return &RepairError{Cause: cause, Stage: stage, Progress: progress, Err: err}
}

func repairContextFailure(stage RepairStage, progress RepairProgress, err error) error {
	cause := RepairCauseInternal
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		cause = RepairCauseCancelled
	}
	return repairFailure(cause, stage, progress, err)
}

func repairErrorCause(err error) RepairCause {
	var repairErr *RepairError
	if errors.As(err, &repairErr) {
		return repairErr.Cause
	}
	return repairCauseForError(err, RepairCauseInternal)
}

func repairCauseForError(err error, fallback RepairCause) RepairCause {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return RepairCauseCancelled
	case errors.Is(err, unix.EXDEV), errors.Is(err, ErrScanBoundary):
		return RepairCauseCrossDevice
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, ErrPermission):
		return RepairCausePermission
	case errors.Is(err, unix.EROFS), errors.Is(err, ErrReadOnly):
		return RepairCauseReadOnly
	case errors.Is(err, unix.ENOSPC), errors.Is(err, ErrNoSpace):
		return RepairCauseNoSpace
	case errors.Is(err, unix.EDQUOT), errors.Is(err, ErrQuota):
		return RepairCauseQuota
	case errors.Is(err, unix.EEXIST), errors.Is(err, ErrCollision):
		return RepairCauseCollision
	case errors.Is(err, ErrUnexpectedType), errors.Is(err, ErrSymlink):
		return RepairCauseWrongType
	default:
		return fallback
	}
}

func repairSourceObservationCause(err error) RepairCause {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
		return RepairCauseSourceChanged
	}
	return repairCauseForError(err, RepairCauseInternal)
}

func repairSourceLeafCause(err error) RepairCause {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
		return RepairCauseSourceChanged
	}
	return repairCauseForError(err, RepairCauseInternal)
}

func repairInitialSourceOpenCause(err error) RepairCause {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) {
		return RepairCauseWrongType
	}
	return repairCauseForError(err, RepairCauseInternal)
}
