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
	report := newID()
	var entered string
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
	if _, err := capability.RepairPool.Exec(ctx, `SELECT nmcp_admin_record_repair_result($1,$2,'succeeded',$3,'actor','host',$4)`,
		report, newID(), newID(), time.Now()); err != nil {
		t.Fatalf("record repair result: %v", err)
	}
	if _, err := capability.RepairPool.Exec(ctx, `SELECT nmcp_admin_record_repair_result($1,$2,'bogus',$3,'actor','host',$4)`,
		report, newID(), newID(), time.Now()); err == nil {
		t.Fatal("record repair result accepted an unknown outcome")
	}
	tx, err := capability.RepairPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	var reason, holder *string
	if err := tx.QueryRow(ctx, `SELECT mode,reason,owner FROM nmcp_admin_lock_maintenance_for_resume()`).Scan(&mode, &reason, &holder); err != nil || mode != "maintenance" {
		t.Fatalf("resume fence = %q, %v", mode, err)
	}
	// An unknown report never authorizes a resume.
	var resumed bool
	if err := tx.QueryRow(ctx, `SELECT nmcp_admin_resume_maintenance($1,'{}'::jsonb,$2,'actor','host',$3)`,
		newID(), newID(), time.Now()).Scan(&resumed); err != nil || resumed {
		t.Fatalf("unauthorized resume = %v, %v", resumed, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var audited string
	if err := owner.Pool.QueryRow(ctx, `SELECT string_agg(command||':'||outcome,',' ORDER BY started_at,command) FROM admin_audit`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audited, "repair_enter:succeeded") || !strings.Contains(audited, "repair_result:succeeded") {
		t.Fatalf("audit = %q", audited)
	}
}
