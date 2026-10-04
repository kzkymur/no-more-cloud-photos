//go:build linux

package metadata

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

func TestBoundedRunnerSuccessAndStableFailure(t *testing.T) {
	runner := testRunner(2*time.Second, 1024)
	success := writeExecutable(t, "success", "#!/bin/sh\nprintf 'ok'\nprintf 'warning' >&2\n")
	output, err := runner.run(context.Background(), success, "ignored")
	if err != nil || string(output.stdout) != "ok" || string(output.stderr) != "warning" {
		t.Fatalf("run success = %q/%q, %v", output.stdout, output.stderr, err)
	}
	failure := writeExecutable(t, "failure", "#!/bin/sh\nprintf 'SECRET_PATH=/private/input' >&2\nexit 7\n")
	_, err = runner.run(context.Background(), failure)
	if !errors.Is(err, ErrProbeFailed) || strings.Contains(err.Error(), "SECRET_PATH") || strings.Contains(err.Error(), "/private") {
		t.Fatalf("failure error = %v", err)
	}
}

func TestBoundedRunnerKillsProcessGroupOnTimeout(t *testing.T) {
	runner := testRunner(100*time.Millisecond, 1024)
	script := writeExecutable(t, "timeout", "#!/bin/sh\n(sleep 30) &\nwait\n")
	started := time.Now()
	_, err := runner.run(context.Background(), script)
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("run error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout cleanup took %s", elapsed)
	}
}

func TestBoundedRunnerKillsOnEitherOutputLimit(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			runner := testRunner(2*time.Second, 32)
			redirection := ""
			if stream == "stderr" {
				redirection = " >&2"
			}
			script := writeExecutable(t, stream, "#!/bin/sh\nwhile :; do printf '0123456789abcdef'"+redirection+"; done\n")
			_, err := runner.run(context.Background(), script)
			if !errors.Is(err, ErrProbeOutputLimit) {
				t.Fatalf("run error = %v, want output limit", err)
			}
		})
	}
}

func TestBoundedRunnerHonorsCallerCancellation(t *testing.T) {
	runner := testRunner(2*time.Second, 1024)
	script := writeExecutable(t, "cancel", "#!/bin/sh\nsleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runner.run(ctx, script)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want cancellation", err)
	}
}

func TestBoundedRunnerKillsResidualGroupAfterParentExit(t *testing.T) {
	runner := testRunner(2*time.Second, 1024)
	script := writeExecutable(t, "residual", "#!/bin/sh\nsleep 30 >/dev/null 2>&1 &\nprintf '%s' \"$!\"\nexit 0\n")
	output, err := runner.run(context.Background(), script)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(output.stdout))
	if err != nil {
		t.Fatalf("child PID %q: %v", output.stdout, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if stat, readErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); readErr == nil {
			fields := strings.Fields(string(stat))
			if len(fields) > 2 && fields[2] == "Z" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("residual child %d remains after runner return", pid)
}

func testRunner(timeout time.Duration, outputLimit int64) boundedRunner {
	policy := DefaultPolicy()
	policy.Timeout = timeout
	policy.OutputBytes = outputLimit
	return boundedRunner{prlimit: "/usr/bin/prlimit", policy: policy}
}

func writeExecutable(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
