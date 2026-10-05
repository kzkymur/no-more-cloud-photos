//go:build linux

package processrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunnerValidatesConfigurationAndCommands(t *testing.T) {
	valid := Limits{Timeout: time.Second, AddressSpaceBytes: 1 << 30, OutputBytesPerStream: 1024}
	for _, test := range []struct {
		path   string
		limits Limits
	}{
		{path: "prlimit", limits: valid},
		{path: "/usr/bin/../bin/prlimit", limits: valid},
		{path: "/usr/bin/prlimit", limits: Limits{}},
	} {
		if _, err := New(test.path, test.limits); !errors.Is(err, ErrInvalid) {
			t.Errorf("New(%q, %+v) error = %v", test.path, test.limits, err)
		}
	}
	runner := newTestRunner(t, time.Second, 1024)
	if _, err := runner.Run(context.Background(), Command{Executable: "relative"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative executable error = %v", err)
	}
	files := make([]*os.File, maxExtraFiles+1)
	if _, err := runner.Run(context.Background(), Command{Executable: "/bin/true", Files: files}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("excess file error = %v", err)
	}
}

func TestRunnerUsesArgumentArrayAndReturnsBoundedStreams(t *testing.T) {
	runner := newTestRunner(t, 2*time.Second, 1024)
	script := writeTestExecutable(t, "arguments", "#!/bin/sh\nprintf '%s' \"$1\"\nprintf 'warning' >&2\n")
	argument := "$(printf shell-was-used)"
	output, err := runner.Run(context.Background(), Command{Executable: script, Arguments: []string{argument}})
	if err != nil || string(output.Stdout) != argument || string(output.Stderr) != "warning" {
		t.Fatalf("Run() = %q/%q, %v", output.Stdout, output.Stderr, err)
	}

	failure := writeTestExecutable(t, "failure", "#!/bin/sh\nprintf 'SECRET=/private/input' >&2\nexit 9\n")
	if _, err := runner.Run(context.Background(), Command{Executable: failure}); !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/private") {
		t.Fatalf("safe failure = %v", err)
	}
}

func TestRunnerTimeoutCancellationAndOutputLimit(t *testing.T) {
	sleep := writeTestExecutable(t, "sleep", "#!/bin/sh\nsleep 30\n")
	if _, err := newTestRunner(t, 50*time.Millisecond, 1024).Run(context.Background(), Command{Executable: sleep}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestRunner(t, time.Second, 1024).Run(ctx, Command{Executable: sleep}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	overflow := writeTestExecutable(t, "overflow", "#!/bin/sh\nwhile :; do printf 0123456789abcdef; done\n")
	if _, err := newTestRunner(t, time.Second, 32).Run(context.Background(), Command{Executable: overflow}); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("output-limit error = %v", err)
	}
}

func TestRunnerReapsDoubleForkSessionEscape(t *testing.T) {
	runner := newTestRunner(t, 100*time.Millisecond, 1024)
	directory := t.TempDir()
	topPIDFile := filepath.Join(directory, "top")
	leafPIDFile := filepath.Join(directory, "leaf")
	script := writeTestExecutable(t, "escape", "#!/bin/sh\nsetsid sh -c 'sleep 30 & printf %s \"$!\" > \"$1\"; wait' helper \"$2\" &\nprintf '%s' \"$!\" > \"$1\"\nwait\n")
	if _, err := runner.Run(context.Background(), Command{Executable: script, Arguments: []string{topPIDFile, leafPIDFile}}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("escape timeout error = %v", err)
	}
	assertGone(t, readTestPID(t, topPIDFile))
	assertGone(t, readTestPID(t, leafPIDFile))
}

func newTestRunner(t *testing.T, timeout time.Duration, outputLimit int64) *Runner {
	t.Helper()
	runner, err := New("/usr/bin/prlimit", Limits{Timeout: timeout, AddressSpaceBytes: 1 << 30, OutputBytesPerStream: outputLimit})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func writeTestExecutable(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("PID %q: %v", data, err)
	}
	return pid
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("escaped descendant %d still exists", pid)
}
