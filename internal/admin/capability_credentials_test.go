package admin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

const validCapabilityDSN = "postgres://nmcp_check:secret@127.0.0.1:5432/nmcp?sslmode=disable"

func writeCapabilityFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check.dsn")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func envWith(name, value string) func(string) string {
	return func(key string) string {
		if key == name {
			return value
		}
		return ""
	}
}

func TestLoadCapabilityDSNFailsClosed(t *testing.T) {
	good := writeCapabilityFile(t, validCapabilityDSN+"\n", 0o600)
	if dsn, err := loadCapabilityDSN(checkCredential, envWith(checkCredential.env, good)); err != nil || dsn != validCapabilityDSN {
		t.Fatalf("valid credential refused: %q %v", dsn, err)
	}
	if _, err := loadCapabilityDSN(checkCredential, envWith(checkCredential.env, writeCapabilityFile(t, validCapabilityDSN, 0o400))); err != nil {
		t.Fatalf("0400 credential without newline refused: %v", err)
	}
	symlink := filepath.Join(t.TempDir(), "link.dsn")
	if err := os.Symlink(good, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo.dsn")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"unset":            "",
		"relative":         "check.dsn",
		"unclean":          filepath.Dir(good) + "/./check.dsn",
		"missing":          good + ".absent",
		"symlink":          symlink,
		"directory":        t.TempDir(),
		"fifo":             fifo,
		"group readable":   writeCapabilityFile(t, validCapabilityDSN, 0o640),
		"world readable":   writeCapabilityFile(t, validCapabilityDSN, 0o604),
		"empty":            writeCapabilityFile(t, "", 0o600),
		"blank":            writeCapabilityFile(t, " \n", 0o600),
		"two lines":        writeCapabilityFile(t, validCapabilityDSN+"\n"+validCapabilityDSN+"\n", 0o600),
		"carriage return":  writeCapabilityFile(t, validCapabilityDSN+"\r\n", 0o600),
		"NUL":              writeCapabilityFile(t, validCapabilityDSN+"\x00", 0o600),
		"oversized":        writeCapabilityFile(t, validCapabilityDSN+"&application_name="+strings.Repeat("a", capabilityDSNMaxBytes), 0o600),
		"not a DSN":        writeCapabilityFile(t, "postgres://[::1", 0o600),
		"other capability": "",
	} {
		t.Run(name, func(t *testing.T) {
			getenv := envWith(checkCredential.env, path)
			if name == "other capability" {
				// The repair variable never satisfies check.
				getenv = envWith(repairCredential.env, good)
			}
			dsn, err := loadCapabilityDSN(checkCredential, getenv)
			if !errors.Is(err, ErrValidation) || dsn != "" {
				t.Fatalf("credential accepted: %q %v", dsn, err)
			}
			if path != "" && strings.Contains(err.Error(), path) {
				t.Fatalf("refusal echoes the path: %v", err)
			}
		})
	}
}

func TestValidCapabilitySessionIdentityRule(t *testing.T) {
	ok := capabilitySession{sessionUser: "nmcp_check", currentUser: "nmcp_check", hasCapability: true}
	if !validCapabilitySession(checkCredential, ok) {
		t.Fatal("exact capability login refused")
	}
	for name, s := range map[string]capabilitySession{
		"migrator credential":    {sessionUser: "nmcp_migrator", currentUser: "nmcp_migrator", hasCapability: true},
		"repair login for check": {sessionUser: "nmcp_repair", currentUser: "nmcp_repair", hasCapability: true},
		"role switched":          {sessionUser: "nmcp_check", currentUser: "nmcp_check_runtime", hasCapability: true},
		"superuser":              {sessionUser: "nmcp_check", currentUser: "nmcp_check", superuser: true, hasCapability: true},
		"capability not usable":  {sessionUser: "nmcp_check", currentUser: "nmcp_check"},
		"extra membership":       {sessionUser: "nmcp_check", currentUser: "nmcp_check", hasCapability: true, forbiddenMember: true},
	} {
		if validCapabilitySession(checkCredential, s) {
			t.Errorf("%s accepted", name)
		}
	}
	if validCapabilitySession(repairCredential, ok) {
		t.Fatal("check login accepted for repair")
	}
}

// TestCapabilityCommandsRequireTheirCredential proves check, maintenance
// resume and repair fail closed (exit 2) before any database connection when
// their capability credential is absent or unsafe, and never fall back to the
// administrative credential.
func TestCapabilityCommandsRequireTheirCredential(t *testing.T) {
	t.Setenv("NMCP_DATABASE_URL", "postgres://nmcp_migrator:secret@127.0.0.1:1/nmcp?sslmode=disable&connect_timeout=1")
	t.Setenv(checkCredential.env, "")
	t.Setenv(repairCredential.env, "")
	unsafe := writeCapabilityFile(t, validCapabilityDSN, 0o644)
	for _, test := range []struct {
		args []string
		env  string
		path string
	}{
		{[]string{"check", "--scope", "all", "--json"}, checkCredential.env, ""},
		{[]string{"check", "--scope", "all", "--json"}, checkCredential.env, unsafe},
		{[]string{"maintenance", "resume", "--yes", "--json"}, checkCredential.env, ""},
		{[]string{"repair", "--report", newID(), "--yes", "--json"}, repairCredential.env, ""},
		{[]string{"repair", "--report", newID(), "--yes", "--json"}, repairCredential.env, unsafe},
	} {
		t.Run(strings.Join(test.args[:len(test.args)-1], " ")+" "+filepath.Base(test.path), func(t *testing.T) {
			t.Setenv(test.env, test.path)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), test.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2; stderr=%s", code, stderr.String())
			}
			if test.path != "" && strings.Contains(stderr.String()+stdout.String(), test.path) {
				t.Fatalf("refusal echoes the credential path: %s", stderr.String())
			}
		})
	}
}

// TestCapabilitySessionQueryOnPostgreSQL runs the production identity query
// on a real server. SET SESSION AUTHORIZATION changes session_user without
// password authentication, so throwaway uniquely named roles stand in for
// nmcp_check / nmcp_check_runtime.
func TestCapabilitySessionQueryOnPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set: PostgreSQL integration NOT executed")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	suffix := strings.ReplaceAll(newID(), "-", "")[:12]
	login, capability, forbidden, inheritless := "t_chk_"+suffix, "t_cap_"+suffix, "t_bad_"+suffix, "t_noi_"+suffix
	ident := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	for _, statement := range []string{
		"CREATE ROLE " + ident(capability) + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + ident(forbidden) + " NOLOGIN",
		"CREATE ROLE " + ident(login) + " LOGIN INHERIT",
		"CREATE ROLE " + ident(inheritless) + " LOGIN INHERIT",
		"GRANT " + ident(capability) + " TO " + ident(login) + " WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
		"GRANT " + ident(capability) + " TO " + ident(inheritless) + " WITH ADMIN FALSE, INHERIT FALSE, SET FALSE",
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		cleanup, err := pgx.Connect(context.Background(), databaseURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close(context.Background())
		for _, name := range []string{login, inheritless, forbidden, capability} {
			_, _ = cleanup.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident(name))
		}
	})
	credential := capabilityCredential{env: "UNUSED", login: login, role: capability}
	check := func(as string) error {
		if _, err := conn.Exec(ctx, "SET SESSION AUTHORIZATION "+ident(as)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := conn.Exec(ctx, "RESET SESSION AUTHORIZATION"); err != nil {
				t.Fatal(err)
			}
		}()
		return assertCapabilitySession(ctx, conn, credential, []string{forbidden, capability, "nmcp_role_that_does_not_exist"}, nil)
	}
	if err := check(login); err != nil {
		t.Fatalf("exact INHERIT TRUE capability login refused: %v", err)
	}
	if err := assertCapabilitySession(ctx, conn, credential, nil, nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("superuser session accepted: %v", err)
	}
	if err := check(inheritless); !errors.Is(err, ErrValidation) {
		t.Fatalf("other login with INHERIT FALSE membership accepted: %v", err)
	}
	// The database pin: this connection's identity passes, another database's
	// (same server, different name/OID) is refused.
	here, err := queryDatabaseIdentity(ctx, conn)
	if err != nil || here.name == "" || here.oid == 0 || here.startedAt == "" {
		t.Fatalf("database identity = %+v, %v", here, err)
	}
	if _, err := conn.Exec(ctx, "SET SESSION AUTHORIZATION "+ident(login)); err != nil {
		t.Fatal(err)
	}
	pinned := assertCapabilitySession(ctx, conn, credential, []string{forbidden}, &here)
	other := here
	other.name, other.oid = here.name+"_candidate", here.oid+1
	stale := assertCapabilitySession(ctx, conn, credential, []string{forbidden}, &other)
	restarted := here
	restarted.startedAt = "1970-01-01 00:00:00+00"
	otherServer := assertCapabilitySession(ctx, conn, credential, []string{forbidden}, &restarted)
	if _, err := conn.Exec(ctx, "RESET SESSION AUTHORIZATION"); err != nil {
		t.Fatal(err)
	}
	if pinned != nil || !errors.Is(stale, ErrValidation) || !errors.Is(otherServer, ErrValidation) {
		t.Fatalf("database pin: same=%v other database=%v other server=%v", pinned, stale, otherServer)
	}
	if _, err := conn.Exec(ctx, "GRANT "+ident(forbidden)+" TO "+ident(login)); err != nil {
		t.Fatal(err)
	}
	if err := check(login); !errors.Is(err, ErrValidation) {
		t.Fatalf("login with an extra managed-role membership accepted: %v", err)
	}
}
