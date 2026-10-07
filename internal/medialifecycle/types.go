// Package medialifecycle implements the transactional media delete, restore,
// and purge-enqueue state transitions.
package medialifecycle

import "github.com/kzkymur/no-more-cloud-photos/internal/readapi"

const (
	// InitialPurgeMaxAttempts is snapshotted into each newly enqueued purge job.
	InitialPurgeMaxAttempts = 3
	// MaxDeletedMediaRetentionDays is the maximum finite deletion retention.
	MaxDeletedMediaRetentionDays = 36500
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
