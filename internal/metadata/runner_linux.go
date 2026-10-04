//go:build linux

package metadata

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type boundedRunner struct {
	prlimit string
	policy  Policy
}

type commandOutput struct {
	stdout []byte
	stderr []byte
}

const supervisorReturnGrace = 2 * time.Second

// limitedBuffer keeps at most limit bytes while continuing to consume the
// child's pipe. The first overflow wakes the runner so it can kill the entire
// process group. Returning len(p), nil avoids an os/exec pipe-copy race.
type limitedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int64
	exceeded atomic.Bool
	signal   chan<- struct{}
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	remaining := w.limit - int64(w.buffer.Len())
	if remaining > 0 {
		keep := int64(len(p))
		if keep > remaining {
			keep = remaining
		}
		_, _ = w.buffer.Write(p[:keep])
	}
	w.mu.Unlock()
	if int64(len(p)) > remaining {
		if w.exceeded.CompareAndSwap(false, true) {
			select {
			case w.signal <- struct{}{}:
			default:
			}
		}
	}
	return len(p), nil
}

func (w *limitedBuffer) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func (r boundedRunner) run(ctx context.Context, executable string, arguments ...string) (commandOutput, error) {
	return r.runWithFiles(ctx, executable, nil, arguments...)
}

func (r boundedRunner) runWithFiles(ctx context.Context, executable string, files []*os.File, arguments ...string) (commandOutput, error) {
	runCtx, cancel := context.WithTimeout(ctx, r.policy.Timeout)
	defer cancel()

	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return commandOutput{}, fmt.Errorf("%w: prepare supervisor control", ErrProbeFailed)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	args := []string{
		probeSupervisorArgument,
		r.prlimit,
		strconv.FormatUint(r.policy.AddressSpaceBytes, 10),
		strconv.Itoa(len(files)),
		executable,
	}
	args = append(args, arguments...)
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TZ=UTC"}
	cmd.ExtraFiles = append([]*os.File{controlRead}, files...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	exceededSignal := make(chan struct{}, 1)
	stdout := &limitedBuffer{limit: r.policy.OutputBytes, signal: exceededSignal}
	stderr := &limitedBuffer{limit: r.policy.OutputBytes, signal: exceededSignal}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return commandOutput{}, fmt.Errorf("%w: start", ErrProbeFailed)
	}
	_ = controlRead.Close()

	requestCleanup := func() {
		_ = controlWrite.Close()
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitResult:
		requestCleanup()
	case <-runCtx.Done():
		requestCleanup()
		var completed bool
		waitErr, completed = waitForSupervisor(waitResult, supervisorReturnGrace)
		if !completed {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			return commandOutput{}, fmt.Errorf("%w: supervisor cleanup timeout", ErrProbeFailed)
		}
	case <-exceededSignal:
		requestCleanup()
		var completed bool
		waitErr, completed = waitForSupervisor(waitResult, supervisorReturnGrace)
		if !completed {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			return commandOutput{}, fmt.Errorf("%w: supervisor cleanup timeout", ErrProbeFailed)
		}
	}
	// Classify after cleanup using a stable precedence rather than the random
	// arm chosen when terminal signals become ready together.
	if ctx.Err() != nil {
		return commandOutput{}, ctx.Err()
	}
	if runCtx.Err() != nil {
		return commandOutput{}, ErrProbeTimeout
	}
	if stdout.exceeded.Load() || stderr.exceeded.Load() {
		return commandOutput{}, ErrProbeOutputLimit
	}
	if waitErr != nil {
		return commandOutput{}, fmt.Errorf("%w: non-zero exit", ErrProbeFailed)
	}
	return commandOutput{stdout: stdout.bytes(), stderr: stderr.bytes()}, nil
}

func waitForSupervisor(waitResult <-chan error, grace time.Duration) (error, bool) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitResult:
		return err, true
	case <-timer.C:
		return nil, false
	}
}
