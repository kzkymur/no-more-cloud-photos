package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// Issue #58: `restore` (prepare) puts the SOURCE into maintenance owned by
// the operation, and only a verified cutover could ever resume it. A
// validation-only drill (prepare, validate, decide not to cut over) therefore
// stranded production in maintenance. `restore --operation-id OP --abandon`
// is the explicit root operator decision to keep the source: it records
// `abandoned` durably in the ledger (after which no cutover or post-restore
// resume can use the operation), runs a fresh full check as nmcp_check, and
// returns the source to normal in one audited transaction. The candidate
// database is never touched; dropping it stays an operator step.

// restoreAbandonablePhases are the online prepare phases. A prepare that
// failed in one of them (phase "failed" with that failed_phase) is equally
// abandonable: it never reached cutover either.
var restoreAbandonablePhases = map[string]bool{"prepared": true, "maintenance": true, "restored": true, "validated": true}

// restoreLedgerPhases lists every phase ever recorded for the operation.
func restoreLedgerPhases(root, id string) ([]string, error) {
	d, err := privateDirectory(root)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, errRestore
	}
	var phases []string
	for _, name := range names {
		if strings.HasPrefix(name, id+".") && strings.HasSuffix(name, ".json") {
			phases = append(phases, strings.TrimSuffix(strings.TrimPrefix(name, id+"."), ".json"))
		}
	}
	return phases, nil
}

// loadUsableRestoreOperation loads an operation for cutover or the
// post-restore resume. An operation with ANY recorded `abandoned` phase is
// refused, whatever its latest phase by UpdatedAt (review X2: a clock step
// back must never revive an abandoned operation).
func loadUsableRestoreOperation(root, id string) (RestoreOperation, error) {
	op, err := LoadRestoreOperation(root, id)
	if err != nil {
		return op, err
	}
	phases, err := restoreLedgerPhases(root, id)
	if err != nil {
		return RestoreOperation{}, err
	}
	for _, phase := range phases {
		if phase == "abandoned" {
			return RestoreOperation{}, errRestoreAbandoned
		}
	}
	return op, nil
}

var errRestoreAbandoned = errors.New("restore operation was abandoned")

// restoreAbandonAuthorized decides from the ledger alone: never any cutover
// phase, never an offline operation, and the latest phase is a prepare phase,
// a prepare failure, or an earlier abandon (an interrupted abandon resumes).
func restoreAbandonAuthorized(op RestoreOperation, phases []string) bool {
	if op.SourceOffline {
		return false
	}
	for _, phase := range phases {
		if strings.HasPrefix(phase, "cutover") || strings.HasPrefix(phase, "offline") {
			return false
		}
	}
	switch {
	case restoreAbandonablePhases[op.Phase], op.Phase == "abandoned":
		return true
	case op.Phase == "failed":
		return restoreAbandonablePhases[op.FailedPhase]
	}
	return false
}

// RestoreAbandon returns the restore source to normal service after an
// operator decides not to cut over. s.Pool must be the SOURCE's owner
// connection and s.CheckPool the nmcp_check pool pinned to it.
func (s *Service) RestoreAbandon(ctx context.Context, id string, c RestoreSettings) (Maintenance, error) {
	if s.Pool == nil || s.CheckPool == nil || !uuidPattern.MatchString(id) || c.LedgerRoot == "" || c.StorageRoot == "" {
		return Maintenance{}, ErrValidation
	}
	op, err := LoadRestoreOperation(c.LedgerRoot, id)
	if err != nil {
		return Maintenance{}, ErrConflict
	}
	phases, err := restoreLedgerPhases(c.LedgerRoot, id)
	if err != nil || !restoreAbandonAuthorized(op, phases) {
		return Maintenance{}, ErrConflict
	}
	// Only the source may be returned this way: never the candidate, never an
	// unrelated database that happens to share the credential.
	identity, err := restoreIdentity(ctx, s.Pool)
	if err != nil {
		return Maintenance{}, err
	}
	if identity != op.Source || identity == op.Candidate {
		return Maintenance{}, ErrConflict
	}
	unlock, err := s.sessionLock(ctx, "maintenance")
	if err != nil {
		return Maintenance{}, err
	}
	defer unlock()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Maintenance{}, err
	}
	defer rollback(tx)
	var mode string
	var reason, owner *string
	if err := tx.QueryRow(ctx, `SELECT mode,reason,owner FROM maintenance_state WHERE id=1 FOR UPDATE NOWAIT`).Scan(&mode, &reason, &owner); err != nil {
		return Maintenance{}, classified(err)
	}
	inRestore := mode == "maintenance" && reason != nil && *reason == "operator restore" && owner != nil && *owner == op.ID
	// A prepare that failed before entering maintenance left the source in
	// normal mode; any other maintenance (another operation, a repair) is not
	// this operation's to end.
	if !inRestore && mode != "normal" {
		return Maintenance{}, ErrConflict
	}
	// Durable decision first: from here on the operation can never be cut
	// over or resumed as a restore, even if what follows is interrupted.
	if op.Phase != "abandoned" {
		op.Phase, op.UpdatedAt = "abandoned", time.Now()
		if err := SaveRestoreOperation(c.LedgerRoot, op); err != nil {
			return Maintenance{}, err
		}
	}
	if !inRestore {
		rollback(tx)
		return s.MaintenanceStatus(ctx)
	}
	store, err := storage.Open(c.StorageRoot, storage.Options{})
	if err != nil {
		return Maintenance{}, err
	}
	defer store.Close()
	started := time.Now()
	check, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if err != nil {
		return Maintenance{}, err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_check_reports r
		JOIN reconciliation_check_report_seals seal ON seal.report_id=r.id
		WHERE r.id=$1 AND r.scope='all' AND seal.finding_count=0
		AND (SELECT count(*) FROM reconciliation_check_source_results src WHERE src.report_id=r.id AND src.result='complete')=3)
		AND NOT EXISTS(SELECT 1 FROM reconciliation_repair_runs run WHERE NOT EXISTS
		(SELECT 1 FROM reconciliation_repair_results result WHERE result.run_id=run.id))`, check.ReportID).Scan(&valid); err != nil {
		return Maintenance{}, err
	}
	if !valid {
		return Maintenance{}, ErrConflict
	}
	encoded, _ := json.Marshal(check)
	if _, err := tx.Exec(ctx, `UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL,
		successful_check_report=$1,updated_at=clock_timestamp() WHERE id=1`, encoded); err != nil {
		return Maintenance{}, err
	}
	if err := s.audit(ctx, tx, "restore_abandon", map[string]any{"operation_id": op.ID, "report_id": check.ReportID},
		[]string{op.ID, check.ReportID}, "succeeded", started); err != nil {
		return Maintenance{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Maintenance{}, err
	}
	return s.MaintenanceStatus(ctx)
}

// restoreAbandonSettings reads only what abandon uses from the restore
// environment: the ledger and the storage root it checks.
func restoreAbandonSettings() (RestoreSettings, error) {
	c := RestoreSettings{LedgerRoot: os.Getenv("NMCP_RESTORE_LEDGER_ROOT"), StorageRoot: os.Getenv("NMCP_STORAGE_ROOT")}
	for _, path := range []string{c.LedgerRoot, c.StorageRoot} {
		if !strings.HasPrefix(path, "/") || filepath.Clean(path) != path {
			return c, ErrValidation
		}
	}
	return c, nil
}
