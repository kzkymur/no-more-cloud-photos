//go:build linux

// Package processrunner executes trusted, absolute tool paths without a shell
// and contains their Linux process trees in a short-lived subreaper.
package processrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalid       = errors.New("invalid process runner input")
	ErrTimeout       = errors.New("process timed out")
	ErrOutputLimit   = errors.New("process output limit exceeded")
	ErrFileSizeLimit = errors.New("process file size limit exceeded")
	ErrFailed        = errors.New("process failed")
)

const (
	maxExtraFiles         = 8
	supervisorReturnGrace = 2 * time.Second
)

type Limits struct {
	Timeout              time.Duration
	AddressSpaceBytes    uint64
	FileSizeBytes        uint64
	OutputBytesPerStream int64
}

type Command struct {
	Executable string
	Arguments  []string
	Files      []*os.File
}

type Output struct {
	Stdout []byte
	Stderr []byte
}

type Runner struct {
	prlimit string
	limits  Limits
}

func New(prlimitPath string, limits Limits) (*Runner, error) {
	if !cleanAbsolute(prlimitPath) || limits.Timeout <= 0 || limits.AddressSpaceBytes == 0 || limits.OutputBytesPerStream <= 0 {
		return nil, ErrInvalid
	}
	return &Runner{prlimit: prlimitPath, limits: limits}, nil
}

func (r *Runner) Run(ctx context.Context, command Command) (Output, error) {
	if r == nil || ctx == nil || !cleanAbsolute(command.Executable) || len(command.Files) > maxExtraFiles {
		return Output{}, ErrInvalid
	}
	for _, file := range command.Files {
		if file == nil {
			return Output{}, ErrInvalid
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()

	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return Output{}, fmt.Errorf("%w: prepare supervisor control", ErrFailed)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	arguments := []string{
		supervisorArgument,
		r.prlimit,
		strconv.FormatUint(r.limits.AddressSpaceBytes, 10),
		strconv.FormatUint(r.limits.FileSizeBytes, 10),
		strconv.Itoa(len(command.Files)),
		command.Executable,
	}
	arguments = append(arguments, command.Arguments...)
	cmd := exec.Command("/proc/self/exe", arguments...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TZ=UTC"}
	cmd.ExtraFiles = append([]*os.File{controlRead}, command.Files...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	exceededSignal := make(chan struct{}, 1)
	stdout := &limitedBuffer{limit: r.limits.OutputBytesPerStream, signal: exceededSignal}
	stderr := &limitedBuffer{limit: r.limits.OutputBytesPerStream, signal: exceededSignal}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return Output{}, fmt.Errorf("%w: start", ErrFailed)
	}
	_ = controlRead.Close()

	requestCleanup := func() { _ = controlWrite.Close() }
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
			return Output{}, fmt.Errorf("%w: supervisor cleanup timeout", ErrFailed)
		}
	case <-exceededSignal:
		requestCleanup()
		var completed bool
		waitErr, completed = waitForSupervisor(waitResult, supervisorReturnGrace)
		if !completed {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			return Output{}, fmt.Errorf("%w: supervisor cleanup timeout", ErrFailed)
		}
	}
	// Classify only after cleanup, with deterministic precedence when terminal
	// conditions became ready together.
	if ctx.Err() != nil {
		return Output{}, ctx.Err()
	}
	if runCtx.Err() != nil {
		return Output{}, ErrTimeout
	}
	if stdout.exceeded.Load() || stderr.exceeded.Load() {
		return Output{}, ErrOutputLimit
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) && exitError.ExitCode() == supervisorFileSizeExitCode {
			return Output{}, ErrFileSizeLimit
		}
		return Output{}, fmt.Errorf("%w: non-zero exit", ErrFailed)
	}
	return Output{Stdout: stdout.bytes(), Stderr: stderr.bytes()}, nil
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// limitedBuffer keeps its bounded prefix while continuing to consume the
// child's pipe. Returning len(p), nil avoids an os/exec pipe-copy race.
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
	if int64(len(p)) > remaining && w.exceeded.CompareAndSwap(false, true) {
		select {
		case w.signal <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (w *limitedBuffer) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
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
