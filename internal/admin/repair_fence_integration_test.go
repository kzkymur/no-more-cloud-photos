package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/reconciliation"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestAdminIntegrationResumeCutoverRejectsWaitingDirectRepairPreflight(t *testing.T) {
	s := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := newID()
	path := filepath.Join(root, "originals", id[:2], id, "original.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if !errors.Is(err, ErrConflict) || !old.Sealed || old.FindingCount != 1 {
		t.Fatalf("old report=%+v err=%v", old, err)
	}
	// The historical report remains repairable, but the fresh full check is
	// clean. Do not create an unfinished plan that would invalidate resume.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='reverse fence',owner='test',entered_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s.afterResumeFence = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	resumed := make(chan error, 1)
	go func() { _, err := s.MaintenanceResume(ctx, store); resumed <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	config := s.Pool.Config().Copy()
	config.MaxConns = 1
	application := "reverse-fence-" + newID()
	config.ConnConfig.RuntimeParams["application_name"] = application
	directPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer directPool.Close()
	repository, err := reconciliation.NewRepairPostgresRepository(directPool)
	if err != nil {
		t.Fatal(err)
	}
	repairer, err := reconciliation.NewRepairer(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	direct := make(chan error, 1)
	go func() { _, err := repairer.DryRun(ctx, old.ReportID); direct <- err }()
	// Observe the actual native preflight waiting for the real row fence,
	// not a sleep or CLI advisory-lock conflict. Bound the observation by ctx.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='transactionid'
			AND query LIKE '%nmcp_preflight_repair_finding%')`, application).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-direct:
			t.Fatalf("direct preflight bypassed resume fence: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	unblock()
	if err := <-resumed; err != nil {
		t.Fatalf("fresh clean resume failed instead of cutting over: %v", err)
	}
	var admission *pgconn.PgError
	if err := <-direct; !errors.As(err, &admission) || admission.Code != "55000" {
		t.Fatalf("waiting direct preflight not rejected after normal cutover: %v", err)
	}
	var mode string
	var repairs, audit int
	if err := s.Pool.QueryRow(ctx, `SELECT mode,
		(SELECT count(*) FROM reconciliation_repair_runs)+(SELECT count(*) FROM reconciliation_repair_manifest_items),
		(SELECT count(*) FROM admin_audit WHERE command='maintenance_resume') FROM maintenance_state WHERE id=1`).Scan(&mode, &repairs, &audit); err != nil || mode != "normal" || repairs != 0 || audit != 1 {
		t.Fatalf("mode=%s repair mutations=%d resume audits=%d err=%v", mode, repairs, audit, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("direct preflight changed source after cutover: %v", err)
	}
}

func TestAdminIntegrationForwardRepairCatalogAndActualRoles(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checked, err := s.Check(ctx, store, reconciliation.ScopeAll)
	if err != nil || !checked.Sealed {
		t.Fatalf("fresh report=%+v err=%v", checked, err)
	}
	var schema, owner, searchPath string
	var definer, closedACL bool
	if err := s.Pool.QueryRow(ctx, `SELECT current_schema(),pg_get_userbyid(p.proowner),p.prosecdef,
		(SELECT setting FROM unnest(p.proconfig) setting WHERE setting LIKE 'search_path=%'),
		NOT EXISTS(SELECT 1 FROM aclexplode(p.proacl) acl WHERE acl.grantee NOT IN
		(SELECT oid FROM pg_roles WHERE rolname IN ('nmcp_repair_function_owner','nmcp_repair_runtime')))
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname=current_schema() AND p.proname='nmcp_read_repairable_findings'`).Scan(&schema, &owner, &definer, &searchPath, &closedACL); err != nil {
		t.Fatal(err)
	}
	wantPath := "search_path=" + schema + ", pg_catalog, pg_temp"
	if owner != "nmcp_repair_function_owner" || !definer || !closedACL || searchPath != wantPath {
		t.Fatalf("definer owner=%s enabled=%v path=%q want=%q closedACL=%v", owner, definer, searchPath, wantPath, closedACL)
	}
	for _, role := range []string{"nmcp_repair_runtime", "nmcp_check_runtime", "nmcp_runtime", "nmcp_worker_runtime"} {
		t.Run(role, func(t *testing.T) {
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(tx)
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			var actual string
			if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&actual); err != nil || actual != role {
				t.Fatalf("not actual role=%q err=%v", actual, err)
			}
			var count int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM nmcp_read_repairable_findings($1,0,1)`, checked.ReportID).Scan(&count)
			if role == "nmcp_repair_runtime" {
				if err != nil || count != 0 {
					t.Fatalf("repair role denied valid current evidence: count=%d err=%v", count, err)
				}
			} else {
				var denial *pgconn.PgError
				if !errors.As(err, &denial) || denial.Code != "42501" || !strings.Contains(denial.Message, "permission denied") {
					t.Fatalf("unauthorized actual role did not fail closed: %v", err)
				}
			}
		})
	}
}
