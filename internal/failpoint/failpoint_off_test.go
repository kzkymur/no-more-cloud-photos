//go:build !nmcp_failpoints

package failpoint

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestOffBuildIgnoresEnvironment re-executes this untagged test binary with
// NMCP_FAILPOINT set to a real point, an ordinal and garbage. The child must
// start, hit every point repeatedly and exit 0 without any stderr output.
func TestOffBuildIgnoresEnvironment(t *testing.T) {
	const childEnv = "NMCP_FAILPOINT_TEST_CHILD"
	if os.Getenv(childEnv) == "off" {
		for range 3 {
			for point := pointNone; point <= pointLimit; point++ {
				Hit(point)
			}
		}
		_, _ = os.Stdout.WriteString("off child survived\n")
		return
	}
	for _, value := range []string{"upload.after_original_publish", "purge.after_file_step@1", "not-a-point@0"} {
		command := exec.Command(os.Args[0], "-test.run=^TestOffBuildIgnoresEnvironment$", "-test.count=1", "-test.v")
		environment := []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "NMCP_FAILPOINT") {
				environment = append(environment, entry)
			}
		}
		command.Env = append(environment, childEnv+"=off", "NMCP_FAILPOINT="+value)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("NMCP_FAILPOINT=%q: child error = %v; stdout=%q stderr=%q", value, err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "off child survived\n") || strings.Contains(stdout.String()+stderr.String(), "nmcp-failpoint") {
			t.Fatalf("NMCP_FAILPOINT=%q: stdout=%q stderr=%q", value, stdout.String(), stderr.String())
		}
	}
}
