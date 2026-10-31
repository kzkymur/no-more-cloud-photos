//go:build linux

package admin

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"
)

// This proves the passfile portability defect with a self-only ACL, not actual
// systemd delivery, distinct-UID isolation, or the cause of a prior root run.
func TestBackupPG17PrivatePassfileMaterialization(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: actual credential compatibility NOT executed")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute PG17 prefix required")
	}
	version, err := exec.Command(filepath.Join(bin, "postgres"), "--version").Output()
	if err != nil || !strings.Contains(string(version), "(PostgreSQL) 17.") {
		t.Fatal("actual PG17 required")
	}
	f := newPG17Fixture(t, bin)
	admin := f.pool("operator", "postgres")
	f.sql(admin, "CREATE ROLE nmcp_backup_dump LOGIN PASSWORD '"+f.passwords["nmcp_backup_dump"]+"'")
	f.sql(admin, "CREATE DATABASE source")
	private := filepath.Join(f.root, "private-credentials")
	if os.Mkdir(private, 0700) != nil {
		t.Fatal("private credential directory")
	}
	source := filepath.Join(private, "delivered.pgpass")
	materialized := filepath.Join(private, "dump.pgpass")
	data, err := os.ReadFile(f.passfiles["nmcp_backup_dump"])
	if err != nil || os.WriteFile(source, data, 0400) != nil {
		t.Fatal("private credential fixture")
	}
	// Linux ACL: owner read, named CURRENT UID read, group/other none, mask read.
	// No unrelated principal receives access; private parent remains 0700.
	acl := make([]byte, 44)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, 4, ^uint32(0)}, {2, 4, uint32(os.Geteuid())}, {4, 0, ^uint32(0)}, {16, 4, ^uint32(0)}, {32, 0, ^uint32(0)}} {
		at := 4 + i*8
		binary.LittleEndian.PutUint16(acl[at:], e.tag)
		binary.LittleEndian.PutUint16(acl[at+2:], e.perm)
		binary.LittleEndian.PutUint32(acl[at+4:], e.id)
	}
	if unix.Setxattr(source, "system.posix_acl_access", acl, 0) != nil {
		t.Fatal("actual POSIX ACL support required for opted-in credential proof")
	}
	readback := make([]byte, 64)
	n, err := unix.Getxattr(source, "system.posix_acl_access", readback)
	info, statErr := os.Lstat(source)
	if err != nil || statErr != nil || !bytes.Equal(acl, readback[:n]) || info.Mode().Perm() != 0440 {
		t.Fatal("self-only ACL mode/readback mismatch")
	}
	settings := BackupSettings{DumpService: "nmcp_backup_dump", ServiceFile: f.serviceFile, PassFile: source}
	if conn, err := settings.dumpConnect(f.ctx); err == nil || conn != nil {
		if conn != nil {
			conn.Close(f.ctx)
		}
		t.Fatal("strict dump credential guard accepted ACL group-class bits")
	}
	// pgx itself can read that ACL; libpq cannot, despite identical effective privacy.
	conn, err := pgx.Connect(f.ctx, "service=nmcp_backup_dump servicefile="+quoteConnValue(f.serviceFile)+" passfile="+quoteConnValue(source))
	if err != nil {
		t.Fatal("pgx ACL-readable authentication failed")
	}
	conn.Close(f.ctx)
	external := func(tool, pass string, args ...string) ([]byte, []byte, error) {
		cmd := exec.CommandContext(f.ctx, filepath.Join(bin, tool), args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "PGSERVICE=nmcp_backup_dump", "PGSERVICEFILE=" + f.serviceFile, "PGPASSFILE=" + pass}
		// Local relocated binaries only; production unit/tool environment is unchanged.
		if ld := os.Getenv("LD_LIBRARY_PATH"); ld != "" {
			cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+ld)
		}
		var out, diagnostic bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diagnostic
		err := cmd.Run()
		return out.Bytes(), diagnostic.Bytes(), err
	}
	_, diagnostic, err := external("psql", source, "-X", "--no-password", "--tuples-only", "--no-align", "--command=SELECT 1")
	if err == nil || !bytes.Contains(diagnostic, []byte("group or world access")) || !bytes.Contains(diagnostic, []byte("no password supplied")) {
		t.Fatal("libpq ACL passfile refusal not demonstrated")
	}
	copyAndVerify := func(label string) {
		t.Helper()
		// Same argv as the ordinary (non-root-prefixed) production ExecStartPre.
		if exec.CommandContext(f.ctx, "/usr/bin/install", "-m", "0600", source, materialized).Run() != nil {
			t.Fatal("service-owned private materialization")
		}
		copied, e := os.ReadFile(materialized)
		st, se := os.Lstat(materialized)
		if e != nil || se != nil || !bytes.Equal(data, copied) || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
			t.Fatal("private copy byte/mode mismatch")
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || raw.Uid != uint32(os.Geteuid()) || raw.Nlink != 1 {
			t.Fatal("private copy ownership/link mismatch")
		}
		settings.PassFile = materialized
		c, e := settings.dumpConnect(f.ctx)
		if e != nil {
			t.Fatal("unchanged strict Go guard refused private copy")
		}
		var who string
		e = c.QueryRow(f.ctx, "SELECT current_user").Scan(&who)
		c.Close(f.ctx)
		if e != nil || who != "nmcp_backup_dump" {
			t.Fatal("private copy used wrong database identity")
		}
		archive := filepath.Join(private, label+".dump")
		if _, _, e = external("pg_dump", materialized, "--no-password", "--format=custom", "--file="+archive); e != nil {
			t.Fatal("actual libpq dump refused private copy")
		}
		if _, _, e = external("pg_restore", materialized, "--list", archive); e != nil {
			t.Fatal("actual dump archive listing failed")
		}
	}
	copyAndVerify("acl")
	// A weak copy is still rejected; no guard relaxation accompanies materialization.
	if os.Chmod(materialized, 0640) != nil {
		t.Fatal("owned negative copy mode")
	}
	if c, e := settings.dumpConnect(f.ctx); e == nil || c != nil {
		if c != nil {
			c.Close(f.ctx)
		}
		t.Fatal("weak destination admitted")
	}
	// Stale but correctly private contents must fail authentication, then be
	// replaced rather than reused on the next ordinary install invocation.
	stale := bytes.ReplaceAll(data, []byte(f.passwords["nmcp_backup_dump"]), []byte("stale-password"))
	if bytes.Equal(stale, data) || os.Chmod(materialized, 0600) != nil || os.WriteFile(materialized, stale, 0600) != nil {
		t.Fatal("owned stale credential setup")
	}
	if c, e := settings.dumpConnect(f.ctx); e == nil || c != nil {
		if c != nil {
			c.Close(f.ctx)
		}
		t.Fatal("stale credential authenticated")
	}
	// Exercise the owner-private 0400 fallback and replacement of stale contents.
	if unix.Removexattr(source, "system.posix_acl_access") != nil || os.Chmod(source, 0400) != nil {
		t.Fatal("owned fallback credential")
	}
	copyAndVerify("owner-private")
	t.Log("Actual PG17: self-only ACL0440 accepted by pgx but refused by unchanged Go guard/libpq; byte-identical service-owned0600 copies passed strict Go identity, pg_dump and pg_restore; owner0400 fallback also passed")
}
