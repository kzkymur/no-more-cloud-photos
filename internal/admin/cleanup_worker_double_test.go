package admin

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// withCleanupWorkerDouble gives a test Service an in-process stand-in for
// the Worker's rendition sweep and purge executor (review D1: the CLI only
// authorizes and awaits). It performs exactly the native lifecycle calls the
// Worker makes for each awaited item, so the CLI's journal proofs, receipts
// and resumption are exercised against real effects.
func withCleanupWorkerDouble(s *Service) *Service {
	s.cleanupWorker = inProcessCleanupWorker(s)
	s.cleanupWaitFor = 3 * time.Second
	return s
}

func inProcessCleanupWorker(s *Service) func(context.Context, *storage.Store, string, []cleanupItem) {
	return func(ctx context.Context, store *storage.Store, kind string, items []cleanupItem) {
		lifecycle, err := medialifecycle.NewService(s.Pool, os.Getenv("NMCP_FILE_BASE_URL"))
		if err != nil {
			return
		}
		for _, item := range items {
			if kind == "renditions" {
				_, _ = lifecycle.CleanupNextRendition(ctx, item.id, nil, func(ctx context.Context, id, path string, size int64) (bool, error) {
					key, err := storage.ParseRenditionKey(path)
					if err != nil || key.RenditionID().String() != id {
						return false, storage.ErrValidation
					}
					result, err := store.DeleteRendition(ctx, key, storage.DeleteExpectation{ExpectedSize: size})
					return result.Missing, err
				})
				continue
			}
			if item.jobID == nil {
				continue
			}
			repository, err := medialifecycle.NewPostgresRepository(s.Pool, os.Getenv("NMCP_FILE_BASE_URL"))
			if err != nil || repository.ReclaimExpiredPurge(ctx, *item.jobID) != nil {
				continue
			}
			lease, err := lifecycle.StartPurge(ctx, *item.jobID)
			if err != nil {
				continue
			}
			_, _ = executeCleanupPurge(ctx, lifecycle, store, lease)
		}
	}
}

type cleanupPurgeExecutor interface {
	HeartbeatPurge(context.Context, string, string) (time.Time, error)
	RunPurgeFileStep(context.Context, string, string, medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error)
	FinalizePurge(context.Context, string, string) error
	FinishPurgeAttempt(context.Context, string, string, string) error
}

func executeCleanupPurge(ctx context.Context, lifecycle cleanupPurgeExecutor, store *storage.Store, lease medialifecycle.PurgeLease) (bool, error) {
	for {
		if _, err := lifecycle.HeartbeatPurge(ctx, lease.JobID, lease.Token); err != nil {
			return false, err
		}
		step, err := lifecycle.RunPurgeFileStep(ctx, lease.JobID, lease.Token, medialifecycle.StoragePurgeFileAction(store))
		if err != nil {
			if cleanupCanReleaseAttempt(err) {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				releaseErr := lifecycle.FinishPurgeAttempt(releaseCtx, lease.JobID, lease.Token, "file_delete_failed")
				cancel()
				err = errors.Join(err, releaseErr)
			}
			return false, err
		}
		if step.Done {
			err := lifecycle.FinalizePurge(ctx, lease.JobID, lease.Token)
			return err == nil, err
		}
	}
}

// Physical uncertainty is not a known failure and must not shorten the lease.
func cleanupCanReleaseAttempt(err error) bool {
	var unknown *medialifecycle.CommitOutcomeUnknown
	if errors.As(err, &unknown) || medialifecycle.IsKind(err, medialifecycle.KindDatabaseUnavailable) ||
		errors.Is(err, medialifecycle.ErrPurgeLeaseLost) || errors.Is(err, storage.ErrOutcomeUncertain) || errors.Is(err, storage.ErrDurability) {
		return false
	}
	return errors.Is(err, storage.ErrPermission) || errors.Is(err, storage.ErrNoSpace) || errors.Is(err, storage.ErrQuota) ||
		errors.Is(err, storage.ErrReadOnly) || errors.Is(err, storage.ErrValidation) || errors.Is(err, storage.ErrSymlink) || errors.Is(err, storage.ErrUnexpectedType)
}

// TestAdminIntegrationCleanupOnlyEnqueuesAndAwaitsTheWorker is review D1 on
// the production path (no Worker double): the CLI binds the exact purge job,
// performs no effect itself, reports the item pending, and a later run with
// the Worker's effect completed adopts the same authorization.
func TestAdminIntegrationCleanupOnlyEnqueuesAndAwaitsTheWorker(t *testing.T) {
	s := integrationService(t)
	s.cleanupWorker, s.cleanupWaitFor = nil, 1500*time.Millisecond
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mediaID := insertMedia(t, s, 401)
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := medialifecycle.NewService(s.Pool, "https://files.example.test/files/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Delete(ctx, mediaID); err != nil {
		t.Fatal(err)
	}
	first, err := s.Cleanup(ctx, store, "media", false)
	if !errors.Is(err, ErrPartial) || first.Pending != 1 || first.Completed != 0 || first.Failed != 0 || !first.MorePossible {
		t.Fatalf("awaiting cleanup=%+v err=%v", first, err)
	}
	var jobID, status string
	var started bool
	if err := s.Pool.QueryRow(ctx, `SELECT i.job_id::text,j.status,j.started_at IS NOT NULL FROM admin_cleanup_items i
		JOIN jobs j ON j.id=i.job_id WHERE i.run_id=$1 AND i.outcome='pending'`, first.RunID).Scan(&jobID, &status, &started); err != nil || started {
		t.Fatalf("bound purge job=%s status=%s started=%v err=%v (the CLI must not execute it)", jobID, status, started, err)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM media WHERE id=$1)`, mediaID).Scan(&exists); err != nil || !exists {
		t.Fatalf("CLI purged media itself exists=%v err=%v", exists, err)
	}
	// The Worker executes the enqueued job; the next run adopts its receipt.
	inProcessCleanupWorker(s)(ctx, store, "media", []cleanupItem{{id: mediaID, mediaID: mediaID, jobID: &jobID}})
	resumed, err := s.Cleanup(ctx, store, "media", false)
	if err != nil || resumed.RunID != first.RunID || resumed.Completed != 1 || resumed.Pending != 0 {
		t.Fatalf("resumed cleanup=%+v err=%v", resumed, err)
	}
}
