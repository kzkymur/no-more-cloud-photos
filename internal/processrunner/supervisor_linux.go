//go:build linux

package processrunner

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
	supervisorArgument         = "__nmcp_internal_process_supervisor_v1"
	supervisorCleanupLimit     = time.Second
	supervisorFileSizeExitCode = 122
)

// The re-executed Core binary is a dedicated, short-lived subreaper. The
// long-lived API or Worker therefore never adopts unrelated descendants.
func init() {
	if len(os.Args) > 1 && os.Args[1] == supervisorArgument {
		os.Exit(runSupervisor(os.Args[2:]))
	}
}

func runSupervisor(arguments []string) int {
	if len(arguments) < 5 {
		return 125
	}
	prlimitPath, addressLimit, fileSizeLimit, fileCountText, executable := arguments[0], arguments[1], arguments[2], arguments[3], arguments[4]
	fileCount, err := strconv.Atoi(fileCountText)
	if err != nil || fileCount < 0 || fileCount > maxExtraFiles || !filepath.IsAbs(prlimitPath) || !filepath.IsAbs(executable) {
		return 125
	}
	fileSizeBytes, err := strconv.ParseUint(fileSizeLimit, 10, 64)
	if err != nil {
		return 125
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 125
	}
	control := os.NewFile(3, "process-supervisor-control")
	if control == nil {
		return 125
	}
	defer control.Close()
	files := make([]*os.File, 0, fileCount)
	for index := 0; index < fileCount; index++ {
		file := os.NewFile(uintptr(4+index), "process-input")
		if file == nil {
			return 125
		}
		defer file.Close()
		files = append(files, file)
	}

	toolArguments := []string{"--as=" + addressLimit}
	if fileSizeBytes > 0 {
		toolArguments = append(toolArguments, "--fsize="+fileSizeLimit)
	}
	toolArguments = append(toolArguments, "--", executable)
	toolArguments = append(toolArguments, arguments[5:]...)
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
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			if status, ok := exitError.Sys().(syscall.WaitStatus); ok &&
				(status.Signal() == syscall.SIGXFSZ || status.ExitStatus() == 128+int(syscall.SIGXFSZ)) {
				return supervisorFileSizeExitCode
			}
		}
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
