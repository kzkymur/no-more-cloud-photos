package admin

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestRestoreAbandonLedgerRule pins issue #58's authorization: only a never
// cut over online operation (a prepare phase, a prepare failure, or an
// interrupted abandon) may be abandoned.
func TestRestoreAbandonLedgerRule(t *testing.T) {
	for name, test := range map[string]struct {
		phase, failed string
		history       []string
		offline, want bool
	}{
		"prepared":               {phase: "prepared", history: []string{"prepared"}, want: true},
		"validated":              {phase: "validated", history: []string{"prepared", "maintenance", "restored", "validated"}, want: true},
		"prepare failed":         {phase: "failed", failed: "restored", history: []string{"prepared", "maintenance", "failed"}, want: true},
		"interrupted abandon":    {phase: "abandoned", history: []string{"prepared", "validated", "abandoned"}, want: true},
		"cutover failed":         {phase: "cutover_failed", failed: "cutover_stopped", history: []string{"validated", "cutover_authorized", "cutover_failed"}},
		"cutover verified":       {phase: "cutover_verified", history: []string{"validated", "cutover_verified"}},
		"abandon after cutover":  {phase: "abandoned", history: []string{"validated", "cutover_authorized", "abandoned"}},
		"failed with cutover":    {phase: "failed", failed: "cutover_preflight", history: []string{"validated", "failed"}},
		"offline operation":      {phase: "validated", history: []string{"validated"}, offline: true},
		"offline phase recorded": {phase: "validated", history: []string{"offline_fence_authorized", "validated"}},
		"unknown phase":          {phase: "something", history: []string{"something"}},
	} {
		t.Run(name, func(t *testing.T) {
			op := RestoreOperation{Phase: test.phase, FailedPhase: test.failed, SourceOffline: test.offline}
			if got := restoreAbandonAuthorized(op, test.history); got != test.want {
				t.Fatalf("authorized = %v, want %v", got, test.want)
			}
		})
	}
}

// TestRestoreAbandonCLIShape: abandon needs exactly an operation ID, never
// with --cutover, --backup-id or offline flags, and only as root.
func TestRestoreAbandonCLIShape(t *testing.T) {
	op := newID()
	if o, err := parse([]string{"restore", "--operation-id", op, "--abandon", "--yes", "--json"}); err != nil || !o.abandon || o.operationID != op {
		t.Fatalf("abandon parse = %+v, %v", o, err)
	}
	for _, args := range [][]string{
		{"restore", "--abandon", "--yes"},
		{"restore", "--operation-id", op, "--abandon", "--cutover", "--yes"},
		{"restore", "--operation-id", op, "--backup-id", newID(), "--abandon", "--yes"},
		{"restore", "--operation-id", op, "--abandon", "--offline-source", "--yes"},
	} {
		if _, err := parse(args); !errors.Is(err, ErrValidation) {
			t.Fatalf("%v accepted: %v", args, err)
		}
	}
	previous := euid
	euid = func() int { return 1000 }
	t.Cleanup(func() { euid = previous })
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("NMCP_ADMIN_DATABASE_URL_FILE", "")
	t.Setenv("NMCP_DATABASE_URL", "postgres://nmcp_migrator:x@127.0.0.1:1/nmcp?sslmode=disable&connect_timeout=1")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"restore", "--operation-id", op, "--abandon", "--yes", "--json"}, &stdout, &stderr); code != 2 {
		t.Fatalf("non-root abandon exit %d, want 2 (stderr %s)", code, stderr.String())
	}
}

// TestAdminIntegrationRestoreAbandonRefusesWithoutTouchingTheSource covers the
// database-backed refusals: an unknown operation, a cut-over operation, and
// a live identity that is not the operation's source all leave maintenance
// and the ledger exactly as they were.
func TestAdminIntegrationRestoreAbandonRefusesWithoutTouchingTheSource(t *testing.T) {
	s := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.CheckPool = s.Pool
	live, err := restoreIdentity(ctx, s.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ledger := privateTestDir(t)
	settings := RestoreSettings{LedgerRoot: ledger, StorageRoot: privateTestDir(t)}
	save := func(op RestoreOperation) {
		t.Helper()
		op.UpdatedAt = time.Now()
		if err := SaveRestoreOperation(ledger, op); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.RestoreAbandon(ctx, newID(), settings); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown operation: %v", err)
	}
	cut := restoreTestOperation()
	cut.Source = live
	cut.Phase = "validated"
	save(cut)
	cut.Phase = "cutover_authorized"
	save(cut)
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='operator restore',owner=$1,entered_at=clock_timestamp() WHERE id=1`, cut.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreAbandon(ctx, cut.ID, settings); !errors.Is(err, ErrConflict) {
		t.Fatalf("abandon after a cutover phase: %v", err)
	}
	other := restoreTestOperation() // its source is not this database
	other.Phase = "validated"
	save(other)
	if _, err := s.RestoreAbandon(ctx, other.ID, settings); !errors.Is(err, ErrConflict) {
		t.Fatalf("abandon whose source is not the live database: %v", err)
	}
	// A validated operation of THIS source while another operation owns the
	// maintenance is not this operation's to end.
	mine := restoreTestOperation()
	mine.Source = live
	mine.Phase = "validated"
	save(mine)
	if _, err := s.RestoreAbandon(ctx, mine.ID, settings); !errors.Is(err, ErrConflict) {
		t.Fatalf("abandon of a maintenance owned by another operation: %v", err)
	}
	state, err := s.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != cut.ID {
		t.Fatalf("refusals changed maintenance: %+v %v", state, err)
	}
	for _, id := range []string{cut.ID, other.ID, mine.ID} {
		if op, err := LoadRestoreOperation(ledger, id); err != nil || op.Phase == "abandoned" {
			t.Fatalf("refusal recorded abandoned for %s: %+v %v", id, op, err)
		}
	}
}
