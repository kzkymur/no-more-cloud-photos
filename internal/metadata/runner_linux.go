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

	limitArgument := "--as=" + strconv.FormatUint(r.policy.AddressSpaceBytes, 10)
	args := append([]string{limitArgument, "--", executable}, arguments...)
	cmd := exec.Command(r.prlimit, args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TZ=UTC"}
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true, Pdeathsig: unix.SIGKILL}
	exceededSignal := make(chan struct{}, 1)
	stdout := &limitedBuffer{limit: r.policy.OutputBytes, signal: exceededSignal}
	stderr := &limitedBuffer{limit: r.policy.OutputBytes, signal: exceededSignal}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return commandOutput{}, fmt.Errorf("%w: start", ErrProbeFailed)
	}

	killGroup := func() {
		if cmd.Process != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitResult:
		// A trusted probe should not outlive its direct process, but kill any
		// residual member before returning on every terminal path.
		killGroup()
	case <-runCtx.Done():
		killGroup()
		waitErr = <-waitResult
		if ctx.Err() != nil {
			return commandOutput{}, ctx.Err()
		}
		return commandOutput{}, ErrProbeTimeout
	case <-exceededSignal:
		killGroup()
		waitErr = <-waitResult
		return commandOutput{}, ErrProbeOutputLimit
	}
	if stdout.exceeded.Load() || stderr.exceeded.Load() {
		return commandOutput{}, ErrProbeOutputLimit
	}
	if waitErr != nil {
		return commandOutput{}, fmt.Errorf("%w: non-zero exit", ErrProbeFailed)
	}
	return commandOutput{stdout: stdout.bytes(), stderr: stderr.bytes()}, nil
}
