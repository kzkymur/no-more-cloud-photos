//go:build nmcp_failpoints

package purgeexecutor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

const purgeFailpointChildEnv = "NMCP_PURGE_FAILPOINT_CHILD"

// tracingService journals every durable step to unbuffered stdout so the
// parent can see exactly which steps completed before SIGKILL.
type tracingService struct {
	file  medialifecycle.PurgeFile
	steps int
}

func (s *tracingService) StartNextPurge(context.Context, int) (medialifecycle.PurgeLease, error) {
	return medialifecycle.PurgeLease{}, errors.New("unused")
}

func (s *tracingService) RunPurgeFileStep(ctx context.Context, _, _ string, action medialifecycle.PurgeFileAction) (medialifecycle.PurgeStepResult, error) {
	s.steps++
	if s.steps > 2 {
		trace("done")
		return medialifecycle.PurgeStepResult{Done: true}, nil
	}
	disposition, err := action(ctx, s.file)
	if err != nil {
		return medialifecycle.PurgeStepResult{}, err
	}
	trace("file " + strconv.Itoa(s.steps) + " " + string(disposition) + " committed")
	return medialifecycle.PurgeStepResult{File: &s.file}, nil
}

func (s *tracingService) FinalizePurge(context.Context, string, string) error {
	trace("finalized")
	return nil
}

func (s *tracingService) HeartbeatPurge(context.Context, string, string) (time.Time, error) {
	return time.Time{}, errors.New("unused")
}

func (s *tracingService) FinishPurgeAttempt(context.Context, string, string, string) error {
	return errors.New("unused")
}

func trace(line string) { _, _ = os.Stdout.WriteString(line + "\n") }

func TestPurgeFailpointsKillAtExactBoundaries(t *testing.T) {
	if os.Getenv(purgeFailpointChildEnv) == "1" {
		runPurgeFailpointChild(t)
		return
	}
	cases := []struct{ point, stdout string }{
		{"purge.after_start", ""},
		{"purge.after_file_delete", ""},
		{"purge.after_file_step", "file 1 missing committed\n"},
		{"purge.after_file_delete@2", "file 1 missing committed\n"},
		{"purge.after_file_step@2", "file 1 missing committed\nfile 2 missing committed\n"},
		{"purge.before_finalize", "file 1 missing committed\nfile 2 missing committed\ndone\n"},
		{"purge.after_finalize", "file 1 missing committed\nfile 2 missing committed\ndone\nfinalized\n"},
	}
	for _, test := range cases {
		t.Run(test.point, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestPurgeFailpointsKillAtExactBoundaries$", "-test.count=1")
			command.Env = append(os.Environ(), purgeFailpointChildEnv+"=1", "NMCP_FAILPOINT="+test.point)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			var exitErr *exec.ExitError
			status, ok := syscall.WaitStatus(0), false
			if errors.As(err, &exitErr) {
				status, ok = exitErr.Sys().(syscall.WaitStatus)
			}
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child error = %v, want SIGKILL; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if stdout.String() != test.stdout {
				t.Fatalf("completed steps = %q, want %q", stdout.String(), test.stdout)
			}
			name, _, _ := strings.Cut(test.point, "@")
			if !strings.HasSuffix(stderr.String(), "nmcp-failpoint fired "+name+"\n") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func runPurgeFailpointChild(t *testing.T) {
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	originalID, _ := storage.ParseOriginalID("11111111-1111-4111-8111-111111111111")
	targetID, _ := storage.ParseJobTargetID("22222222-2222-4222-8222-222222222222")
	renditionID, _ := storage.ParseRenditionID("33333333-3333-4333-8333-333333333333")
	key, err := storage.NewRenditionKey(originalID, targetID, renditionID, storage.RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	service := &tracingService{file: medialifecycle.PurgeFile{Kind: medialifecycle.PurgeFileRendition,
		ObjectID: renditionID.String(), RelativePath: key.String()}}
	lease := job.Lease{ID: "job", MediaID: "media", Token: "token", Type: job.TypePurge}
	err = (Executor{Service: service, Store: store}).Execute(context.Background(), lease, worker.ExecutionLimits{})
	t.Fatalf("armed purge failpoint did not kill the process; Execute error = %v", err)
}
