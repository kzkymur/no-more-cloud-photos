// Package medialifecycle implements transactional media delete, restore,
// purge-enqueue, and purge-start state transitions.
package medialifecycle

import (
	"errors"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

var ErrNoPurgeWork = errors.New("no eligible purge work")

const (
	// InitialPurgeMaxAttempts is snapshotted into each newly enqueued purge job.
	InitialPurgeMaxAttempts = 3
	// MaxDeletedMediaRetentionDays is the maximum finite deletion retention.
	MaxDeletedMediaRetentionDays = 36500
	// DefaultPurgeLeaseDuration is the fixed initial lease granted by StartPurge.
	DefaultPurgeLeaseDuration = 2 * time.Minute
)

type DeleteResult struct {
	Media   readapi.MediaDetail
	Changed bool
}

type RestoreResult struct {
	Media readapi.MediaDetail
}

type EnqueueDisposition string

const (
	EnqueueCreated         EnqueueDisposition = "created"
	EnqueueExistingQueued  EnqueueDisposition = "existing_queued"
	EnqueueExistingRunning EnqueueDisposition = "existing_running"
)

type EnqueueResult struct {
	Job         readapi.Job
	Disposition EnqueueDisposition
}

type DuePurgeCursor struct {
	PurgeAfter time.Time
	MediaID    string
}

type DuePurgeCandidate struct {
	MediaID    string
	PurgeAfter time.Time
}

type PurgeLease struct {
	JobID          string
	MediaID        string
	Token          string
	Attempts       int
	MaxAttempts    int
	LeaseExpiresAt time.Time
	StartedAt      time.Time
	AvailableAt    time.Time
	CreatedAt      time.Time
}
