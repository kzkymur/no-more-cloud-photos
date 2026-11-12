package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A reconciliation capability is reachable only through its own dedicated
// login: check and repair never borrow the administrative (migrator)
// credential. The login holds exactly one INHERIT TRUE, SET FALSE membership
// in its NOLOGIN NOINHERIT capability role (provisioned by infra role SQL).
type capabilityCredential struct {
	env   string
	login string
	role  string
	// name is the systemd credential name (LoadCredential=NAME:PATH) the
	// documented operator invocation delivers in $CREDENTIALS_DIRECTORY.
	name string
}

var (
	checkCredential  = capabilityCredential{env: "NMCP_CHECK_DATABASE_URL_FILE", login: "nmcp_check", role: "nmcp_check_runtime", name: "check.dsn"}
	repairCredential = capabilityCredential{env: "NMCP_REPAIR_DATABASE_URL_FILE", login: "nmcp_repair", role: "nmcp_repair_runtime", name: "repair.dsn"}
)

// Roles a capability login must never reach, directly or transitively.
var capabilityForbiddenRoles = []string{
	"nmcp_migrator", "nmcp_api", "nmcp_worker", "nmcp_runtime", "nmcp_worker_runtime",
	"nmcp_check_runtime", "nmcp_repair_runtime",
	"nmcp_purge_function_owner", "nmcp_check_function_owner", "nmcp_repair_function_owner",
	"nmcp_backup_control", "nmcp_backup_dump", "nmcp_bootstrap",
}

const capabilityDSNMaxBytes = 4096

var errCapabilityCredential = fmt.Errorf("%w: capability credential refused", ErrValidation)

// loadCapabilityDSN reads the capability login's libpq connection string from
// the file named by its environment variable: an absolute, clean path to a
// regular file opened without following a final symlink, owned by root or the
// invoking user, with no group/other permission bits, holding exactly one DSN
// line of at most 4 KiB. The path and contents are never echoed.
func loadCapabilityDSN(c capabilityCredential, getenv func(string) string) (string, error) {
	// A systemd credential (the documented invocation's LoadCredential=) is
	// the primary source: the DSN is never nmcp-readable at rest. systemd may
	// deliver it root-owned 0440 with an ACL for the user, so that private,
	// root-owned directory admits group read but no write bits and no world
	// access. The *_FILE variable is only a fallback without
	// $CREDENTIALS_DIRECTORY (for example the Compose harness).
	directory, err := credentialsDirectory(getenv)
	if err != nil {
		return "", err
	}
	if directory != "" {
		path := filepath.Join(directory, c.name)
		if _, err := os.Lstat(path); err != nil {
			return "", errCapabilityCredential
		}
		return readCapabilityDSN(path, 0o027)
	}
	path := getenv(c.env)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errCapabilityCredential
	}
	return readCapabilityDSN(path, 0o077)
}

// readCapabilityDSN applies the trusted-file rule with the given forbidden
// permission bits.
func readCapabilityDSN(path string, forbidden os.FileMode) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errCapabilityCredential
	}
	file := os.NewFile(uintptr(fd), "capability-credential")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", errCapabilityCredential
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 && int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&forbidden != 0 ||
		info.Size() <= 0 || info.Size() > capabilityDSNMaxBytes {
		return "", errCapabilityCredential
	}
	data, err := io.ReadAll(io.LimitReader(file, capabilityDSNMaxBytes+1))
	if err != nil || len(data) > capabilityDSNMaxBytes {
		return "", errCapabilityCredential
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(bytes.TrimSpace(data)) == 0 || bytes.ContainsAny(data, "\x00\r\n") {
		return "", errCapabilityCredential
	}
	dsn := string(data)
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return "", errCapabilityCredential
	}
	return dsn, nil
}

// databaseIdentity pins one database on one running server: name, OID and
// the postmaster start time (two clusters, even with equal names and OIDs,
// never share a running postmaster).
type databaseIdentity struct {
	name      string
	oid       uint32
	startedAt string
}

const databaseIdentityQuery = `SELECT current_database()::text, d.oid::oid, pg_catalog.pg_postmaster_start_time()::text
	FROM pg_catalog.pg_database AS d WHERE d.datname=current_database()`

func queryDatabaseIdentity(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (databaseIdentity, error) {
	var id databaseIdentity
	if err := q.QueryRow(ctx, databaseIdentityQuery).Scan(&id.name, &id.oid, &id.startedAt); err != nil {
		return databaseIdentity{}, err
	}
	return id, nil
}

// capabilitySession is what a capability connection must prove about itself.
type capabilitySession struct {
	sessionUser, currentUser string
	superuser                bool
	hasCapability            bool
	forbiddenMember          bool
}

// validCapabilitySession is the fail-closed identity rule: exactly the
// dedicated login (so a migrator, API, Worker, backup or cross-capability DSN
// is refused), no SET ROLE or startup role switch, not superuser, the
// capability usable through inheritance, and no membership in any other
// managed role.
func validCapabilitySession(c capabilityCredential, s capabilitySession) bool {
	return s.sessionUser == c.login && s.currentUser == c.login && !s.superuser && s.hasCapability && !s.forbiddenMember
}

const capabilitySessionQuery = `SELECT session_user::text, current_user::text, r.rolsuper,
	pg_catalog.pg_has_role(session_user, $1::text, 'USAGE'),
	EXISTS (SELECT 1 FROM pg_catalog.unnest($2::text[]) AS f(name)
	        JOIN pg_catalog.pg_roles AS other ON other.rolname=f.name
	        WHERE other.rolname<>$1::text AND pg_catalog.pg_has_role(session_user, other.oid, 'MEMBER'))
	FROM pg_catalog.pg_roles AS r WHERE r.rolname=session_user`

// assertCapabilitySession proves the connection's identity with one catalog
// query; any query failure is a refusal.
func assertCapabilitySession(ctx context.Context, conn *pgx.Conn, c capabilityCredential, forbidden []string, database *databaseIdentity) error {
	if database != nil {
		// A stale credential (for example still naming the pre-cutover source)
		// must never check or repair a database other than the administrative
		// connection's.
		got, err := queryDatabaseIdentity(ctx, conn)
		if err != nil || got != *database {
			return errCapabilityCredential
		}
	}
	var s capabilitySession
	if err := conn.QueryRow(ctx, capabilitySessionQuery, c.role, forbidden).Scan(
		&s.sessionUser, &s.currentUser, &s.superuser, &s.hasCapability, &s.forbiddenMember); err != nil {
		return errCapabilityCredential
	}
	if !validCapabilitySession(c, s) {
		return errCapabilityCredential
	}
	return nil
}

// openCapabilityPool connects with the capability login and re-proves its
// identity on every pooled connection before any query can use it.
func openCapabilityPool(ctx context.Context, c capabilityCredential, dsn string, database *databaseIdentity) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errCapabilityCredential
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return assertCapabilitySession(ctx, conn, c, capabilityForbiddenRoles, database)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errCapabilityCredential
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if errors.Is(err, errCapabilityCredential) {
			return nil, errCapabilityCredential
		}
		// Unreachable database or authentication failure stays operational.
		return nil, err
	}
	return pool, nil
}
