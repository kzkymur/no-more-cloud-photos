package admin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// drainManager is an in-memory unit-state machine behind the drain's
// injectable systemctl seam. It never executes /usr/bin/systemctl.
type drainManager struct {
	mu       sync.Mutex
	state    map[string]string
	calls    []string
	failStop map[string]bool // stop command fails and the unit stays active
	hang     map[string]bool // stop is queued but the unit never deactivates
}

func newDrainManager(active ...string) *drainManager {
	m := &drainManager{state: map[string]string{}, failStop: map[string]bool{}, hang: map[string]bool{}}
	for _, u := range active {
		m.state[u] = "active"
	}
	return m
}
func (m *drainManager) run(ctx context.Context, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return "", errCutover
	}
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("unbounded systemctl call")
	}
	m.calls = append(m.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "stop":
		unit := args[len(args)-1]
		if m.failStop[unit] {
			return "", errors.New("raw manager text: password=hunter2")
		}
		if !m.hang[unit] {
			m.state[unit] = "inactive"
		} else {
			m.state[unit] = "deactivating"
		}
		return "", nil
	case len(args) == 4 && args[0] == "show" && args[1] == "--property=ActiveState" && args[2] == "--value":
		if s, ok := m.state[args[3]]; ok {
			return s, nil
		}
		return "inactive", nil
	case len(args) >= 1 && args[0] == "start":
		m.state[args[len(args)-1]] = "active"
		return "", nil
	}
	return "", errors.New("unexpected systemctl verb")
}
func (m *drainManager) snapshot() ([]string, map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := map[string]string{}
	for k, v := range m.state {
		state[k] = v
	}
	return append([]string{}, m.calls...), state
}
func drainSystem(m *drainManager, offline *RestoreOperation) *systemCutover {
	// backupPGUnit is deliberately empty: validateLoadedBackupUnit would fail,
	// and Stop would return before stopping anything.
	return &systemCutover{offline: offline, drainSystemctl: m.run, drainRuntimeAbsent: func(context.Context) error { return nil }, drainPoll: time.Millisecond}
}
func drainOfflineOperation() *RestoreOperation {
	op := cutoverTestOperation()
	op.SourceOffline = true
	op.OfflineAuthority = &OfflineRestoreAuthority{SourceUnit: "postgresql@17-old.service", CandidateUnit: "postgresql@17-new.service"}
	return &op
}
func drainErrorLabels(err error) []string {
	var out []string
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			out = append(out, e.Error())
		}
	} else if err != nil {
		out = append(out, err.Error())
	}
	return out
}

func TestEmergencyDrainStopsEveryUnitWithoutValidation(t *testing.T) {
	m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service")
	if err := drainSystem(m, nil).EmergencyDrain(context.Background()); err != nil {
		t.Fatalf("clean drain reported %v", err)
	}
	calls, state := m.snapshot()
	want := []string{"stop nmcp-backup.timer", "stop --no-block nmcp-backup.service", "stop --no-block nmcp-api.service", "stop --no-block nmcp-worker.service"}
	if !reflect.DeepEqual(calls[:4], want) {
		t.Fatalf("timer-first independent stops: %q", calls)
	}
	for _, c := range calls[4:] {
		if !strings.HasPrefix(c, "show --property=ActiveState --value ") {
			t.Fatalf("drain issued non-stop/show command %q", c)
		}
	}
	for unit, s := range state {
		if s != "inactive" {
			t.Fatalf("%s left %s", unit, s)
		}
	}
	if (&systemCutover{}).Stop(context.Background()) == nil {
		t.Fatal("normal Stop lost its validation precondition")
	}
}

func TestEmergencyDrainNeverShortCircuits(t *testing.T) {
	for _, failing := range []string{"nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service"} {
		t.Run(failing, func(t *testing.T) {
			m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service")
			m.failStop[failing] = true
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := drainSystem(m, nil).EmergencyDrain(ctx)
			_, state := m.snapshot()
			for unit, s := range state {
				if unit != failing && s != "inactive" {
					t.Fatalf("%s not stopped after %s failed", unit, failing)
				}
			}
			labels := strings.Join(drainErrorLabels(err), "|")
			if !strings.Contains(labels, "emergency drain: stop "+failing) || !strings.Contains(labels, "emergency drain: still active "+failing) {
				t.Fatalf("failure not reported: %q", labels)
			}
			if strings.Contains(labels, "hunter2") || strings.Contains(labels, "raw manager") {
				t.Fatal("raw systemctl text leaked")
			}
		})
	}
}

func TestEmergencyDrainBoundedWhenUnitsHangAndAfterCancellation(t *testing.T) {
	m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service")
	m.hang["nmcp-api.service"] = true
	m.hang["nmcp-worker.service"] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // even an already-cancelled context must attempt every stop
	started := time.Now()
	a := drainSystem(m, nil)
	runtimeChecked := false
	a.drainRuntimeAbsent = func(c context.Context) error { runtimeChecked = c.Err() == nil; return errCutover }
	err := a.EmergencyDrain(ctx)
	if time.Since(started) > 5*time.Second {
		t.Fatal("drain wait unbounded")
	}
	calls, state := m.snapshot()
	for _, unit := range []string{"nmcp-backup.timer", "nmcp-backup.service"} {
		if state[unit] != "inactive" {
			t.Fatalf("%s not stopped with cancelled caller context", unit)
		}
	}
	if len(calls) < 4 {
		t.Fatalf("stops not attempted: %q", calls)
	}
	labels := drainErrorLabels(err)
	want := []string{"emergency drain: still active nmcp-api.service", "emergency drain: still active nmcp-worker.service", "emergency drain: backup runtime directory still present"}
	if !reflect.DeepEqual(labels, want) || !runtimeChecked {
		t.Fatalf("labels %q runtime=%v", labels, runtimeChecked)
	}
}

func TestEmergencyDrainOfflineOrderingAndCandidateUntouched(t *testing.T) {
	op := drainOfflineOperation()
	m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service", "postgresql@17-old.service", "postgresql@17-new.service")
	if err := drainSystem(m, op).EmergencyDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, state := m.snapshot()
	want := []string{"stop nmcp-backup.timer", "stop --no-block nmcp-backup.service", "stop --no-block nmcp-api.service", "stop --no-block nmcp-worker.service", "stop --no-block postgresql@17-old.service"}
	if !reflect.DeepEqual(calls[:5], want) {
		t.Fatalf("offline drain order %q", calls)
	}
	if state["postgresql@17-old.service"] != "inactive" || state["postgresql@17-new.service"] != "active" {
		t.Fatal("offline source not drained or candidate stopped")
	}
	for _, c := range calls {
		if strings.Contains(c, "postgresql@17-new.service") {
			t.Fatal("candidate cluster touched")
		}
	}
	// Missing/unsafe authority: still stop every fixed unit, report the gap.
	for _, change := range []func(*RestoreOperation){
		func(op *RestoreOperation) { op.OfflineAuthority = nil },
		func(op *RestoreOperation) { op.OfflineAuthority.SourceUnit = op.OfflineAuthority.CandidateUnit },
		func(op *RestoreOperation) { op.OfflineAuthority.SourceUnit = "evil.service" },
	} {
		bad := drainOfflineOperation()
		change(bad)
		m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service", "postgresql@17-new.service")
		err := drainSystem(m, bad).EmergencyDrain(context.Background())
		calls, state := m.snapshot()
		if !reflect.DeepEqual(drainErrorLabels(err), []string{"emergency drain: offline source authority unavailable"}) {
			t.Fatalf("authority gap not reported: %v", err)
		}
		if state["nmcp-api.service"] != "inactive" || state["nmcp-worker.service"] != "inactive" || state["postgresql@17-new.service"] != "active" {
			t.Fatal("fixed units not drained or candidate stopped")
		}
		for _, c := range calls {
			if strings.Contains(c, "evil.service") || strings.Contains(c, "postgresql@17-new.service") {
				t.Fatal("unauthenticated unit targeted")
			}
		}
	}
}

// partialStartCutover drives runCutover with fake steps, but a real
// systemCutover.EmergencyDrain over the same in-memory manager.
type partialStartCutover struct {
	fakeCutover
	m     *drainManager
	drain *systemCutover
}

func (p *partialStartCutover) Start(context.Context) error {
	p.calls = append(p.calls, "start")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = p.m.run(ctx, "start", "nmcp-api.service")
	_, _ = p.m.run(ctx, "start", "nmcp-worker.service")
	if p.fail == "start" {
		return errors.New("worker never became ready: credential text")
	}
	return nil
}
func (p *partialStartCutover) EmergencyDrain(ctx context.Context) error {
	p.calls = append(p.calls, "drain")
	return p.drain.EmergencyDrain(ctx)
}

func TestCutoverPartialStartFailureDrainsAllAndRecordsEvidence(t *testing.T) {
	for _, offline := range []bool{false, true} {
		root := privateTestDir(t)
		op := cutoverTestOperation()
		var off *RestoreOperation
		if offline {
			off = drainOfflineOperation()
		}
		m := newDrainManager("nmcp-backup.timer")
		m.failStop["nmcp-worker.service"] = true
		a := &partialStartCutover{fakeCutover: fakeCutover{fail: "start"}, m: m, drain: drainSystem(m, off)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // caller cancellation must not suppress the drain
		out, err := runCutover(ctx, root, op, &boundedDrain{partialStartCutover: a, bound: 50 * time.Millisecond}, SaveRestoreOperation)
		if !errors.Is(err, errCutover) || out.Phase != "cutover_failed" || out.FailedPhase != "cutover_started" {
			t.Fatalf("phase=%s failed=%s err=%v", out.Phase, out.FailedPhase, err)
		}
		_, state := m.snapshot()
		if state["nmcp-api.service"] != "inactive" || state["nmcp-backup.timer"] != "inactive" {
			t.Fatal("partial start left a unit running")
		}
		if offline && state["postgresql@17-old.service"] != "inactive" {
			t.Fatal("offline source not drained")
		}
		want := []string{"emergency drain: stop nmcp-worker.service", "emergency drain: still active nmcp-worker.service"}
		if !reflect.DeepEqual(out.DrainErrors, want) {
			t.Fatalf("drain evidence %q", out.DrainErrors)
		}
		if strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "emergency drain: stop nmcp-worker.service") {
			t.Fatalf("unsanitized or missing drain error: %q", err)
		}
		loaded, e := LoadRestoreOperation(root, op.ID)
		if e != nil || loaded.Phase != "cutover_failed" || !reflect.DeepEqual(loaded.DrainErrors, want) {
			t.Fatal("drain evidence not durable", e)
		}
		calls := len(a.calls)
		if _, e := runCutover(context.Background(), root, loaded, &boundedDrain{partialStartCutover: a, bound: 50 * time.Millisecond}, SaveRestoreOperation); e == nil || len(a.calls) != calls {
			t.Fatal("failed cutover resumed")
		}
	}
}

// Failing validation in Stop (the original early-return) still drains everything.
type failingStopCutover struct{ partialStartCutover }

func (f *failingStopCutover) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return errCutover // backup unit validation failure
}
func TestCutoverValidationFailureInStopStillDrains(t *testing.T) {
	m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service")
	a := &failingStopCutover{partialStartCutover{m: m, drain: drainSystem(m, nil)}}
	out, err := runCutover(context.Background(), privateTestDir(t), cutoverTestOperation(), a, SaveRestoreOperation)
	if !errors.Is(err, errCutover) || out.FailedPhase != "cutover_stopped" || len(out.DrainErrors) != 0 {
		t.Fatalf("failed=%s drain=%q err=%v", out.FailedPhase, out.DrainErrors, err)
	}
	if !reflect.DeepEqual(a.calls, []string{"preflight", "stop", "drain"}) {
		t.Fatal(a.calls)
	}
	_, state := m.snapshot()
	for unit, s := range state {
		if s != "inactive" {
			t.Fatalf("%s left %s", unit, s)
		}
	}
}

func TestCutoverPhasePersistenceFailureDrains(t *testing.T) {
	for _, phase := range []string{"cutover_preflight", "cutover_stopped", "cutover_switched", "cutover_targets_verified", "cutover_started", "cutover_verified"} {
		t.Run(phase, func(t *testing.T) {
			m := newDrainManager("nmcp-backup.timer", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service")
			m.hang["nmcp-backup.service"] = true
			a := &partialStartCutover{m: m, drain: drainSystem(m, nil)}
			a.drain.drainRuntimeAbsent = func(context.Context) error { return nil }
			save := func(_ string, op RestoreOperation) error {
				if op.Phase == phase {
					return errors.New("fsync")
				}
				return nil
			}
			// Bound the drain's wait through the hung unit using a short test drain context.
			short := &boundedDrain{partialStartCutover: a, bound: 20 * time.Millisecond}
			out, err := runCutover(context.Background(), "unused", cutoverTestOperation(), short, save)
			if !errors.Is(err, errCutover) || a.calls[len(a.calls)-1] != "drain" {
				t.Fatalf("persist failure did not drain: %q %v", a.calls, err)
			}
			_, state := m.snapshot()
			if state["nmcp-backup.timer"] != "inactive" || state["nmcp-api.service"] != "inactive" || state["nmcp-worker.service"] != "inactive" {
				t.Fatal("phase-persist failure left units running")
			}
			if !reflect.DeepEqual(out.DrainErrors, []string{"emergency drain: still active nmcp-backup.service"}) || !strings.Contains(err.Error(), "still active nmcp-backup.service") {
				t.Fatalf("drain evidence %q", out.DrainErrors)
			}
		})
	}
}

type boundedDrain struct {
	*partialStartCutover
	bound time.Duration
}

func (b *boundedDrain) EmergencyDrain(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		return errors.New("drain context not independent")
	}
	c, cancel := context.WithTimeout(ctx, b.bound)
	defer cancel()
	return b.partialStartCutover.EmergencyDrain(c)
}

func TestCutoverDrainErrorSanitization(t *testing.T) {
	var op RestoreOperation
	err := cutoverFailure(&op, errors.Join(newCutoverDrainError("stop", "nmcp-api.service"), errors.New("PGPASSWORD=secret"), newCutoverDrainError("stop", "evil\nunit.service"), newCutoverDrainError("stop", "postgresql@17-old.service")))
	want := []string{"emergency drain: stop nmcp-api.service", "emergency drain: unclassified failure", "emergency drain: stop offline-source-unit", "emergency drain: stop postgresql@17-old.service"}
	if !reflect.DeepEqual(op.DrainErrors, want) || !errors.Is(err, errCutover) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "evil") {
		t.Fatalf("%q %q", op.DrainErrors, err)
	}
	op = RestoreOperation{}
	if cutoverFailure(&op, nil) != errCutover || op.DrainErrors != nil {
		t.Fatal("clean drain changed error/evidence")
	}
}
