//go:build linux

package database

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// Requires a disposable PostgreSQL 17 database, migration-owner credentials,
// and the preprovisioned isolated NOLOGIN capability/definer roles. The usual
// integrationPool provides schema isolation; this test never starts a server.
func TestBackupCheckerCompatibilityPG17(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; no PostgreSQL 17 execution proof")
	}
	for _, mode := range []string{"fresh", "upgrade21"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			pool := integrationPool(t, url)
			var version int
			if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version/10000 != 17 {
				t.Fatalf("requires actual PostgreSQL 17, got %d", version)
			}
			full, err := NewMigrator(pool)
			if err != nil {
				t.Fatal(err)
			}
			if full.migrations[len(full.migrations)-1].version != 23 {
				t.Fatal("compatibility test requires explicit latest version 23")
			}
			prefix := func(v int64) {
				t.Helper()
				n := 0
				for n < len(full.migrations) && full.migrations[n].version <= v {
					n++
				}
				if err := newMigrator(pool, full.migrations[:n]).Up(ctx); err != nil {
					t.Fatal(err)
				}
			}
			schema := schemaForPool(t, ctx, pool)
			root := t.TempDir()
			store, err := storage.Open(root, storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var historical, report22 string
			if mode == "upgrade21" {
				prefix(21)
				historical = backupCompatCheck(t, ctx, url, schema, pool, 21)
				backupCompatPlan(t, ctx, url, schema, pool, store, historical, 21)
				prefix(22)
				report22 = backupCompatCheck(t, ctx, url, schema, pool, 22)
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, `SET LOCAL ROLE nmcp_repair_runtime`); err != nil {
					t.Fatal(err)
				}
				var count int
				err = tx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_repairable_findings($1,0,1)`, report22).Scan(&count)
				backupCompatRejected(t, err)
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				prefix(22)
			}
			before := backupCompatEvidence(t, ctx, pool)
			boundary := backupCompatBoundary(t, ctx, pool)
			prefix(23)
			if got := backupCompatEvidence(t, ctx, pool); got != before {
				t.Fatal("forward migration changed sealed/source/finding/repair evidence or old migration history")
			}
			if got := backupCompatBoundary(t, ctx, pool); got != boundary {
				t.Fatalf("forward migration changed reader identity/security/search_path/ACL: before=%s after=%s", boundary, got)
			}
			assertCheckerDefinerCatalog(t, ctx, pool)
			if historical != "" {
				// Replay must reuse the same durable old plan, including its old schema snapshot.
				backupCompatPlan(t, ctx, url, schema, pool, store, historical, 21)
				backupCompatPlan(t, ctx, url, schema, pool, store, report22, 22)
			}
			latest := backupCompatCheck(t, ctx, url, schema, pool, 23)
			backupCompatPlan(t, ctx, url, schema, pool, store, latest, 23)
			for v := int64(17); v <= 23; v++ {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				clone := backupCompatClone(t, ctx, tx, latest, v, nil, true, false)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				backupCompatPlan(t, ctx, url, schema, pool, store, clone, v)
			}
			backupCompatNegatives(t, ctx, pool, latest)
		})
	}
}

// This database-scoped fixture deliberately supplies a deterministic scan input.
// It proves real checker classification/publication and PostgreSQL repair admission,
// NOT the Store scanner, filesystem durability, or repair filesystem execution.
type backupCompatScanner struct{}

func (s backupCompatScanner) ScanWithQuarantine(ctx context.Context, _ []string) ([]storage.Observation, error) {
	// Plans are prepared but never executed: no quarantine destinations exist.
	return s.Scan(ctx)
}

func (backupCompatScanner) Scan(context.Context) ([]storage.Observation, error) {
	now := time.Now().UTC()
	size := int64(20)
	hash := sha256.Sum256([]byte("compatibility orphan"))
	return []storage.Observation{{RelativeKey: "originals/12/12345678-1234-4123-8123-123456789abc/original.png", Type: storage.ObservationRegular, Size: &size, SHA256: &hash, ModifiedAt: &now, ChangedAt: &now, ObservedAt: now, Stable: true, LinkCount: 1}}, nil
}

func backupCompatCheck(t *testing.T, ctx context.Context, url, schema string, pool *pgxpool.Pool, want int64) string {
	t.Helper()
	checkPool := integrationRolePool(t, url, schema, "nmcp_check_runtime")
	repo, err := reconciliation.NewPostgresRepository(checkPool)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := reconciliation.NewChecker(repo, backupCompatScanner{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := checker.Check(ctx, reconciliation.ScopeAll)
	if err != nil || !result.Sealed || result.FindingCount != 1 {
		t.Fatalf("actual checker=%+v err=%v", result, err)
	}
	var v int64
	if err := pool.QueryRow(ctx, `SELECT migration_version FROM reconciliation_check_reports WHERE id=$1`, result.ReportID).Scan(&v); err != nil || v != want {
		t.Fatalf("checker version=%d want=%d err=%v", v, want, err)
	}
	return result.ReportID
}

func backupCompatPlan(t *testing.T, ctx context.Context, url, schema string, pool *pgxpool.Pool, store *storage.Store, report string, want int64) {
	t.Helper()
	repairPool := integrationRolePool(t, url, schema, "nmcp_repair_runtime")
	repo, err := reconciliation.NewRepairPostgresRepository(repairPool)
	if err != nil {
		t.Fatal(err)
	}
	repairer, err := reconciliation.NewRepairer(repo, store)
	if err != nil {
		t.Fatal(err)
	}
	// Stable authorization ID makes upgrade/replay assertions exercise the old seal.
	request := reconciliation.RepairPlanRequest{ReportID: report, AuthorizationID: report, Actor: "compatibility-test"}
	plan, err := repairer.Plan(ctx, request)
	if err != nil {
		t.Fatalf("report v%d repair plan: %v", want, err)
	}
	again, err := repairer.Plan(ctx, request)
	if err != nil || again.Run.RunID != plan.Run.RunID {
		t.Fatalf("durable plan replay=%+v err=%v", again, err)
	}
	var v int64
	var exact bool
	if err := pool.QueryRow(ctx, `SELECT s.report_migration_version,
  s.report_format_version=r.format_version AND s.report_classifier_version=r.classifier_version
  AND s.report_migration_name=r.migration_name AND s.report_migration_checksum=r.migration_checksum
  AND s.item_count=1 AND s.plan_digest=nmcp_read_repair_plan_digest(s.run_id)
  FROM reconciliation_repair_plan_seals s JOIN reconciliation_check_reports r ON r.id=s.source_report_id WHERE s.run_id=$1`, plan.Run.RunID).Scan(&v, &exact); err != nil || v != want || !exact {
		t.Fatalf("plan schema snapshot=%d want=%d exact=%t err=%v", v, want, exact, err)
	}
}

func backupCompatEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var all strings.Builder
	for _, table := range []string{"reconciliation_check_reports", "reconciliation_check_source_results", "reconciliation_check_findings", "reconciliation_check_report_seals", "reconciliation_repair_runs", "reconciliation_repair_manifest_items", "reconciliation_repair_plan_seals", "reconciliation_repair_attempts", "reconciliation_repair_events", "reconciliation_repair_results", "schema_migrations"} {
		filter := ""
		if table == "schema_migrations" {
			filter = " WHERE version<=22"
		}
		var rows string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t) AS row FROM `+pgx.Identifier{table}.Sanitize()+` t`+filter+`) q`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		all.WriteString(table + rows)
	}
	return all.String()
}

func backupCompatBoundary(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var closed bool
	if err := pool.QueryRow(ctx, `SELECT owner.rolname='nmcp_repair_function_owner' AND p.prosecdef
 AND NOT owner.rolcanlogin AND NOT owner.rolinherit
 AND p.proconfig=ARRAY[pg_catalog.format('search_path=%I, pg_catalog, pg_temp',current_schema())]
 AND (SELECT count(*) FILTER (WHERE a.privilege_type='EXECUTE' AND a.grantee=runtime.oid AND NOT a.is_grantable)=1
  AND count(*) FILTER (WHERE a.grantee<>p.proowner)=1
  FROM pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a)
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
 CROSS JOIN pg_catalog.pg_roles runtime
 WHERE p.oid='nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)'::regprocedure
 AND runtime.rolname='nmcp_repair_runtime'`).Scan(&closed); err != nil || !closed {
		t.Fatalf("repair reader closed owner/SECDEF/search_path/exact ACL=%t err=%v", closed, err)
	}
	var result string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_array(p.oid,owner.rolname,p.prosecdef,p.proconfig,p.proacl)::text
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
 WHERE p.oid='nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)'::regprocedure`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Clone immutable evidence in the isolated fixture, never mutate an old report.
func backupCompatClone(t *testing.T, ctx context.Context, tx pgx.Tx, source string, version int64, overrides map[string]any, sealed, sourceError bool) string {
	t.Helper()
	id := newUUIDv4(t)
	patch := map[string]any{"id": id, "migration_version": version}
	for k, v := range overrides {
		patch[k] = v
	}
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_check_reports
 SELECT (jsonb_populate_record(NULL::reconciliation_check_reports,to_jsonb(r)||jsonb_build_object(
 'migration_name',m.name,'migration_checksum',m.checksum)||$3::jsonb)).*
 FROM reconciliation_check_reports r CROSS JOIN schema_migrations m WHERE r.id=$1 AND m.version=$2`, source, version, data); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_check_source_results
 SELECT (jsonb_populate_record(NULL::reconciliation_check_source_results,to_jsonb(s)||jsonb_build_object('report_id',$1::text)
 ||CASE WHEN $3 AND source='storage_scan' THEN '{"result":"error","error_code":"fixture_error"}'::jsonb ELSE '{}'::jsonb END)).*
 FROM reconciliation_check_source_results s WHERE report_id=$2`, id, source, sourceError); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_check_findings
 SELECT (jsonb_populate_record(NULL::reconciliation_check_findings,to_jsonb(f)||jsonb_build_object('id',$3::text,'report_id',$1::text))).*
 FROM reconciliation_check_findings f WHERE report_id=$2`, id, source, newUUIDv4(t)); err != nil {
		t.Fatal(err)
	}
	if sealed {
		if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_check_report_seals
 SELECT (jsonb_populate_record(NULL::reconciliation_check_report_seals,to_jsonb(s)||jsonb_build_object('report_id',$1::text))).*
 FROM reconciliation_check_report_seals s WHERE report_id=$2`, id, source); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func backupCompatRejected(t *testing.T, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("expected fail-closed check violation, got %v", err)
	}
}

func backupCompatNegatives(t *testing.T, ctx context.Context, pool *pgxpool.Pool, source string) {
	t.Helper()
	for _, tc := range []struct {
		name                         string
		version                      int64
		patch                        map[string]any
		drop                         string
		unsealed, sourceError, dirty bool
	}{
		{name: "old16", version: 16, drop: "migration_version"},
		{name: "future24-with-matching-clean-history", version: 24},
		{name: "unknown-report-format", version: 23, drop: "format_version", patch: map[string]any{"format_version": 2}},
		{name: "unknown-classifier", version: 23, drop: "classifier_version", patch: map[string]any{"classifier_version": 2}},
		{name: "wrong-name", version: 23, patch: map[string]any{"migration_name": "unknown"}},
		{name: "wrong-checksum", version: 23, patch: map[string]any{"migration_checksum": strings.Repeat("0", 64)}},
		{name: "unsealed", version: 23, unsealed: true},
		{name: "source-error", version: 23, sourceError: true},
		{name: "dirty-history", version: 23, dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if tc.version == 24 {
				if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum,transactional,idempotent,dirty) VALUES(24,'future',repeat('a',64),true,false,false)`); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise reader defense-in-depth even for values rejected by storage CHECKs.
			// All adversarial DDL is isolated to this rollback-only fixture transaction.
			if tc.drop != "" {
				if _, err := tx.Exec(ctx, `ALTER TABLE reconciliation_check_reports DROP CONSTRAINT `+pgx.Identifier{"reconciliation_check_reports_" + tc.drop + "_check"}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			id := backupCompatClone(t, ctx, tx, source, tc.version, tc.patch, !tc.unsealed, tc.sourceError)
			if tc.dirty {
				if _, err := tx.Exec(ctx, `UPDATE schema_migrations SET dirty=true WHERE version=23`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE nmcp_repair_runtime`); err != nil {
				t.Fatal(err)
			}
			var count int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_repairable_findings($1,0,1)`, id).Scan(&count)
			backupCompatRejected(t, err)
		})
	}
	// Check the seal's independent explicit boundary, not just reader admission.
	for _, v := range []int64{16, 24} {
		t.Run(fmt.Sprintf("seal-rejects-%d", v), func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			run := newUUIDv4(t)
			if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_repair_runs(id,source_report_id,authorization_id,authorized_actor) VALUES($1,$2,$3,'fixture')`, run, source, newUUIDv4(t)); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO reconciliation_repair_plan_seals
    SELECT (jsonb_populate_record(NULL::reconciliation_repair_plan_seals,to_jsonb(s)||jsonb_build_object('id',$1::text,'run_id',$2::text,'report_migration_version',$3::bigint))).*
    FROM reconciliation_repair_plan_seals s WHERE source_report_id=$4 LIMIT 1`, newUUIDv4(t), run, v, source)
			backupCompatRejected(t, err)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.ConstraintName != "reconciliation_repair_plan_seals_report_migration_version_check" {
				t.Fatalf("wrong seal rejection constraint: %v", err)
			}
		})
	}
}
