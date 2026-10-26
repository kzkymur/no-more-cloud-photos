package admin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func cleanupDurabilityFixture(t *testing.T) (*Service, *medialifecycle.Service, *storage.Store) {
	t.Helper()
	s := integrationService(t)
	if _, err := s.ConfigSet(context.Background(), "deleted_media_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := medialifecycle.NewService(s.Pool, "https://files.example.test/files/")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return s, lifecycle, store
}

func cleanupDurabilityDelete(t *testing.T, s *Service, lifecycle *medialifecycle.Service, number int) string {
	t.Helper()
	id := insertMedia(t, s, number)
	if _, err := lifecycle.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func cleanupDurabilityExec(t *testing.T, s *Service, sql string, args ...any) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestAdminIntegrationCleanupLegacyEmptyRunDoesNotStarveNewWork(t *testing.T) {
	for _, terminalItem := range []bool{false, true} {
		name := "no_items"
		if terminalItem {
			name = "last_receipt_committed"
		}
		t.Run(name, func(t *testing.T) {
			s, lifecycle, store := cleanupDurabilityFixture(t)
			ctx := context.Background()
			legacyRun := newID()
			if terminalItem {
				cleanupDurabilityDelete(t, s, lifecycle, 600)
				first, err := s.Cleanup(ctx, store, "media", false)
				if err != nil || first.Completed != 1 {
					t.Fatalf("first=%+v err=%v", first, err)
				}
				legacyRun = first.RunID
				// Reproduce the old crash gap: terminal receipt, unfinished run.
				cleanupDurabilityExec(t, s, `UPDATE admin_cleanup_runs SET finished_at=NULL WHERE id=$1`, legacyRun)
			} else {
				cleanupDurabilityExec(t, s, `INSERT INTO admin_cleanup_runs(id,kind) VALUES($1,'media')`, legacyRun)
			}
			id := cleanupDurabilityDelete(t, s, lifecycle, 601)
			result, err := New(s.Pool).Cleanup(ctx, store, "media", false)
			if err != nil || result.RunID == legacyRun || result.Completed != 1 || len(result.IDs) != 1 || result.IDs[0] != id {
				t.Fatalf("new work starved: result=%+v err=%v", result, err)
			}
			var bothFinished bool
			if err := s.Pool.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(finished_at IS NOT NULL)
				FROM admin_cleanup_runs WHERE id IN ($1,$2)`, legacyRun, result.RunID).Scan(&bothFinished); err != nil || !bothFinished {
				t.Fatalf("runs finished=%v err=%v", bothFinished, err)
			}
		})
	}
}

func TestAdminIntegrationCleanupInterruptedLeaseExpiresAndRecovers(t *testing.T) {
	s, lifecycle, store := cleanupDurabilityFixture(t)
	ctx := context.Background()
	id := cleanupDurabilityDelete(t, s, lifecycle, 610)
	enqueued, err := lifecycle.EnqueueDuePurge(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lifecycle.StartPurge(ctx, enqueued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated expired purge is outside the due-media snapshot. Exact-job
	// reclaim must not act as a global worker scheduler on the CLI's behalf.
	if _, err := s.ConfigSet(ctx, "deleted_media_retention_days", "36500"); err != nil {
		t.Fatal(err)
	}
	otherID := cleanupDurabilityDelete(t, s, lifecycle, 611)
	other, err := lifecycle.EnqueuePurge(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StartPurge(ctx, other.Job.ID); err != nil {
		t.Fatal(err)
	}
	cleanupDurabilityExec(t, s, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, other.Job.ID)
	var otherBefore, otherAfter []byte
	if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM jobs j WHERE id=$1`, other.Job.ID).Scan(&otherBefore); err != nil {
		t.Fatal(err)
	}
	// No worker loop runs: this committed start is the interrupted process.
	var before, after []byte
	if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM jobs j WHERE id=$1`, lease.JobID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	live, err := New(s.Pool).Cleanup(ctx, store, "media", false)
	if !errors.Is(err, ErrPartial) || live.Failed != 1 || live.Completed != 0 {
		t.Fatalf("live lease cleanup=%+v err=%v", live, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM jobs j WHERE id=$1`, lease.JobID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("live lease changed: before=%s after=%s err=%v", before, after, err)
	}
	cleanupDurabilityExec(t, s, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, lease.JobID)
	recovered, err := New(s.Pool).Cleanup(ctx, store, "media", false)
	if errors.Is(err, ErrPartial) {
		// Native reclaim honors retry backoff; advance eligibility, not leases.
		var released bool
		if err := s.Pool.QueryRow(ctx, `SELECT status='queued' AND lease_token IS NULL AND lease_expires_at IS NULL
			AND attempts=1 AND started_at=$2 AND error_code='lease_expired' FROM jobs WHERE id=$1`, lease.JobID, lease.StartedAt).Scan(&released); err != nil || !released {
			t.Fatalf("expired attempt not released=%v err=%v", released, err)
		}
		cleanupDurabilityExec(t, s, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, lease.JobID)
		recovered, err = New(s.Pool).Cleanup(ctx, store, "media", false)
	}
	if err != nil || recovered.RunID != live.RunID || recovered.Completed != 1 || recovered.Failed != 0 {
		t.Fatalf("recovery=%+v err=%v", recovered, err)
	}
	var exactRetry bool
	if err := s.Pool.QueryRow(ctx, `SELECT status='succeeded' AND attempts=2 AND started_at=$2
		AND lease_token IS NULL AND (SELECT count(*) FROM jobs WHERE type='purge' AND media_id_snapshot=$3)=1
		FROM jobs WHERE id=$1`, lease.JobID, lease.StartedAt, id).Scan(&exactRetry); err != nil || !exactRetry {
		t.Fatalf("exact native retry=%v err=%v", exactRetry, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM jobs j WHERE id=$1`, other.Job.ID).Scan(&otherAfter); err != nil || !bytes.Equal(otherBefore, otherAfter) {
		t.Fatalf("unrelated expired lease changed: before=%s after=%s err=%v", otherBefore, otherAfter, err)
	}
	if err := lifecycle.FinishPurgeAttempt(ctx, lease.JobID, lease.Token, "file_delete_failed"); err == nil {
		t.Fatal("obsolete lease token released a completed successor")
	}
}

func TestAdminIntegrationCleanupRestoreRedeletePinsNewAuthorizationAndAudit(t *testing.T) {
	s, lifecycle, store := cleanupDurabilityFixture(t)
	ctx := context.Background()
	id := cleanupDurabilityDelete(t, s, lifecycle, 620)
	// A deferred check observes the receipt transaction itself, before the
	// caller could hide a crash gap with a separate run-completion update.
	cleanupDurabilityExec(t, s, `CREATE FUNCTION require_atomic_cleanup_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.outcome<>'pending' AND NOT EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=NEW.run_id AND outcome='pending')
		AND EXISTS(SELECT 1 FROM admin_cleanup_runs WHERE id=NEW.run_id AND finished_at IS NULL)
		THEN RAISE EXCEPTION 'last receipt committed before run completion'; END IF; RETURN NEW; END $$;
		CREATE CONSTRAINT TRIGGER require_atomic_cleanup_completion AFTER UPDATE ON admin_cleanup_items
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION require_atomic_cleanup_completion()`)
	items, oldRun, err := s.cleanupSelection(ctx, "media", false)
	if err != nil || len(items) != 1 {
		t.Fatalf("selection=%+v err=%v", items, err)
	}
	first, err := lifecycle.EnqueueDuePurge(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// The CLI stopped after binding J1, before its irreversible first start.
	cleanupDurabilityExec(t, s, `UPDATE admin_cleanup_items SET job_id=$3 WHERE run_id=$1 AND object_id=$2`, oldRun, id, first.Job.ID)
	if _, err := lifecycle.Restore(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.EnqueueDuePurge(ctx, id)
	if err != nil || second.Job.ID == first.Job.ID {
		t.Fatalf("replacement=%+v err=%v", second, err)
	}
	skipped, err := New(s.Pool).Cleanup(ctx, store, "media", false)
	if err != nil || skipped.RunID != oldRun || skipped.Skipped != 1 || skipped.Completed != 0 {
		t.Fatalf("old authorization=%+v err=%v", skipped, err)
	}
	var isolated bool
	if err := s.Pool.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM jobs WHERE id=$1 AND status='cancelled' AND started_at IS NULL AND cancel_reason='media_restored')
		AND EXISTS(SELECT 1 FROM jobs WHERE id=$2 AND status='queued' AND started_at IS NULL AND attempts=0)
		AND EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$3 AND job_id=$1 AND outcome='skipped')
		AND EXISTS(SELECT 1 FROM admin_cleanup_runs WHERE id=$3 AND finished_at IS NOT NULL)
		AND NOT EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$2)`, first.Job.ID, second.Job.ID, oldRun).Scan(&isolated); err != nil || !isolated {
		t.Fatalf("J2 executed under J1 authorization: isolated=%v err=%v", isolated, err)
	}
	cleanupDurabilityExec(t, s, `CREATE FUNCTION reject_durable_cleanup_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.command='cleanup_result' THEN RAISE EXCEPTION 'injected receipt rejection'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_durable_cleanup_audit BEFORE INSERT ON admin_audit FOR EACH ROW EXECUTE FUNCTION reject_durable_cleanup_audit()`)
	rejected, err := New(s.Pool).Cleanup(ctx, store, "media", false)
	if !errors.Is(err, ErrPartial) || rejected.RunID == oldRun || rejected.RunID == "" || rejected.Failed != 1 || rejected.Completed != 0 {
		t.Fatalf("rejected receipt=%+v err=%v", rejected, err)
	}
	var recoverable bool
	if err := s.Pool.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$1 AND object_id=$2 AND job_id=$3 AND outcome='pending')
		AND EXISTS(SELECT 1 FROM admin_cleanup_runs WHERE id=$1 AND finished_at IS NULL)
		AND EXISTS(SELECT 1 FROM jobs WHERE id=$3 AND status='succeeded' AND attempts=1)
		AND NOT EXISTS(SELECT 1 FROM media WHERE id=$2)
		AND EXISTS(SELECT 1 FROM admin_audit WHERE command='cleanup_begin' AND sanitized_arguments->>'run_id'=$1::text AND $2::uuid=ANY(affected_ids))
		AND NOT EXISTS(SELECT 1 FROM admin_audit WHERE command='cleanup_result' AND sanitized_arguments->>'run_id'=$1::text)`, rejected.RunID, id, second.Job.ID).Scan(&recoverable); err != nil || !recoverable {
		t.Fatalf("receipt crash gap lost authorization: recoverable=%v err=%v", recoverable, err)
	}
	cleanupDurabilityExec(t, s, `DROP TRIGGER reject_durable_cleanup_audit ON admin_audit`)
	resumed, err := New(s.Pool).Cleanup(ctx, store, "media", false)
	if err != nil || resumed.RunID != rejected.RunID || resumed.Completed != 1 || len(resumed.IDs) != 1 || resumed.IDs[0] != id {
		t.Fatalf("receipt recovery=%+v err=%v", resumed, err)
	}
	if _, err := New(s.Pool).Cleanup(ctx, store, "media", false); err != nil {
		t.Fatal(err)
	}
	var receipts, attempts int
	var finished bool
	if err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM admin_audit WHERE command='cleanup_result' AND sanitized_arguments->>'run_id'=$1::text
		AND sanitized_arguments->>'outcome'='completed' AND $2::uuid=ANY(affected_ids)),
		(SELECT attempts FROM jobs WHERE id=$3),finished_at IS NOT NULL FROM admin_cleanup_runs WHERE id=$1::uuid`, resumed.RunID, id, second.Job.ID).Scan(&receipts, &attempts, &finished); err != nil || receipts != 1 || attempts != 1 || !finished {
		t.Fatalf("replay receipts=%d attempts=%d finished=%v err=%v", receipts, attempts, finished, err)
	}
}

func TestAdminIntegrationCleanupDeleteFailureReleasesAttempt(t *testing.T) {
	s, lifecycle, _ := cleanupDurabilityFixture(t)
	ctx := context.Background()
	id := cleanupDurabilityDelete(t, s, lifecycle, 630)
	var path string
	if err := s.Pool.QueryRow(ctx, `SELECT relative_path FROM originals WHERE media_id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	faulted := false
	store, err := storage.Open(root, storage.Options{Faults: storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
		if event.Boundary == storage.BoundaryDelete && event.Phase == storage.Before {
			faulted = true
			return storage.ErrPermission
		}
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	failed, err := s.Cleanup(ctx, store, "media", false)
	if !errors.Is(err, ErrPartial) || failed.Failed != 1 || !faulted {
		t.Fatalf("faulted cleanup=%+v injected=%v err=%v", failed, faulted, err)
	}
	var released bool
	if err := s.Pool.QueryRow(ctx, `SELECT j.status='queued' AND j.attempts=1 AND j.lease_token IS NULL
		AND j.lease_expires_at IS NULL AND j.error_code='file_delete_failed' AND i.outcome='pending'
		AND NOT EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=j.id AND disposition<>'pending')
		FROM admin_cleanup_items i JOIN jobs j ON j.id=i.job_id WHERE i.run_id=$1`, failed.RunID).Scan(&released); err != nil || !released {
		t.Fatalf("failed attempt retained lease: released=%v err=%v", released, err)
	}
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("pre-delete failure changed original: %v", err)
	}
	cleanupDurabilityExec(t, s, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=(SELECT job_id FROM admin_cleanup_items WHERE run_id=$1)`, failed.RunID)
	clean, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()
	resumed, err := New(s.Pool).Cleanup(ctx, clean, "media", false)
	if err != nil || resumed.RunID != failed.RunID || resumed.Completed != 1 {
		t.Fatalf("released attempt recovery=%+v err=%v", resumed, err)
	}
}

func TestAdminIntegrationCleanupFrozenRenditionBecomesNativeIneligible(t *testing.T) {
	s, lifecycle, store := cleanupDurabilityFixture(t)
	ctx := context.Background()
	id := dueHistoricalRendition(t, s)
	items, runID, err := s.cleanupSelection(ctx, "renditions", false)
	if err != nil || len(items) != 1 || items[0].id != id {
		t.Fatalf("frozen renditions=%+v err=%v", items, err)
	}
	if _, err := lifecycle.Delete(ctx, items[0].mediaID); err != nil {
		t.Fatal(err)
	}
	enqueued, err := lifecycle.EnqueueDuePurge(ctx, items[0].mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StartPurge(ctx, enqueued.Job.ID); err != nil {
		t.Fatal(err)
	}
	// A started native purge owns this exact rendition; the frozen CLI run
	// must close as skipped rather than retry forever or bypass ownership.
	result, err := New(s.Pool).Cleanup(ctx, store, "renditions", false)
	if err != nil || result.RunID != runID || result.Skipped != 1 || result.Completed != 0 || result.Failed != 0 {
		t.Fatalf("native takeover cleanup=%+v err=%v", result, err)
	}
	var untouched bool
	if err := s.Pool.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM renditions WHERE id=$1)
		AND EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$2 AND object_id=$1 AND disposition='pending')
		AND NOT EXISTS(SELECT 1 FROM rendition_cleanup_progress WHERE rendition_id=$1)
		AND EXISTS(SELECT 1 FROM admin_cleanup_items WHERE run_id=$3 AND object_id=$1 AND outcome='skipped')
		AND EXISTS(SELECT 1 FROM admin_cleanup_runs WHERE id=$3 AND finished_at IS NOT NULL)`, id, enqueued.Job.ID, runID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("native ownership bypassed: untouched=%v err=%v", untouched, err)
	}
}

func TestAdminIntegrationCleanupPhysicalUncertaintyPreservesLiveTokenUntilExpiry(t *testing.T) {
	for _, boundary := range []storage.Boundary{storage.BoundaryDelete, storage.BoundaryDeleteDirectorySync} {
		t.Run(string(boundary), func(t *testing.T) {
			s, lifecycle, _ := cleanupDurabilityFixture(t)
			ctx := context.Background()
			id := cleanupDurabilityDelete(t, s, lifecycle, 650)
			var path string
			if err := s.Pool.QueryRow(ctx, `SELECT relative_path FROM originals WHERE media_id=$1`, id).Scan(&path); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte("0123456789"), 0o600); err != nil {
				t.Fatal(err)
			}
			phase := storage.After
			if boundary == storage.BoundaryDeleteDirectorySync {
				phase = storage.Before
			}
			injected := false
			faulted, err := storage.Open(root, storage.Options{Faults: storage.FaultInjectorFunc(func(_ context.Context, event storage.FaultEvent) error {
				if event.Boundary == boundary && event.Phase == phase {
					injected = true
					// This known-failure marker is wrapped in physical uncertainty
					// after unlink. Uncertainty must win: no early lease release.
					return storage.ErrPermission
				}
				return nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			defer faulted.Close()
			first, err := s.Cleanup(ctx, faulted, "media", false)
			if !errors.Is(err, ErrPartial) || first.Failed != 1 || first.Completed != 0 || !injected {
				t.Fatalf("uncertain cleanup=%+v injected=%v err=%v", first, injected, err)
			}
			if _, err := os.Stat(fullPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fault was not after actual unlink: %v", err)
			}
			var jobID, token string
			var before, after []byte
			if err := s.Pool.QueryRow(ctx, `SELECT j.id::text,j.lease_token::text,to_jsonb(j)
				FROM admin_cleanup_items i JOIN jobs j ON j.id=i.job_id
				WHERE i.run_id=$1 AND i.outcome='pending' AND j.status='running' AND j.attempts=1
				AND j.lease_token IS NOT NULL AND j.lease_expires_at>clock_timestamp()`, first.RunID).Scan(&jobID, &token, &before); err != nil {
				t.Fatalf("physical uncertainty released live ownership: %v", err)
			}
			var recoverable bool
			if err := s.Pool.QueryRow(ctx, `SELECT
				EXISTS(SELECT 1 FROM media WHERE id=$1)
				AND EXISTS(SELECT 1 FROM originals WHERE media_id=$1)
				AND EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$2 AND disposition='pending')
				AND NOT EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$2 AND disposition<>'pending')
				AND NOT EXISTS(SELECT 1 FROM admin_audit WHERE command='cleanup_result' AND $1::uuid=ANY(affected_ids))`, id, jobID).Scan(&recoverable); err != nil || !recoverable {
				t.Fatalf("lost uncertain native progress/authorization=%v err=%v", !recoverable, err)
			}
			clean, err := storage.Open(root, storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer clean.Close()
			live, err := New(s.Pool).Cleanup(ctx, clean, "media", false)
			if !errors.Is(err, ErrPartial) || live.RunID != first.RunID || live.Completed != 0 {
				t.Fatalf("live uncertain lease was stolen=%+v err=%v", live, err)
			}
			if err := s.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM jobs j WHERE id=$1`, jobID).Scan(&after); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("live job mutated before expiry: before=%s after=%s err=%v", before, after, err)
			}
			cleanupDurabilityExec(t, s, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, jobID)
			recovered, err := New(s.Pool).Cleanup(ctx, clean, "media", false)
			if errors.Is(err, ErrPartial) {
				var backoff bool
				if err := s.Pool.QueryRow(ctx, `SELECT status='queued' AND attempts=1 AND lease_token IS NULL
					AND lease_expires_at IS NULL AND error_code='lease_expired' FROM jobs WHERE id=$1`, jobID).Scan(&backoff); err != nil || !backoff {
					t.Fatalf("exact expiry did not preserve native backoff: %v err=%v", backoff, err)
				}
				cleanupDurabilityExec(t, s, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, jobID)
				recovered, err = New(s.Pool).Cleanup(ctx, clean, "media", false)
			}
			if err != nil || recovered.RunID != first.RunID || recovered.Completed != 1 || len(recovered.IDs) != 1 || recovered.IDs[0] != id {
				t.Fatalf("uncertain missing-file recovery=%+v err=%v", recovered, err)
			}
			if _, err := New(s.Pool).Cleanup(ctx, clean, "media", false); err != nil {
				t.Fatal(err)
			}
			var completedOnce bool
			if err := s.Pool.QueryRow(ctx, `SELECT status='succeeded' AND attempts=2 AND lease_token IS NULL
				AND (SELECT count(*) FROM jobs WHERE type='purge' AND media_id_snapshot=$2)=1
				AND EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$1 AND object_kind='original' AND disposition='missing')
				AND (SELECT count(*) FROM change_events WHERE media_id=$2 AND event_type='media_purged' AND reason='physical_purge')=1
				AND (SELECT count(*) FROM admin_audit WHERE command='cleanup_result' AND $2::uuid=ANY(affected_ids)
				     AND sanitized_arguments->>'outcome'='completed')=1
				FROM jobs WHERE id=$1`, jobID, id).Scan(&completedOnce); err != nil || !completedOnce {
				t.Fatalf("uncertain completion lost identity/exactly-once=%v err=%v", !completedOnce, err)
			}
			if err := lifecycle.FinishPurgeAttempt(ctx, jobID, token, "file_delete_failed"); err == nil {
				t.Fatal("obsolete pre-expiry token released successor completion")
			}
		})
	}
}
