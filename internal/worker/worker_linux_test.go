//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
)

func TestWorkerShutdownReapsExecutorSessionAndDoubleForkDescendants(t *testing.T) {
	directory := t.TempDir()
	topPIDFile := filepath.Join(directory, "top")
	leafPIDFile := filepath.Join(directory, "leaf")
	script := filepath.Join(directory, "escape")
	body := "#!/bin/sh\nsetsid sh -c 'sleep 30 & printf %s \"$!\" > \"$1\"; wait' helper \"$2\" &\nprintf '%s' \"$!\" > \"$1\"\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := processrunner.New("/usr/bin/prlimit", processrunner.Limits{
		Timeout: 5 * time.Second, AddressSpaceBytes: 1 << 30, OutputBytesPerStream: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeRepository{}
	executor := executorFunc(func(ctx context.Context, _ job.Lease, limits ExecutionLimits) error {
		if limits.Threads != 1 {
			t.Errorf("thread limit = %d", limits.Threads)
		}
		_, err := runner.Run(ctx, processrunner.Command{Executable: script, Arguments: []string{topPIDFile, leafPIDFile}})
		return err
	})
	worker := testWorker(t, repository, executor)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.runLease(ctx, testLease()) }()
	topPID := waitWorkerPID(t, topPIDFile)
	leafPID := waitWorkerPID(t, leafPIDFile)
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	assertWorkerProcessGone(t, topPID)
	assertWorkerProcessGone(t, leafPID)
	if len(repository.finishCodes) != 1 || repository.finishCodes[0] != job.FailureWorkerShutdown {
		t.Fatalf("shutdown finish codes = %v", repository.finishCodes)
	}
}

func waitWorkerPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr == nil {
				return pid
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process PID was not written to %s", path)
	return 0
}

func assertWorkerProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Worker executor descendant %d still exists", pid)
}
