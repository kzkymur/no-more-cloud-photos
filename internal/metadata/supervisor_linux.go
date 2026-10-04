//go:build linux

package metadata

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	probeSupervisorArgument = "__nmcp_internal_probe_supervisor_v1"
	supervisorCleanupLimit  = time.Second
)

// A short-lived copy of the current Core executable is the dedicated Linux
// subreaper. The long-lived API/Worker process never adopts unrelated children.
func init() {
	if len(os.Args) > 1 && os.Args[1] == probeSupervisorArgument {
		os.Exit(runProbeSupervisor(os.Args[2:]))
	}
}

func runProbeSupervisor(arguments []string) int {
	if len(arguments) < 4 {
		return 125
	}
	prlimitPath, addressLimit, fileCountText, executable := arguments[0], arguments[1], arguments[2], arguments[3]
	fileCount, err := strconv.Atoi(fileCountText)
	if err != nil || fileCount < 0 || fileCount > 8 || !filepath.IsAbs(prlimitPath) || !filepath.IsAbs(executable) {
		return 125
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 125
	}
	control := os.NewFile(3, "probe-supervisor-control")
	if control == nil {
		return 125
	}
	defer control.Close()
	files := make([]*os.File, 0, fileCount)
	for index := 0; index < fileCount; index++ {
		file := os.NewFile(uintptr(4+index), "probe-input")
		if file == nil {
			return 125
		}
		defer file.Close()
		files = append(files, file)
	}

	toolArguments := append([]string{"--as=" + addressLimit, "--", executable}, arguments[4:]...)
	command := exec.Command(prlimitPath, toolArguments...)
	command.Env = []string{"LC_ALL=C", "LANG=C", "TZ=UTC"}
	command.ExtraFiles = files
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return 125
	}

	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	controlResult := make(chan struct{}, 1)
	go func() {
		var one [1]byte
		_, _ = io.ReadFull(control, one[:])
		controlResult <- struct{}{}
	}()

	var waitErr error
	select {
	case waitErr = <-waitResult:
	case <-controlResult:
		_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		waitErr = <-waitResult
	}
	_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
	if !killAndReapAdoptedChildren(supervisorCleanupLimit) {
		return 125
	}
	if waitErr != nil {
		return 1
	}
	return 0
}

func killAndReapAdoptedChildren(limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	childrenPath := "/proc/self/task/" + strconv.Itoa(os.Getpid()) + "/children"
	for time.Now().Before(deadline) {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.ECHILD) {
				return true
			}
			if err != nil || pid == 0 {
				break
			}
		}
		contents, err := os.ReadFile(childrenPath)
		if err != nil {
			return false
		}
		for _, field := range strings.Fields(string(contents)) {
			pid, err := strconv.Atoi(field)
			if err == nil && pid > 0 {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
