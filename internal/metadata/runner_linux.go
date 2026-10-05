//go:build linux

package metadata

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
)

type boundedRunner struct {
	prlimit string
	policy  Policy
}

type commandOutput struct {
	stdout []byte
	stderr []byte
}

func (r boundedRunner) run(ctx context.Context, executable string, arguments ...string) (commandOutput, error) {
	return r.runWithFiles(ctx, executable, nil, arguments...)
}

func (r boundedRunner) runWithFiles(ctx context.Context, executable string, files []*os.File, arguments ...string) (commandOutput, error) {
	runner, err := processrunner.New(r.prlimit, processrunner.Limits{
		Timeout: r.policy.Timeout, AddressSpaceBytes: r.policy.AddressSpaceBytes, OutputBytesPerStream: r.policy.OutputBytes,
	})
	if err != nil {
		return commandOutput{}, fmt.Errorf("%w: configure runner", ErrProbeFailed)
	}
	output, err := runner.Run(ctx, processrunner.Command{Executable: executable, Arguments: arguments, Files: files})
	switch {
	case err == nil:
		return commandOutput{stdout: output.Stdout, stderr: output.Stderr}, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return commandOutput{}, err
	case errors.Is(err, processrunner.ErrTimeout):
		return commandOutput{}, ErrProbeTimeout
	case errors.Is(err, processrunner.ErrOutputLimit):
		return commandOutput{}, ErrProbeOutputLimit
	default:
		return commandOutput{}, fmt.Errorf("%w: process runner", ErrProbeFailed)
	}
}
