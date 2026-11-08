package admin

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Offline promotion never reconnects to the old cluster. Its immutable external
// fence remains a precondition at every stage, while target catalog/backend
// witnesses replace the unavailable source observer.
func restoreOfflineCutover(ctx context.Context, op RestoreOperation, c RestoreSettings) (RestoreOperation, error) {
	if os.Geteuid() != 0 || !c.OfflineSource || !validOfflineOperation(op) || op.Phase != "offline_validated" {
		return op, ErrValidation
	}
	authority, err := readOfflineAuthority(c.OfflineAuthorityFile)
	if err != nil || !sameOfflineAuthority(&authority, op.OfflineAuthority) || VerifyOfflineSourceFence(ctx, op) != nil {
		return op, errCutover
	}
	connections, err := resolveRestoreConnections(c)
	if err != nil {
		return op, errCutover
	}
	defer connections.Close()
	cfg := connections.operator.ConnConfig
	expected := op.OfflineAuthority.CandidateEndpoint
	if cfg.Host != expected.SocketDirectory || cfg.Port != expected.Port || cfg.Database != op.Candidate.DatabaseName {
		return op, errCutover
	}
	target, err := pgxpool.NewWithConfig(ctx, connections.operator.Copy())
	if err != nil {
		return op, errCutover
	}
	defer target.Close()
	id, err := restoreIdentity(ctx, target)
	if err != nil || id != op.Candidate {
		return op, errCutover
	}
	graph, err := restoreRoleGraph(ctx, target)
	if err != nil || graph != op.RoleGraphSHA256 {
		return op, errCutover
	}
	state, err := New(target).MaintenanceStatus(ctx)
	if err != nil || state.Mode != "maintenance" || state.Owner == nil || *state.Owner != op.ID {
		return op, errCutover
	}
	unlock, err := New(target).sessionLock(ctx, "maintenance")
	if err != nil {
		return op, errCutover
	}
	defer unlock()
	actions := &systemCutover{target: target, settings: c, offline: &op}
	return runCutover(ctx, c.LedgerRoot, op, actions, SaveRestoreOperation)
}

// A source-cluster dependency must not restart the stopped source as a side
// effect of starting the API. Only the fixed installed deployment drop-ins can
// be retargeted; missing/unexpected effective dependencies fail closed.
func (a *systemCutover) offlineDependencies(ctx context.Context) error {
	if a.offline == nil {
		return nil
	}
	auth := a.offline.OfflineAuthority
	if auth == nil || a.backupPGUnit != auth.SourceUnit || auth.SourceUnit == auth.CandidateUnit {
		return errCutover
	}
	sourceBody, err := backupPGDependencyBody(auth.SourceUnit)
	if err != nil {
		return err
	}
	candidateBody, err := backupPGDependencyBody(auth.CandidateUnit)
	if err != nil {
		return err
	}
	staged := []cutoverFile{}
	for _, item := range []struct{ unit, path string }{{"nmcp-migrate.service", migratePGDependencyDropin}, {"nmcp-backup.service", backupPGDependencyDropin}} {
		reload, err := systemctl(ctx, "show", "--property=NeedDaemonReload", "--value", item.unit)
		if err != nil || reload != "no" {
			return errCutover
		}
		for _, property := range []string{"Requires", "After"} {
			value, err := systemctl(ctx, "show", "--property="+property, "--value", item.unit)
			if err != nil {
				return errCutover
			}
			raw, err := json.Marshal(strings.Fields(value))
			if err != nil || !validateBackupUnitDependencySet(raw, auth.SourceUnit) {
				return errCutover
			}
		}
		file, err := safeCutoverFile(item.path, false, true)
		if err != nil || string(file.before) != sourceBody {
			return errCutover
		}
		file.after = []byte(candidateBody)
		staged = append(staged, file)
	}
	a.files = append(a.files, staged...)
	return nil
}
func containsUnit(value, unit string) bool {
	for _, part := range strings.Fields(value) {
		if part == unit {
			return true
		}
	}
	return false
}
func (a *systemCutover) verifyNoSourceDependencies(ctx context.Context) error {
	if a.offline == nil {
		return nil
	}
	for _, unit := range []string{"nmcp-migrate.service", "nmcp-backup.service", "nmcp-api.service", "nmcp-worker.service"} {
		for _, property := range []string{"Requires", "Wants", "BindsTo"} {
			value, err := systemctl(ctx, "show", "--property="+property, "--value", unit)
			if err != nil || containsUnit(value, a.offline.OfflineAuthority.SourceUnit) {
				return errCutover
			}
		}
	}
	return VerifyOfflineSourceFence(ctx, *a.offline)
}
