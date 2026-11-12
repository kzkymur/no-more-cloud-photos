//go:build nmcp_failpoints

package failpoint

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
)

// EnvName selects at most one point per process: NAME or NAME@N, where N is a
// positive decimal hit ordinal (default 1).
const EnvName = "NMCP_FAILPOINT"

// pointNames is the only place point names exist. The array length ties it to
// the enum; TestPointNamesComplete rejects gaps and duplicates.
var pointNames = [pointLimit]string{
	UploadBeforeOriginalPublish: "upload.before_original_publish",
	UploadAfterOriginalPublish:  "upload.after_original_publish",
	UploadAfterDBCommit:         "upload.after_db_commit",
	RenditionBeforeFilePublish:  "rendition.before_file_publish",
	RenditionAfterFilePublish:   "rendition.after_file_publish",
	RenditionAfterDBCommit:      "rendition.after_db_commit",
	PurgeAfterStart:             "purge.after_start",
	PurgeAfterFileDelete:        "purge.after_file_delete",
	PurgeAfterFileStep:          "purge.after_file_step",
	PurgeBeforeFinalize:         "purge.before_finalize",
	PurgeAfterFinalize:          "purge.after_finalize",
}

// Name returns the stable dotted name of point, or "" for an unknown value.
func Name(point Point) string {
	if point <= pointNone || point >= pointLimit {
		return ""
	}
	return pointNames[point]
}

type spec struct {
	point Point
	nth   uint64
}

type injector struct {
	point Point
	nth   uint64
	hits  atomic.Uint64
	fire  func(Point)
}

func (i *injector) hit(point Point) {
	if point != i.point {
		return
	}
	// Exactly one caller observes the Nth increment, however many goroutines
	// race on the same point.
	if i.hits.Add(1) == i.nth {
		i.fire(point)
	}
}

// active is written once by init before any other goroutine exists and is
// read-only afterwards.
var active *injector

func init() {
	value, ok := os.LookupEnv(EnvName)
	if !ok || value == "" {
		return
	}
	parsed, err := parse(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nmcp-failpoint: invalid %s: %v\n", EnvName, err)
		os.Exit(2)
	}
	active = &injector{point: parsed.point, nth: parsed.nth, fire: kill}
	fmt.Fprintf(os.Stderr, "nmcp-failpoint armed %s@%d\n", Name(parsed.point), parsed.nth)
}

// Hit kills the process with SIGKILL when point is the armed point and this is
// its configured hit ordinal. Otherwise it only counts the hit.
func Hit(point Point) {
	if active != nil {
		active.hit(point)
	}
}

// parse accepts exactly NAME or NAME@N with a known NAME and N in [1, 2^64).
func parse(value string) (spec, error) {
	name, ordinal := value, ""
	for index := 0; index < len(value); index++ {
		if value[index] == '@' {
			name, ordinal = value[:index], value[index+1:]
			if ordinal == "" {
				return spec{}, fmt.Errorf("empty hit ordinal in %q", value)
			}
			break
		}
	}
	point := pointNone
	for candidate := pointNone + 1; candidate < pointLimit; candidate++ {
		if pointNames[candidate] == name {
			point = candidate
			break
		}
	}
	if point == pointNone {
		return spec{}, fmt.Errorf("unknown failpoint %q", name)
	}
	if ordinal == "" {
		return spec{point: point, nth: 1}, nil
	}
	if ordinal[0] == '0' {
		return spec{}, fmt.Errorf("hit ordinal %q must be a positive decimal without leading zeros", ordinal)
	}
	var nth uint64
	for index := 0; index < len(ordinal); index++ {
		digit := ordinal[index]
		if digit < '0' || digit > '9' {
			return spec{}, fmt.Errorf("hit ordinal %q is not decimal", ordinal)
		}
		if nth > (^uint64(0)-uint64(digit-'0'))/10 {
			return spec{}, fmt.Errorf("hit ordinal %q overflows", ordinal)
		}
		nth = nth*10 + uint64(digit-'0')
	}
	return spec{point: point, nth: nth}, nil
}

// kill simulates power loss or an OOM kill: one unbuffered stderr line, no
// fsync, no deferred cleanup and no graceful shutdown.
func kill(point Point) {
	_, _ = os.Stderr.WriteString("nmcp-failpoint fired " + Name(point) + "\n")
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// SIGKILL to the own thread group is fatal before the syscall returns to
	// user space, except for the init process of a PID namespace (a container
	// entrypoint), which the kernel shields from its own signals. Then end the
	// process with exit_group directly: still no deferred cleanup, flush or
	// graceful shutdown, and the 128+SIGKILL status an external kill reports.
	syscall.Exit(137)
}
