package admin

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func capabilityRolesTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set: PostgreSQL integration NOT executed")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, databaseURL
}

func execAll(t *testing.T, pool *pgxpool.Pool, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestRestoreRoleGraphToleratesOnlyTheOptionalCapabilityLogins proves the
// role-graph rule in both directions: a cluster with the exactly provisioned
// optional logins hashes like one without them, and every other drift of those
// logins still changes the hash. Unique throwaway roles stand in for the
// nmcp_ graph so the shared test cluster's real roles are never touched.
func TestRestoreRoleGraphToleratesOnlyTheOptionalCapabilityLogins(t *testing.T) {
	pool, _ := capabilityRolesTestPool(t)
	ctx := context.Background()
	prefix := "t_rg" + strings.ReplaceAll(newID(), "-", "")[:10] + "_"
	ident := func(name string) string { return pgx.Identifier{prefix + name}.Sanitize() }
	login, capability := prefix+"check", prefix+"check_runtime"
	graph := func() string {
		t.Helper()
		hash, err := restoreRoleGraphWith(ctx, pool, prefix, []string{login}, []string{capability})
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	t.Cleanup(func() {
		for _, name := range []string{"other", "check", "migrator", "check_runtime"} {
			_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident(name))
		}
	})
	execAll(t, pool,
		"CREATE ROLE "+ident("check_runtime")+" NOLOGIN NOINHERIT",
		"CREATE ROLE "+ident("migrator")+" LOGIN")
	without := graph()

	execAll(t, pool,
		"CREATE ROLE "+ident("check")+" LOGIN INHERIT",
		"GRANT "+ident("check_runtime")+" TO "+ident("check")+" WITH ADMIN FALSE, INHERIT TRUE, SET FALSE")
	if with := graph(); with != without {
		t.Fatal("exactly provisioned optional login changed the role graph (backup before logins -> host with logins)")
	}
	execAll(t, pool, "DROP ROLE "+ident("check"))
	if graph() != without {
		t.Fatal("removing the optional login changed the role graph (host with logins -> host without)")
	}

	for name, drift := range map[string][]string{
		"login with CREATEROLE": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT CREATEROLE",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE"},
		"login with a connection limit": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT CONNECTION LIMIT 3",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE"},
		"login with role config": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"ALTER ROLE " + ident("check") + " SET search_path=public",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE"},
		"edge with SET TRUE": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET TRUE"},
		"edge without INHERIT": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT FALSE, SET FALSE"},
		"edge with ADMIN": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN TRUE, INHERIT TRUE, SET FALSE"},
		"extra edge to the migrator": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
			"GRANT " + ident("migrator") + " TO " + ident("check")},
		"member of the optional login": {"CREATE ROLE " + ident("check") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("check") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
			"CREATE ROLE " + ident("other") + " LOGIN", "GRANT " + ident("check") + " TO " + ident("other")},
		"capability edge to a non-optional login": {"CREATE ROLE " + ident("other") + " LOGIN INHERIT",
			"GRANT " + ident("check_runtime") + " TO " + ident("other") + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE"},
	} {
		t.Run(name, func(t *testing.T) {
			execAll(t, pool, drift...)
			defer execAll(t, pool, "DROP ROLE IF EXISTS "+ident("other"), "DROP ROLE IF EXISTS "+ident("check"))
			if graph() == without {
				t.Fatalf("%s did not change the role graph", name)
			}
		})
	}
}

// TestCutoverFenceCountsLiveCapabilitySessions proves a live session of a
// fenced login blocks the cutover fence and that the reconciliation logins
// are fenced alongside API, Worker and backup.
func TestCutoverFenceCountsLiveCapabilitySessions(t *testing.T) {
	for _, required := range []string{"nmcp_api", "nmcp_worker", "nmcp_backup_dump", "nmcp_backup_control", "nmcp_check", "nmcp_repair"} {
		if !slices.Contains(cutoverFencedLogins, required) {
			t.Fatalf("cutover fence omits %s", required)
		}
	}
	pool, databaseURL := capabilityRolesTestPool(t)
	ctx := context.Background()
	login := "t_fence" + strings.ReplaceAll(newID(), "-", "")[:10]
	password := strings.ReplaceAll(newID(), "-", "")
	execAll(t, pool, "CREATE ROLE "+pgx.Identifier{login}.Sanitize()+" LOGIN PASSWORD '"+password+"'")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename=$1", login)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{login}.Sanitize())
	})
	previous := cutoverFencedLogins
	cutoverFencedLogins = append(slices.Clone(previous), login)
	t.Cleanup(func() { cutoverFencedLogins = previous })

	var database uint32
	if err := pool.QueryRow(ctx, "SELECT oid FROM pg_database WHERE datname=current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	baseline, err := fencedSessionCount(ctx, pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	baselineElsewhere, err := fencedSessionCount(ctx, pool, database)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.User, config.Password = login, password
	session, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect as the throwaway fenced login: %v", err)
	}
	live, err := fencedSessionCount(ctx, pool, 0)
	if err != nil || live != baseline+1 {
		t.Fatalf("live fenced session not counted: baseline=%d live=%d err=%v", baseline, live, err)
	}
	if excluded, err := fencedSessionCount(ctx, pool, database); err != nil || excluded != baselineElsewhere {
		t.Fatalf("session in the excepted database counted: excluded=%d baseline=%d err=%v", excluded, baselineElsewhere, err)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
