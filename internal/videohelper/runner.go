//go:build linux

package videohelper

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

type commandRunner interface {
	run(path string, args []string, maxOutput int64) ([]byte, []byte, error)
	stream(path string, args []string, consume func(io.Reader) error) error
}

type osCommandRunner struct{}

type limitedBuffer struct {
	b         bytes.Buffer
	remaining int64
	exceeded  atomic.Bool
	signal    chan<- struct{}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		if b.exceeded.CompareAndSwap(false, true) && b.signal != nil {
			select {
			case b.signal <- struct{}{}:
			default:
			}
		}
		return len(p), nil
	}
	b.remaining -= int64(len(p))
	return b.b.Write(p)
}

func (osCommandRunner) run(path string, args []string, maxOutput int64) ([]byte, []byte, error) {
	exceeded := make(chan struct{}, 1)
	stdout := limitedBuffer{remaining: maxOutput, signal: exceeded}
	stderr := limitedBuffer{remaining: maxOutput, signal: exceeded}
	cmd := exec.Command(path, args...)
	files, err := inheritedDescriptorFiles(args)
	if err != nil {
		return nil, nil, err
	}
	defer closeFiles(files)
	cmd.ExtraFiles = files
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
	case <-exceeded:
		_ = cmd.Process.Kill()
		err = <-wait
	}
	if stdout.exceeded.Load() || stderr.exceeded.Load() {
		return nil, nil, fail("resource_limit")
	}
	return stdout.b.Bytes(), stderr.b.Bytes(), err
}

func (osCommandRunner) stream(path string, args []string, consume func(io.Reader) error) error {
	cmd := exec.Command(path, args...)
	files, err := inheritedDescriptorFiles(args)
	if err != nil {
		return err
	}
	defer closeFiles(files)
	cmd.ExtraFiles = files
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	exceeded := make(chan struct{}, 1)
	stderr := limitedBuffer{remaining: 1 << 20, signal: exceeded}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	consumeErr := consume(stdout)
	if consumeErr != nil {
		_ = stdout.Close()
	}
	if stderr.exceeded.Load() {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if stderr.exceeded.Load() {
		return fail("resource_limit")
	}
	if consumeErr != nil {
		return consumeErr
	}
	if waitErr != nil {
		return errors.New("codec command failed")
	}
	return nil
}

func inheritedDescriptorFiles(args []string) ([]*os.File, error) {
	maximum := 2
	for _, argument := range args {
		if !strings.HasPrefix(argument, "/proc/self/fd/") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(argument, "/proc/self/fd/"))
		if err != nil || n < 3 || n > 5 {
			return nil, fail("policy_violation")
		}
		maximum = max(maximum, n)
	}
	if maximum < 3 {
		return nil, nil
	}
	files := make([]*os.File, 0, maximum-2)
	for descriptor := 3; descriptor <= maximum; descriptor++ {
		duplicate, err := unix.Dup(descriptor)
		if err != nil {
			closeFiles(files)
			return nil, fail("policy_violation")
		}
		files = append(files, os.NewFile(uintptr(duplicate), "inherited-fd-"+strconv.Itoa(descriptor)))
	}
	return files, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}
