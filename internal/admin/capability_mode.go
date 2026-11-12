package admin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

// Review B2 run modes. Capability commands (check, repair, maintenance
// resume) run as uid nmcp with only the nmcp_check / nmcp_repair logins and
// must never see the owner credential; owner commands may use the migrator
// login only as root. See docs/ubuntu-deployment.md "Running nmcp-admin".

// ownerCredentialName is the systemd credential (LoadCredential=admin.dsn:...)
// the owner invocation delivers; ownerDSNFileEnv is its file fallback.
const (
	ownerCredentialName = "admin.dsn"
	ownerDSNFileEnv     = "NMCP_ADMIN_DATABASE_URL_FILE"
	ownerDSNEnv         = "NMCP_DATABASE_URL"
)

var euid = os.Geteuid

// credentialsDirectoryOwner is the uid systemd's credential directory must
// have (root); tests substitute their own uid.
var credentialsDirectoryOwner uint32 = 0

func capabilityCommand(command string) bool {
	return command == "check" || command == "repair" || command == "maintenance resume"
}

// credentialsDirectory returns $CREDENTIALS_DIRECTORY after proving it is an
// absolute, root-owned directory not writable by group or others (systemd's
// /run/credentials/<unit>), or "" when unset.
func credentialsDirectory(getenv func(string) string) (string, error) {
	directory := getenv("CREDENTIALS_DIRECTORY")
	if directory == "" {
		return "", nil
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errCapabilityCredential
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", errCapabilityCredential
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != credentialsDirectoryOwner {
		return "", errCapabilityCredential
	}
	return directory, nil
}

// loadOwnerDSN resolves the owner connection string for owner commands: the
// systemd credential admin.dsn, else (only without $CREDENTIALS_DIRECTORY)
// the file named by NMCP_ADMIN_DATABASE_URL_FILE, else the legacy
// NMCP_DATABASE_URL environment value that the root migration unit and the
// nmcp-backup unit (backup-control service DSN, not the migrator) still use.
func loadOwnerDSN(getenv func(string) string) (string, error) {
	directory, err := credentialsDirectory(getenv)
	if err != nil {
		return "", err
	}
	if directory != "" {
		path := filepath.Join(directory, ownerCredentialName)
		if _, statErr := os.Lstat(path); statErr == nil {
			return readCapabilityDSN(path, 0o027)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", errCapabilityCredential
		}
	} else if path := getenv(ownerDSNFileEnv); path != "" {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", errCapabilityCredential
		}
		return readCapabilityDSN(path, 0o077)
	}
	return getenv(ownerDSNEnv), nil
}

// ownerGetenv serves the resolved owner DSN as NMCP_DATABASE_URL to the
// shared configuration loader, so validation stays in one place.
func ownerGetenv(getenv func(string) string, dsn string) func(string) string {
	return func(key string) string {
		if key == ownerDSNEnv {
			return dsn
		}
		return getenv(key)
	}
}

// refuseUnprivilegedOwner rejects an owner (migrator) session unless the
// process is root, so the migrator credential is never usable by uid nmcp.
// Other owner-command logins (the backup unit's nmcp_backup_control) pass.
func refuseUnprivilegedOwner(ctx context.Context, pool *pgxpool.Pool) error {
	var migrator bool
	if err := pool.QueryRow(ctx, `SELECT session_user::text='nmcp_migrator'
		OR (EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='nmcp_migrator')
		    AND pg_catalog.pg_has_role(session_user,'nmcp_migrator','MEMBER'))`).Scan(&migrator); err != nil {
		return err
	}
	if migrator && euid() != 0 {
		return ErrValidation
	}
	return nil
}

// refuseOwnerCredential fails closed when any owner DSN source is present
// in a capability command's environment, even an empty one.
func refuseOwnerCredential(getenv func(string) string, lookup func(string) (string, bool)) error {
	for _, key := range []string{ownerDSNEnv, ownerDSNFileEnv} {
		if _, present := lookup(key); present {
			return ErrValidation
		}
	}
	directory, err := credentialsDirectory(getenv)
	if err != nil {
		return err
	}
	if directory != "" {
		if _, statErr := os.Lstat(filepath.Join(directory, ownerCredentialName)); !errors.Is(statErr, os.ErrNotExist) {
			return ErrValidation
		}
	}
	return nil
}

// runCapability runs check, repair or maintenance resume as uid nmcp with
// capability pools only: no owner pool exists in this process.
func runCapability(ctx context.Context, o options, stdout, stderr io.Writer) int {
	if euid() == 0 {
		return finish(stdout, stderr, o.json, nil, ErrValidation)
	}
	if err := refuseOwnerCredential(os.Getenv, os.LookupEnv); err != nil {
		return finish(stdout, stderr, o.json, nil, err)
	}
	credentials := []capabilityCredential{checkCredential}
	switch o.command {
	case "repair":
		credentials = []capabilityCredential{repairCredential}
	case "maintenance resume":
		credentials = []capabilityCredential{checkCredential, repairCredential}
	}
	dsns := make([]string, len(credentials))
	for index, credential := range credentials {
		dsn, err := loadCapabilityDSN(credential, os.Getenv)
		if err != nil {
			return finish(stdout, stderr, o.json, nil, err)
		}
		dsns[index] = dsn
	}
	service := New(nil)
	service.capability = true
	var pinned *databaseIdentity
	for index, credential := range credentials {
		pool, err := openCapabilityPool(ctx, credential, dsns[index], pinned)
		if err != nil {
			return finish(stdout, stderr, o.json, nil, err)
		}
		defer pool.Close()
		if pinned == nil {
			identity, identityErr := queryDatabaseIdentity(ctx, pool)
			if identityErr != nil {
				return finish(stdout, stderr, o.json, nil, identityErr)
			}
			pinned = &identity
			migrator, migratorErr := database.NewMigrator(pool)
			if migratorErr != nil {
				return finish(stdout, stderr, o.json, nil, migratorErr)
			}
			status, statusErr := migrator.CapabilityStatus(ctx)
			if statusErr != nil || !status.Ready() {
				if statusErr == nil {
					statusErr = ErrConflict
				}
				return finish(stdout, stderr, o.json, nil, statusErr)
			}
		}
		if credential == repairCredential {
			service.RepairPool = pool
		} else {
			service.CheckPool = pool
		}
	}
	result, err := service.Operational(ctx, o)
	return finish(stdout, stderr, o.json, result, err)
}

// LoadOwnerConfig is the owner-command configuration for `nmcp-admin
// migrate`: the same DSN sources as every owner command.
func LoadOwnerConfig(getenv func(string) string) (config.AdminConfig, error) {
	dsn, err := loadOwnerDSN(getenv)
	if err != nil {
		return config.AdminConfig{}, err
	}
	return config.LoadAdminFrom(ownerGetenv(getenv, dsn))
}

// RefuseUnprivilegedOwner is refuseUnprivilegedOwner for `nmcp-admin migrate`.
func RefuseUnprivilegedOwner(ctx context.Context, pool *pgxpool.Pool) error {
	return refuseUnprivilegedOwner(ctx, pool)
}
