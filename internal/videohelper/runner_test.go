//go:build linux

package videohelper

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStreamFailsFastOnStderrOverflowAndCleansDescendant(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := t.TempDir() + "/descendant.pid"
	t.Setenv("VIDEOHELPER_RUNNER_CHILD", "codec")
	t.Setenv("VIDEOHELPER_RUNNER_DESCENDANT_PID", pidFile)

	result := make(chan error, 1)
	go func() {
		result <- (osCommandRunner{}).stream(executable, []string{"-test.run=^TestStreamCodecChild$"}, func(stdout io.Reader) error {
			_, err := io.Copy(io.Discard, stdout)
			return err
		})
	}()

	select {
	case err := <-result:
		if !isCode(err, "resource_limit") {
			t.Fatalf("stream error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not terminate promptly after stderr overflow")
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			break
		}
		if err != nil {
			t.Fatalf("check descendant: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived stream cleanup", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamFirstCauseArbitratesConsumerAndStderrOverflow(t *testing.T) {
	consumerFailure := fail("unsupported_input")
	for _, test := range []struct {
		name          string
		consumerFirst bool
		wantCode      string
	}{
		{name: "consumer first then stderr overflow", consumerFirst: true, wantCode: "unsupported_input"},
		{name: "stderr overflow first then consumer", wantCode: "resource_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var first firstStreamFailure
			stderr := limitedBuffer{remaining: 4, onExceeded: func() { first.record(fail("resource_limit")) }}
			firstRecorded := make(chan struct{})
			secondRecorded := make(chan struct{})
			recordConsumer := func(done chan<- struct{}) {
				first.record(consumerFailure)
				close(done)
			}
			recordOverflow := func(done chan<- struct{}) {
				if _, err := stderr.Write([]byte("12345")); err != nil {
					t.Errorf("stderr write: %v", err)
				}
				close(done)
			}
			if test.consumerFirst {
				go recordConsumer(firstRecorded)
				<-firstRecorded
				go recordOverflow(secondRecorded)
			} else {
				go recordOverflow(firstRecorded)
				<-firstRecorded
				go recordConsumer(secondRecorded)
			}
			<-secondRecorded
			if !isCode(first.err, test.wantCode) {
				t.Fatalf("first error = %v, want %s", first.err, test.wantCode)
			}
			if !stderr.exceeded.Load() || stderr.b.Len() != 0 {
				t.Fatalf("stderr cap state: exceeded=%v length=%d", stderr.exceeded.Load(), stderr.b.Len())
			}
		})
	}
}

func TestStreamCodecChild(t *testing.T) {
	switch os.Getenv("VIDEOHELPER_RUNNER_CHILD") {
	case "":
		return
	case "descendant":
		for {
			time.Sleep(time.Hour)
		}
	case "codec":
	default:
		os.Exit(2)
	}

	executable, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	descendant := exec.Command(executable, "-test.run=^TestStreamCodecChild$")
	descendant.Env = []string{"VIDEOHELPER_RUNNER_CHILD=descendant"}
	descendant.Stdout = os.Stdout
	descendant.Stderr = os.Stderr
	if err := descendant.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("VIDEOHELPER_RUNNER_DESCENDANT_PID"), []byte(strconv.Itoa(descendant.Process.Pid)), 0o600); err != nil {
		os.Exit(2)
	}
	chunk := make([]byte, 64<<10)
	for i := 0; i < 17; i++ {
		if _, err := os.Stderr.Write(chunk); err != nil {
			os.Exit(2)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}
