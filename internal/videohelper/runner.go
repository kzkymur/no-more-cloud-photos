//go:build linux

package videohelper

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
)

type commandRunner interface {
	run(path string, args []string, maxOutput int64) ([]byte, []byte, error)
	stream(path string, args []string, consume func(io.Reader) error) error
}

type osCommandRunner struct{}

type limitedBuffer struct {
	b         bytes.Buffer
	remaining int64
	exceeded  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		b.exceeded = true
		return len(p), nil
	}
	b.remaining -= int64(len(p))
	return b.b.Write(p)
}

func (osCommandRunner) run(path string, args []string, maxOutput int64) ([]byte, []byte, error) {
	stdout := limitedBuffer{remaining: maxOutput}
	stderr := limitedBuffer{remaining: maxOutput}
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, nil, fail("resource_limit")
	}
	return stdout.b.Bytes(), stderr.b.Bytes(), err
}

func (osCommandRunner) stream(path string, args []string, consume func(io.Reader) error) error {
	cmd := exec.Command(path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := limitedBuffer{remaining: 1 << 20}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	consumeErr := consume(stdout)
	if consumeErr != nil {
		_ = stdout.Close()
	}
	waitErr := cmd.Wait()
	if stderr.exceeded {
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
