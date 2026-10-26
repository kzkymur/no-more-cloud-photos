package admin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

type uncertainCleanupExecutor struct {
	file         medialifecycle.PurgeFile
	heartbeatErr error
	stepErr      error
	finalizeErr  error
	action       bool
	done         bool
	steps        int
	finalized    int
	finished     int
}

func (e *uncertainCleanupExecutor) HeartbeatPurge(context.Context, string, string) (time.Time, error) {
	return time.Now().Add(time.Minute), e.heartbeatErr
}
func (e *uncertainCleanupExecutor) RunPurgeFileStep(ctx context.Context, _, _ string, action medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error) {
	e.steps++
	if e.action {
		if _, err := action(ctx, e.file); err != nil {
			return medialifecycle.PurgeStepResult{}, err
		}
	}
	return medialifecycle.PurgeStepResult{Done: e.done}, e.stepErr
}
func (e *uncertainCleanupExecutor) FinalizePurge(context.Context, string, string) error {
	e.finalized++
	return e.finalizeErr
}
func (e *uncertainCleanupExecutor) FinishPurgeAttempt(context.Context, string, string, string) error {
	e.finished++
	return nil
}

func TestCleanupPhysicalStepUnknownCommitDoesNotReleaseOrContinue(t *testing.T) {
	root := t.TempDir()
	id := "10000000-0000-4000-8000-000000000001"
	relative := "originals/10/" + id + "/original.jpg"
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unknown := &medialifecycle.CommitOutcomeUnknown{Cause: io.ErrUnexpectedEOF}
	executor := &uncertainCleanupExecutor{
		file: medialifecycle.PurgeFile{Kind: medialifecycle.PurgeFileOriginal, ObjectID: id, RelativePath: relative, SizeBytes: 3},
		action: true, stepErr: unknown,
	}
	done, err := executeCleanupPurge(context.Background(), executor, store, medialifecycle.PurgeLease{JobID: newID(), Token: newID()})
	if done || !errors.Is(err, unknown) || executor.steps != 1 || executor.finished != 0 || executor.finalized != 0 {
		t.Fatalf("uncertain physical step continued: done=%v err=%v executor=%+v", done, err, executor)
	}
	if _, err := os.Stat(absolute); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("physical effect was not actually exercised: %v", err)
	}
}

func TestCleanupUnknownHeartbeatAndFinalizeNeverReleaseLease(t *testing.T) {
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unknown := &medialifecycle.CommitOutcomeUnknown{Cause: io.ErrUnexpectedEOF}
	for _, boundary := range []string{"heartbeat", "finalize", "known-storage-failure"} {
		t.Run(boundary, func(t *testing.T) {
			executor := &uncertainCleanupExecutor{done: true}
			switch boundary {
			case "heartbeat":
				executor.heartbeatErr = unknown
			case "finalize":
				executor.finalizeErr = unknown
			default:
				executor.stepErr = storage.ErrPermission
			}
			done, err := executeCleanupPurge(context.Background(), executor, store, medialifecycle.PurgeLease{JobID: newID(), Token: newID()})
			if done || err == nil {
				t.Fatalf("failed boundary claimed success: done=%v err=%v", done, err)
			}
			if boundary == "known-storage-failure" {
				if executor.finished != 1 || executor.finalized != 0 {
					t.Fatalf("known failure not released once: %+v", executor)
				}
			} else if executor.finished != 0 {
				t.Fatalf("uncertain ownership released: %+v", executor)
			}
			if boundary == "heartbeat" && executor.steps != 0 || boundary == "finalize" && executor.finalized != 1 {
				t.Fatalf("wrong execution boundary: %+v", executor)
			}
		})
	}
}
