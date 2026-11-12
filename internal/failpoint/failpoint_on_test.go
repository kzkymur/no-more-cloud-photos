//go:build nmcp_failpoints

package failpoint

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

const childModeEnv = "NMCP_FAILPOINT_TEST_CHILD"

func TestPointNamesComplete(t *testing.T) {
	seen := map[string]Point{}
	for point := pointNone + 1; point < pointLimit; point++ {
		name := Name(point)
		if name == "" {
			t.Fatalf("point %d has no name", point)
		}
		if previous, duplicate := seen[name]; duplicate {
			t.Fatalf("points %d and %d share name %q", previous, point, name)
		}
		seen[name] = point
		dot := strings.IndexByte(name, '.')
		if dot <= 0 || dot == len(name)-1 || strings.Count(name, ".") != 1 || strings.ContainsAny(name, "@ \t\n") {
			t.Fatalf("point name %q is not a stable dotted name", name)
		}
	}
	if Name(pointNone) != "" || Name(pointLimit) != "" {
		t.Fatal("out-of-range points must have no name")
	}
}

func TestParse(t *testing.T) {
	valid := []struct {
		value string
		want  spec
	}{
		{"upload.after_original_publish", spec{UploadAfterOriginalPublish, 1}},
		{"upload.after_original_publish@1", spec{UploadAfterOriginalPublish, 1}},
		{"purge.after_file_step@3", spec{PurgeAfterFileStep, 3}},
		{"rendition.after_db_commit@18446744073709551615", spec{RenditionAfterDBCommit, 1<<64 - 1}},
	}
	for point := pointNone + 1; point < pointLimit; point++ {
		valid = append(valid, struct {
			value string
			want  spec
		}{Name(point), spec{point, 1}})
	}
	for _, test := range valid {
		got, err := parse(test.value)
		if err != nil || got != test.want {
			t.Errorf("parse(%q) = %+v, %v; want %+v", test.value, got, err, test.want)
		}
	}
	invalid := []string{
		"unknown.point", "upload", "UPLOAD.AFTER_DB_COMMIT", " upload.after_db_commit", "upload.after_db_commit ",
		"upload.after_db_commit@", "upload.after_db_commit@0", "upload.after_db_commit@01", "upload.after_db_commit@-1",
		"upload.after_db_commit@+1", "upload.after_db_commit@1x", "upload.after_db_commit@1@2", "upload.after_db_commit@ 1",
		"upload.after_db_commit@18446744073709551616", "@1", "upload.after_db_commit,purge.after_start",
	}
	for _, value := range invalid {
		if got, err := parse(value); err == nil {
			t.Errorf("parse(%q) = %+v, want error", value, got)
		}
	}
}

func TestHitCountsOnlyArmedPointAndFiresOnceAtNth(t *testing.T) {
	var fired atomic.Int64
	var firedPoint atomic.Uint32
	instance := &injector{point: PurgeAfterFileStep, nth: 500, fire: func(point Point) {
		fired.Add(1)
		firedPoint.Store(uint32(point))
	}}
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				instance.hit(PurgeAfterFileDelete)
				instance.hit(PurgeAfterFileStep)
			}
		}()
	}
	wait.Wait()
	if got := instance.hits.Load(); got != 1600 {
		t.Fatalf("armed point hits = %d, want 1600", got)
	}
	if got := fired.Load(); got != 1 {
		t.Fatalf("fired %d times, want exactly once", got)
	}
	if Point(firedPoint.Load()) != PurgeAfterFileStep {
		t.Fatalf("fired point %d, want %d", firedPoint.Load(), PurgeAfterFileStep)
	}
}

func TestHitDisarmedIsNoop(t *testing.T) {
	saved := active
	active = nil
	t.Cleanup(func() { active = saved })
	for point := pointNone; point <= pointLimit; point++ {
		Hit(point)
	}
}

// TestFiredPointKillsProcess re-executes this test binary with the point
// armed at its third hit and requires a SIGKILL death after exactly two
// survived hits plus the single stderr line.
func TestFiredPointKillsProcess(t *testing.T) {
	if os.Getenv(childModeEnv) == "fire" {
		for index := 1; ; index++ {
			Hit(PurgeAfterStart) // never armed in this child: must not count
			Hit(PurgeAfterFileStep)
			_, _ = os.Stdout.WriteString("survived " + strconv.Itoa(index) + "\n")
			if index > 5 {
				os.Exit(0)
			}
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestFiredPointKillsProcess$", "-test.count=1")
	command.Env = append(withoutFailpointEnv(), childModeEnv+"=fire", EnvName+"=purge.after_file_step@3")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child error = %v, want SIGKILL exit; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child status = %v, want killed by SIGKILL; stdout=%q stderr=%q", exitErr, stdout.String(), stderr.String())
	}
	if got, want := stdout.String(), "survived 1\nsurvived 2\n"; got != want {
		t.Fatalf("child stdout = %q, want %q", got, want)
	}
	wantStderr := "nmcp-failpoint armed purge.after_file_step@3\nnmcp-failpoint fired purge.after_file_step\n"
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("child stderr = %q, want %q", got, wantStderr)
	}
}

// TestUnknownPointExitsTwoAtStartup proves the strict parse runs in package
// initialization: the child exits 2 before any test function runs.
func TestUnknownPointExitsTwoAtStartup(t *testing.T) {
	for _, value := range []string{"upload.after_everything", "upload.after_db_commit@0"} {
		command := exec.Command(os.Args[0], "-test.run=^TestUnknownPointChildMustNotRun$", "-test.count=1")
		command.Env = append(withoutFailpointEnv(), childModeEnv+"=startup", EnvName+"="+value)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("%s=%q: child error = %v, want exit 2; stdout=%q stderr=%q", EnvName, value, err, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "nmcp-failpoint: invalid "+EnvName+": ") ||
			strings.Count(stderr.String(), "\n") != 1 {
			t.Fatalf("%s=%q: stdout=%q stderr=%q", EnvName, value, stdout.String(), stderr.String())
		}
	}
}

func TestUnknownPointChildMustNotRun(t *testing.T) {
	if os.Getenv(childModeEnv) == "startup" {
		_, _ = os.Stdout.WriteString("test body ran\n")
	}
}

func withoutFailpointEnv() []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, EnvName+"=") && !strings.HasPrefix(entry, childModeEnv+"=") {
			environment = append(environment, entry)
		}
	}
	return environment
}
