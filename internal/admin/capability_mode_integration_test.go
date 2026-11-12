package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// capabilityIntegrationServices returns the migrated owner service and a
// capability-mode service whose pools are throwaway logins holding only
// nmcp_check_runtime / nmcp_repair_runtime, like nmcp_check / nmcp_repair.
func capabilityIntegrationServices(t *testing.T, ctx context.Context) (*Service, *Service) {
	t.Helper()
	owner := integrationService(t)
	var schema string
	if err := owner.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(newID(), "-", "")[:10]
	checkLogin, repairLogin := "t_cchk_"+suffix, "t_crep_"+suffix
	ident := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	for _, statement := range []string{
		"CREATE ROLE " + ident(checkLogin) + " LOGIN INHERIT",
		"CREATE ROLE " + ident(repairLogin) + " LOGIN INHERIT",
		"GRANT nmcp_check_runtime TO " + ident(checkLogin) + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
		"GRANT nmcp_repair_runtime TO " + ident(repairLogin) + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
	} {
		if _, err := owner.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{checkLogin, repairLogin} {
			_, _ = owner.Pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident(name))
		}
	})
	rolePool := func(role string) *pgxpool.Pool {
		t.Helper()
		config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		config.ConnConfig.RuntimeParams["search_path"] = schema + ",pg_catalog,pg_temp"
		config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET SESSION AUTHORIZATION "+ident(role))
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	capability := New(nil)
	capability.capability = true
	capability.CheckPool, capability.RepairPool = rolePool(checkLogin), rolePool(repairLogin)

	return owner, capability
}

// TestAdminIntegrationCapabilityRepairAndResume runs repair and maintenance
// resume exactly as a uid-nmcp capability process does (review B2): no owner
// pool, the check on a login holding only nmcp_check_runtime, every
// owner-table step through the 0025 definers on a login holding only
// nmcp_repair_runtime.
func TestAdminIntegrationCapabilityRepairAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, capability := capabilityIntegrationServices(t, ctx)
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := newID()
	orphan := filepath.Join(root, "originals", id[:2], id, "original.jpg")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, err := capability.Check(ctx, store, reconciliation.ScopeAll)
	if !errors.Is(err, ErrConflict) || !checked.Sealed || checked.FindingCount != 1 {
		t.Fatalf("capability check=%+v err=%v", checked, err)
	}
	if _, err := capability.Repair(ctx, store, checked.ReportID, false); err != nil {
		t.Fatalf("capability repair: %v", err)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capability repair left the orphan in place")
	}
	state, err := capability.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "maintenance" || state.Reason == nil || *state.Reason != "administrative repair" {
		t.Fatalf("maintenance after repair = %+v, %v", state, err)
	}
	resumed, err := capability.MaintenanceResume(ctx, store)
	if err != nil {
		t.Fatalf("capability resume: %v", err)
	}
	if after, ok := resumed.(Maintenance); !ok || after.Mode != "normal" || len(after.SuccessfulCheck) == 0 {
		t.Fatalf("resume result = %+v", resumed)
	}
	var audited string
	if err := owner.Pool.QueryRow(ctx, `SELECT string_agg(command||':'||outcome,',' ORDER BY started_at,command) FROM admin_audit`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repair_enter:succeeded", "repair_result:succeeded", "maintenance_resume:succeeded"} {
		if !strings.Contains(audited, want) {
			t.Fatalf("audit %q lacks %s", audited, want)
		}
	}
	// The capability boundary itself: the check login cannot enter
	// maintenance, and the repair login cannot touch maintenance_state.
	if _, err := capability.CheckPool.Exec(ctx, `SELECT nmcp_admin_repair_enter_maintenance($1,$2,'x','y',clock_timestamp())`, newID(), newID()); err == nil {
		t.Fatal("check login executed the repair maintenance definer")
	}
	if _, err := capability.RepairPool.Exec(ctx, `UPDATE maintenance_state SET mode='normal'`); err == nil {
		t.Fatal("repair login updated maintenance_state directly")
	}
	// A post-restore maintenance needs the root operator procedure.
	if _, err := owner.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='operator restore',owner='op',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := capability.MaintenanceResume(ctx, store); !errors.Is(err, ErrConflict) {
		t.Fatalf("capability resume of a restore maintenance: %v", err)
	}
}

// TestAdminIntegrationCapabilityDefinerBoundary exercises every 0025 definer
// on the capability logins without a storage scan: the check login reads
// migration history only, the repair login enters maintenance, records a
// result and fences/resumes only through the definers, and neither touches
// the owner tables directly.
func TestAdminIntegrationCapabilityDefinerBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, capability := capabilityIntegrationServices(t, ctx)
	for _, pool := range []*pgxpool.Pool{capability.CheckPool, capability.RepairPool} {
		migrator, err := database.NewMigrator(pool)
		if err != nil {
			t.Fatal(err)
		}
		status, err := migrator.CapabilityStatus(ctx)
		if err != nil || !status.Ready() {
			t.Fatalf("capability migration status = %+v, %v", status, err)
		}
	}
	if _, err := capability.CheckPool.Exec(ctx, `SELECT * FROM nmcp_admin_read_maintenance_state()`); err == nil {
		t.Fatal("check login read maintenance through the repair definer")
	}
	for _, statement := range []string{
		`SELECT * FROM schema_migrations`, `SELECT * FROM maintenance_state`,
		`INSERT INTO admin_audit DEFAULT VALUES`, `UPDATE maintenance_state SET mode='normal'`,
	} {
		for _, pool := range []*pgxpool.Pool{capability.CheckPool, capability.RepairPool} {
			if _, err := pool.Exec(ctx, statement); err == nil {
				t.Fatalf("capability login ran %s directly", statement)
			}
		}
	}
	// Review K1: every definer validates its subject itself. A report that does
	// not exist, or is unsealed, never enters maintenance.
	var entered string
	for name, report := range map[string]string{"unknown": newID(), "unsealed": syntheticReport(t, ctx, owner, false, 0, true)} {
		if err := capability.RepairPool.QueryRow(ctx, `SELECT nmcp_admin_repair_enter_maintenance($1,$2,'actor','host',$3)`,
			report, newID(), time.Now()).Scan(&entered); err == nil {
			t.Fatalf("%s report entered maintenance: %q", name, entered)
		}
	}
	if state, err := capability.MaintenanceStatus(ctx); err != nil || state.Mode != "normal" {
		t.Fatalf("refused enter changed maintenance: %+v %v", state, err)
	}
	report := syntheticReport(t, ctx, owner, true, 1, true)
	if err := capability.RepairPool.QueryRow(ctx, `SELECT nmcp_admin_repair_enter_maintenance($1,$2,'actor','host',$3)`,
		report, newID(), time.Now()).Scan(&entered); err != nil || entered != "entered" {
		t.Fatalf("enter maintenance = %q, %v", entered, err)
	}
	state, err := capability.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "maintenance" || state.Reason == nil || *state.Reason != "administrative repair" ||
		state.Owner == nil || *state.Owner != "actor@host" {
		t.Fatalf("maintenance = %+v, %v", state, err)
	}
	if err := capability.RepairPool.QueryRow(ctx, `SELECT nmcp_admin_repair_enter_maintenance($1,$2,'actor','host',$3)`,
		report, newID(), time.Now()).Scan(&entered); err != nil || entered != "already" {
		t.Fatalf("re-enter maintenance = %q, %v", entered, err)
	}
	// The result audit needs an executed run of this very report.
	run := newID()
	if _, err := capability.RepairPool.Exec(ctx, `SELECT nmcp_admin_record_repair_result($1,$2,'succeeded',$3,'actor','host',$4)`,
		report, run, newID(), time.Now()); err == nil {
		t.Fatal("record repair result accepted a run that does not exist")
	}
	if _, err := owner.Pool.Exec(ctx, `INSERT INTO reconciliation_repair_runs(id,source_report_id,authorization_id,authorized_actor) VALUES($1,$2,$3,'actor')`,
		run, report, newID()); err != nil {
		t.Fatal(err)
	}
	if _, err := capability.RepairPool.Exec(ctx, `SELECT nmcp_admin_record_repair_result($1,$2,'succeeded',$3,'actor','host',$4)`,
		report, run, newID(), time.Now()); err == nil {
		t.Fatal("record repair result accepted a run that has not executed")
	}
	if _, err := capability.RepairPool.Exec(ctx, `SELECT nmcp_admin_record_repair_result($1,$2,'bogus',$3,'actor','host',$4)`,
		report, run, newID(), time.Now()); err == nil {
		t.Fatal("record repair result accepted an unknown outcome")
	}
	// The positive result audit (an executed run of this report) is proven
	// end to end by TestAdminIntegrationCapabilityRepairAndResume.
	// An unfinished repair run blocks resume even on a fresh clean report.
	fresh := syntheticReport(t, ctx, owner, true, 0, true)
	if resumed, err := capabilityResumeGate(t, ctx, capability, fresh, `{"ReportID":"`+fresh+`"}`); err != nil || resumed {
		t.Fatalf("resume with an unfinished repair run = %v, %v", resumed, err)
	}
	var audited string
	if err := owner.Pool.QueryRow(ctx, `SELECT string_agg(command||':'||outcome,',' ORDER BY started_at,command) FROM admin_audit`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audited, "repair_enter:succeeded") || strings.Contains(audited, "repair_result") || strings.Contains(audited, "maintenance_resume") {
		t.Fatalf("audit = %q", audited)
	}
}

// syntheticReport inserts a full-scope check report on the current schema
// version without a storage scan: sealed or not, with findingCount findings
// counted in its seal, and every source complete (or storage_scan errored).
func syntheticReport(t *testing.T, ctx context.Context, owner *Service, sealed bool, findingCount int, complete bool) string {
	t.Helper()
	id := newID()
	if _, err := owner.Pool.Exec(ctx, `INSERT INTO reconciliation_check_reports
		(id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
		db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at)
		SELECT $1,1,1,'all',m.version,m.name,m.checksum,t.at,t.at,t.at,t.at,t.at-interval '48 hours'
		FROM schema_migrations m, (SELECT clock_timestamp() AS at) t ORDER BY m.version DESC LIMIT 1`, id); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"database_references", "attempt_owners", "storage_scan"} {
		result, code := "complete", any(nil)
		if !complete && source == "storage_scan" {
			result, code = "error", "storage_scan"
		}
		if _, err := owner.Pool.Exec(ctx, `INSERT INTO reconciliation_check_source_results(report_id,source,result,error_code) VALUES($1,$2,$3,$4)`,
			id, source, result, code); err != nil {
			t.Fatal(err)
		}
	}
	if sealed {
		if _, err := owner.Pool.Exec(ctx, `INSERT INTO reconciliation_check_report_seals(report_id,finding_count,fs_scan_ended_at) VALUES($1,$2,clock_timestamp())`,
			id, findingCount); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// capabilityResumeGate runs the resume fence and transition definers in one
// repair-login transaction, as capabilityResume does, and rolls it back.
func capabilityResumeGate(t *testing.T, ctx context.Context, capability *Service, report, checkResult string) (bool, error) {
	t.Helper()
	tx, err := capability.RepairPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var mode string
	var reason, holder *string
	if err := tx.QueryRow(ctx, `SELECT mode,reason,owner FROM nmcp_admin_lock_maintenance_for_resume()`).Scan(&mode, &reason, &holder); err != nil {
		return false, err
	}
	var resumed bool
	err = tx.QueryRow(ctx, `SELECT nmcp_admin_resume_maintenance($1,$2::jsonb,$3,'actor','host',$4)`,
		report, checkResult, newID(), time.Now()).Scan(&resumed)
	if err == nil && resumed {
		err = tx.Commit(ctx)
	}
	return resumed, err
}

// TestAdminIntegrationCapabilityResumeGate pins review K1: the resume definer
// is the database-side gate. It refuses a post-restore maintenance and
// resumes only on a fresh (created after maintenance was entered), sealed,
// finding-free full report that the caller's check result names.
func TestAdminIntegrationCapabilityResumeGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, capability := capabilityIntegrationServices(t, ctx)
	stale := syntheticReport(t, ctx, owner, true, 0, true)
	report := syntheticReport(t, ctx, owner, true, 1, true)
	var entered string
	if err := capability.RepairPool.QueryRow(ctx, `SELECT nmcp_admin_repair_enter_maintenance($1,$2,'actor','host',$3)`,
		report, newID(), time.Now()).Scan(&entered); err != nil || entered != "entered" {
		t.Fatalf("enter maintenance = %q, %v", entered, err)
	}
	fresh := syntheticReport(t, ctx, owner, true, 0, true)
	unsealed := syntheticReport(t, ctx, owner, false, 0, true)
	errored := syntheticReport(t, ctx, owner, true, 0, false)
	for name, call := range map[string][2]string{
		"unknown report":                 {newID(), `{"ReportID":"x"}`},
		"report older than maintenance":  {stale, `{"ReportID":"` + stale + `"}`},
		"check result naming another":    {fresh, `{"ReportID":"` + stale + `"}`},
		"check result without report id": {fresh, `{}`},
		"report with findings":           {report, `{"ReportID":"` + report + `"}`},
		"unsealed report":                {unsealed, `{"ReportID":"` + unsealed + `"}`},
		"report with a source error":     {errored, `{"ReportID":"` + errored + `"}`},
	} {
		if resumed, err := capabilityResumeGate(t, ctx, capability, call[0], call[1]); err != nil || resumed {
			t.Fatalf("resume of %s = %v, %v", name, resumed, err)
		}
	}
	if _, err := owner.Pool.Exec(ctx, `UPDATE maintenance_state SET reason='operator restore' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if resumed, err := capabilityResumeGate(t, ctx, capability, fresh, `{"ReportID":"`+fresh+`"}`); err == nil || resumed {
		t.Fatalf("capability resume of a restore maintenance = %v, %v", resumed, err)
	}
	if _, err := owner.Pool.Exec(ctx, `UPDATE maintenance_state SET reason='administrative repair' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if resumed, err := capabilityResumeGate(t, ctx, capability, fresh, `{"ReportID":"`+fresh+`"}`); err != nil || !resumed {
		t.Fatalf("resume on a fresh clean report = %v, %v", resumed, err)
	}
	state, err := capability.MaintenanceStatus(ctx)
	if err != nil || state.Mode != "normal" {
		t.Fatalf("after resume: %+v %v", state, err)
	}
}
