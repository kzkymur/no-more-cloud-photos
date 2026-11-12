package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	previousEUID := euid
	euid = func() int { return 1000 }
	t.Cleanup(func() { euid = previousEUID })
	t.Setenv("CREDENTIALS_DIRECTORY", "")
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

// TestLoadCapabilityDSNFromSystemdCredentials covers the documented
// invocation: LoadCredential= delivers <name> in $CREDENTIALS_DIRECTORY and
// takes precedence over the *_FILE variable.
func TestLoadCapabilityDSNFromSystemdCredentials(t *testing.T) {
	newCredentials := testCredentialsDirectories(t)
	credentials := newCredentials()
	write := func(mode os.FileMode) {
		t.Helper()
		path := filepath.Join(credentials, checkCredential.name)
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(validCapabilityDSN+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	unreadableFallback := filepath.Join(t.TempDir(), "absent.dsn")
	getenv := func(key string) string {
		switch key {
		case "CREDENTIALS_DIRECTORY":
			return credentials
		case checkCredential.env:
			return unreadableFallback
		}
		return ""
	}
	for _, mode := range []os.FileMode{0o400, 0o440, 0o600} {
		write(mode)
		if dsn, err := loadCapabilityDSN(checkCredential, getenv); err != nil || dsn != validCapabilityDSN {
			t.Fatalf("systemd credential %o refused: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o444, 0o460, 0o602} {
		write(mode)
		if _, err := loadCapabilityDSN(checkCredential, getenv); !errors.Is(err, ErrValidation) {
			t.Fatalf("systemd credential %o accepted", mode)
		}
	}
	write(0o400)
	if _, err := loadCapabilityDSN(repairCredential, getenv); !errors.Is(err, ErrValidation) {
		t.Fatal("check credential satisfied repair")
	}
	_ = os.Remove(filepath.Join(credentials, checkCredential.name))
	if err := os.Symlink(writeCapabilityFile(t, validCapabilityDSN, 0o600), filepath.Join(credentials, checkCredential.name)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCapabilityDSN(checkCredential, getenv); !errors.Is(err, ErrValidation) {
		t.Fatal("symlinked systemd credential accepted")
	}
	_ = os.Remove(filepath.Join(credentials, checkCredential.name))
	fallback := writeCapabilityFile(t, validCapabilityDSN, 0o600)
	// With $CREDENTIALS_DIRECTORY set the credential must be there: the file
	// variable is a fallback only when systemd delivers no credentials.
	if _, err := loadCapabilityDSN(checkCredential, func(key string) string {
		if key == "CREDENTIALS_DIRECTORY" {
			return credentials
		}
		if key == checkCredential.env {
			return fallback
		}
		return ""
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("absent systemd credential fell back to the file variable: %v", err)
	}
	if dsn, err := loadCapabilityDSN(checkCredential, envWith(checkCredential.env, fallback)); err != nil || dsn != validCapabilityDSN {
		t.Fatalf("file fallback without CREDENTIALS_DIRECTORY refused: %v", err)
	}
	credentialsDirectoryOwner = uint32(os.Geteuid()) + 1
	write(0o400)
	if _, err := loadCapabilityDSN(checkCredential, getenv); !errors.Is(err, ErrValidation) {
		t.Fatal("credentials directory not owned by root accepted")
	}
	credentialsDirectoryOwner = uint32(os.Geteuid())
	if _, err := loadCapabilityDSN(checkCredential, func(key string) string {
		if key == "CREDENTIALS_DIRECTORY" {
			return "relative/dir"
		}
		return ""
	}); !errors.Is(err, ErrValidation) {
		t.Fatal("relative CREDENTIALS_DIRECTORY accepted")
	}
}

// TestCapabilityCommandsNeverAcceptTheOwnerCredential pins review B2: a
// capability command refuses (exit 2) when any owner DSN source is present,
// even an empty NMCP_DATABASE_URL, and refuses to run as root.
func TestCapabilityCommandsNeverAcceptTheOwnerCredential(t *testing.T) {
	previousEUID := euid
	t.Cleanup(func() { euid = previousEUID })
	good := writeCapabilityFile(t, validCapabilityDSN, 0o600)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv(checkCredential.env, good)
	t.Setenv(repairCredential.env, good)
	for name, setup := range map[string]func(){
		"migrator DSN in env":            func() { t.Setenv(ownerDSNEnv, "postgres://nmcp_migrator:secret@127.0.0.1:1/nmcp") },
		"empty migrator DSN env":         func() { t.Setenv(ownerDSNEnv, "") },
		"owner DSN file variable":        func() { t.Setenv(ownerDSNFileEnv, good) },
		"capability command run as root": func() { euid = func() int { return 0 } },
	} {
		t.Run(name, func(t *testing.T) {
			euid = func() int { return 1000 }
			setup()
			for _, args := range [][]string{{"check", "--scope", "all", "--json"}, {"repair", "--report", newID(), "--yes", "--json"}} {
				var stdout, stderr bytes.Buffer
				if code := Run(context.Background(), args, &stdout, &stderr); code != 2 {
					t.Fatalf("%s: exit %d, want 2 (stderr=%s)", args[0], code, stderr.String())
				}
			}
		})
	}
}

func TestLoadOwnerDSNSources(t *testing.T) {
	newCredentials := testCredentialsDirectories(t)
	credentials := newCredentials()
	if err := os.WriteFile(filepath.Join(credentials, ownerCredentialName), []byte("postgres://from-credential\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	file := writeCapabilityFile(t, "postgres://from-file", 0o600)
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	for name, test := range map[string]struct {
		values map[string]string
		want   string
		ok     bool
	}{
		"credential wins":                   {map[string]string{"CREDENTIALS_DIRECTORY": credentials, ownerDSNFileEnv: file, ownerDSNEnv: "postgres://env"}, "postgres://from-credential", true},
		"file without credentials dir":      {map[string]string{ownerDSNFileEnv: file, ownerDSNEnv: "postgres://env"}, "postgres://from-file", true},
		"legacy env":                        {map[string]string{ownerDSNEnv: "service=nmcp_backup_control"}, "service=nmcp_backup_control", true},
		"file ignored with credentials dir": {map[string]string{"CREDENTIALS_DIRECTORY": newCredentials(), ownerDSNFileEnv: file, ownerDSNEnv: "postgres://env"}, "postgres://env", true},
		"group readable file":               {map[string]string{ownerDSNFileEnv: writeCapabilityFile(t, "postgres://x", 0o640)}, "", false},
		"relative file":                     {map[string]string{ownerDSNFileEnv: "admin.dsn"}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := loadOwnerDSN(env(test.values))
			if (err == nil) != test.ok || got != test.want {
				t.Fatalf("loadOwnerDSN = %q, %v; want %q ok=%t", got, err, test.want, test.ok)
			}
		})
	}
}

// testCredentialsDirectories stands in for systemd's /run/credentials: it
// points the parent and owner rules at a private temporary tree and returns
// a maker of per-unit directories below it.
func testCredentialsDirectories(t *testing.T) func() string {
	t.Helper()
	previousOwner, previousParent := credentialsDirectoryOwner, credentialsDirectoryParent
	parent := t.TempDir()
	credentialsDirectoryOwner, credentialsDirectoryParent = uint32(os.Geteuid()), parent
	t.Cleanup(func() { credentialsDirectoryOwner, credentialsDirectoryParent = previousOwner, previousParent })
	count := 0
	return func() string {
		count++
		dir := filepath.Join(parent, fmt.Sprintf("unit-%d.service", count))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
}

// TestCredentialsDirectoryMustBeSystemdUnitDirectory pins review n4.
func TestCredentialsDirectoryMustBeSystemdUnitDirectory(t *testing.T) {
	newCredentials := testCredentialsDirectories(t)
	unit := newCredentials()
	if got, err := credentialsDirectory(func(string) string { return unit }); err != nil || got != unit {
		t.Fatalf("unit directory refused: %q %v", got, err)
	}
	nested := filepath.Join(unit, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"outside /run/credentials": t.TempDir(), "nested below a unit": nested, "the parent itself": credentialsDirectoryParent} {
		if _, err := credentialsDirectory(func(string) string { return dir }); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
