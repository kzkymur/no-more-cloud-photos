package admin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This is an operator-only connection-routing proof, not a restricted-role
// full Restore/backup success proof. No role-phase or maintenance fence bypass.
func TestRestoreConnectionPG17Pinned(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("actual PG17 connection proof requires TEST_BACKUP_PG17_BINDIR")
	}
	f := newPG17Fixture(t, bin)
	operator := f.pool("operator", "postgres")
	f.sql(operator, `CREATE DATABASE candidate TEMPLATE template0`)
	f.sql(operator, `CREATE ROLE nmcp_api LOGIN PASSWORD `+quoteConnValue(f.passwords["nmcp_api"]))
	f.sql(operator, `CREATE DATABASE operator TEMPLATE template0`)
	cfg := operator.Config().Copy()
	cfg.ConnConfig.Database = "operator"
	unrelated, e := pgxpool.NewWithConfig(f.ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer unrelated.Close()
	f.sql(unrelated, `CREATE TABLE untouched(v text); INSERT INTO untouched VALUES ('must survive')`)
	fingerprint := func() string {
		return pg17Scalar(t, f.ctx, unrelated, `SELECT md5(jsonb_build_object(
 'class',(SELECT jsonb_agg(to_jsonb(c) ORDER BY oid) FROM pg_class c),
 'namespace',(SELECT jsonb_agg(to_jsonb(n) ORDER BY oid) FROM pg_namespace n),
 'attribute',(SELECT jsonb_agg(to_jsonb(a) ORDER BY attrelid,attnum) FROM pg_attribute a),
 'type',(SELECT jsonb_agg(to_jsonb(y) ORDER BY oid) FROM pg_type y),
 'proc',(SELECT jsonb_agg(to_jsonb(p) ORDER BY oid) FROM pg_proc p),
 'constraint',(SELECT jsonb_agg(to_jsonb(c) ORDER BY oid) FROM pg_constraint c),
 'data',(SELECT jsonb_agg(v ORDER BY v) FROM untouched))::text)`)
	}
	before := fingerprint()
	dir := privateTestDir(t)
	c := RestoreSettings{RestoreTool: filepath.Join(dir, "canary"), Root: filepath.Join(dir, "backups"), LedgerRoot: filepath.Join(dir, "ledger"), ReceiptFile: filepath.Join(dir, "receipt"), Service: "restore_operator", APIService: "restore_api", ServiceFile: filepath.Join(dir, "services"), PassFile: filepath.Join(dir, "pass"), StorageRoot: filepath.Join(dir, "storage"), FileBaseURL: "https://files.example.test/files/"}
	canary := filepath.Join(dir, "ran")
	f.write(c.RestoreTool, "#!/bin/sh\ntouch '"+canary+"'\nexit 1\n", 0700)
	f.write(c.PassFile, fmt.Sprintf("%s:6543:candidate:operator:%s\n%s:6543:candidate:nmcp_api:%s\n%s:6543:operator:operator:%s\nlocalhost:6543:candidate:operator:%s\n", f.socket, f.passwords["operator"], f.socket, f.passwords["nmcp_api"], f.socket, f.passwords["operator"], f.passwords["operator"]), 0600)
	partial := fmt.Sprintf("[restore_operator]\nhost=%s\nport=6543\nuser=operator\nsslmode=disable\n[restore_api]\nhost=%s\nport=6543\ndbname=candidate\nuser=nmcp_api\nsslmode=disable\n", f.socket, f.socket)
	f.write(c.ServiceFile, partial, 0600)
	// Establish the actual dangerous libpq default: the same partial section,
	// without the parent's PGDATABASE, reaches the unrelated user-named DB.
	defaultProbe := f.process(f.ctx, "psql", "-X", "--no-password", "--tuples-only", "--no-align", "--dbname=service="+c.Service, "--command=SELECT current_database()")
	defaultProbe.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "PGSERVICEFILE=" + c.ServiceFile, "PGPASSFILE=" + c.PassFile}
	raw, e := defaultProbe.Output()
	if e != nil || strings.TrimSpace(string(raw)) != "operator" {
		t.Fatal("could not prove actual libpq unrelated default-user target")
	}
	t.Setenv("PGDATABASE", "candidate")
	// Independently reproduce the pgx half of the original mismatch, without
	// invoking the fixed resolver: ambient PGDATABASE fills the partial section.
	legacy, e := pgxpool.New(f.ctx, "service="+quoteConnValue(c.Service)+" servicefile="+quoteConnValue(c.ServiceFile)+" passfile="+quoteConnValue(c.PassFile))
	if e != nil {
		t.Fatal("legacy divergence probe parse failed")
	}
	if pg17Scalar(t, f.ctx, legacy, `SELECT current_database()`) != "candidate" {
		legacy.Close()
		t.Fatal("ambient pgx candidate routing not established")
	}
	legacy.Close()
	if p, e := restorePool(f.ctx, c, c.Service); e == nil {
		p.Close()
		t.Fatal("partial ambient production probe accepted")
	}
	// The production entrypoint must reject before receipt/artifact processing,
	// before maintenance, and especially before it could execute pg_restore.
	if _, e := New(operator).Restore(f.ctx, newID(), c); e == nil {
		t.Fatal("ambient partial service accepted")
	}
	if _, e := os.Stat(canary); !os.IsNotExist(e) {
		t.Fatal("tool canary ran")
	}
	if fingerprint() != before {
		t.Fatal("unrelated default-user database changed")
	}
	t.Setenv("PGDATABASE", "")
	if e := os.Unsetenv("PGDATABASE"); e != nil {
		t.Fatal(e)
	}
	if _, e := resolveRestoreConnections(c); e == nil {
		t.Fatal("partial service accepted without environment")
	}
	complete := strings.Replace(partial, "user=operator\n", "dbname=candidate\nuser=operator\n", 1)
	f.write(c.ServiceFile, complete, 0600)
	r, e := resolveRestoreConnections(c)
	if e != nil {
		t.Fatal("complete explicit service rejected")
	}
	defer r.Close()
	// Retarget both on-disk inputs after resolution. Tool must keep the snapshot.
	f.write(c.ServiceFile, strings.ReplaceAll(complete, "dbname=candidate", "dbname=operator"), 0600)
	f.write(c.PassFile, "invalid\n", 0600)
	p, e := pgxpool.NewWithConfig(f.ctx, r.operator)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if pg17Scalar(t, f.ctx, p, `SELECT current_database()`) != "candidate" {
		t.Fatal("probe retargeted")
	}
	api, e := pgxpool.NewWithConfig(f.ctx, r.api)
	if e != nil {
		t.Fatal(e)
	}
	defer api.Close()
	if pg17Scalar(t, f.ctx, api, `SELECT session_user||':'||current_user||':'||current_database()`) != "nmcp_api:nmcp_api:candidate" {
		t.Fatal("API probe identity not pinned")
	}
	f.sql(operator, `CREATE TABLE pin_payload(v text); INSERT INTO pin_payload VALUES ('real dump payload')`)
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	dump := f.process(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-acl", "--table=pin_payload", "--dbname="+f.dsn("operator", "postgres"))
	var archive bytes.Buffer
	dump.Stdout = &archive
	if e := dump.Run(); e != nil {
		t.Fatal("real dump failed")
	}
	c.RestoreTool = filepath.Join(bin, "pg_restore")
	cmd := r.command(ctx, c)
	cmd.Stdin = &archive
	if e := cmd.Run(); e != nil {
		t.Fatal("real pg_restore pinned snapshot failed")
	}
	if pg17Scalar(t, f.ctx, p, `SELECT v FROM pin_payload`) != "real dump payload" {
		t.Fatal("candidate payload absent")
	}
	if fingerprint() != before {
		t.Fatal("tool changed unrelated default-user database")
	}
	t.Log("real PG17: partial+ambient rejected before canary; complete pinned probe/tool restored candidate only; unrelated catalog/data unchanged")
}
