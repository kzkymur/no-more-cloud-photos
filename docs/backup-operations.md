# Database backup operations

## Scope and readiness

The backup is a PostgreSQL custom-format database dump, not an Original/media
backup, PostgreSQL base backup, WAL archive, or point-in-time recovery (PITR).
Originals remain subject to the separate cloud-original retention policy.
A backup on the same HDD can survive a logical database mistake, **not HDD loss**.
Keep an independently verified off-device copy under an operator policy; this
unit does not upload or verify such a copy. Protect dumps like the live database.

The files below define production wiring, not proof of deployment. Installer,
release packaging and real PostgreSQL 17 role/restore tests must pass before
activation. This runbook does not assert that the restore CLI/cutover workflow
is complete. Follow the released CLI help and accepted restore evidence; do not
substitute a manual `pg_restore` for missing application validation.

## Role lifecycle: bootstrap, migrate, then exact object grants

Role creation runs before tables exist. It is **not** evidence that dump/control
object access is ready. As the cluster administrator, on the pinned intended DB:

```sh
# Fresh bootstrap establishes the closed hardened graph first.
psql -X --dbname=nmcp --set=ON_ERROR_STOP=1 \
  --set=nmcp_migration_compatibility=0 \
  --file=infra/ubuntu/postgresql-roles.sql
# Provision distinct login passwords interactively (\password), never in argv.
# Execute the existing reviewed release migration compatibility protocol.
# Once the exact release migrations are current, re-harden and grant objects:
psql -X --dbname=nmcp --set=ON_ERROR_STOP=1 \
  --set=nmcp_migration_compatibility=0 \
  --set=nmcp_backup_grants_after_migration=1 \
  --file=infra/ubuntu/postgresql-roles.sql
```

Use the trusted release's SQL files, not a stale downloaded bootstrap. On a
fresh DB, do not start in compatibility mode: its guard correctly requires an
already established closed graph. The reviewed migration wrapper temporarily
enables only its exact migration-owner inheritance, then re-hardens it.

The post-migration phase grants dump SELECT/schema USAGE and owner-specific
default privileges across **all** owners; inventory must reject gaps. Control
gets SELECT on `system_config`, `maintenance_state`, `schema_migrations`,
SELECT/INSERT/UPDATE on `backup_runs`, INSERT on `admin_audit`, and EXECUTE on
`nmcp_backup_normal_fence()`. It receives **no** direct UPDATE on maintenance.
That closed schema-qualified function only holds a short shared maintenance
fence. No routine credential gains owner membership, CREATEDB, SUPERUSER or
restore authority. Repeat the postphase after an accepted migration adds objects.
Only then install backup credentials/configuration and activate the wake unit.

## Local installation prerequisites

Provision a dedicated non-login `nmcp-backup` Unix user/group, no supplementary
runtime groups. Use `/var/lib/nmcp-backup`, owned by that user/group, mode 0700.
It is deliberately outside `/var/lib/nmcp` (runtime-owned mode 0700) and outside
all Nginx bind mounts. Never put dumps in `media`, `originals`, `renditions`, or
`/srv/nmcp`. Confirm the approved durable mount, free bytes/inodes and access as
the backup UID. No recursive permission changes to media are required.

Install the reviewed units as root:root 0644 and the release binary at
`/opt/nmcp/current/bin/nmcp-admin`. Unit writable storage is only the private
backup root (plus systemd private temporary storage), UMask 0077 and strict
hardening. UNIX PostgreSQL sockets remain available; do not use `PrivateNetwork`
or hide `/var/run/postgresql`. Pin the concrete PostgreSQL dependency through
the deployment configuration, not the `postgresql.service` meta-unit. Do not
use the migration oneshot as an uncoordinated backup startup dependency.

Install `infra/ubuntu/env/backup.env.example` as `/etc/nmcp/backup.env`,
root:root 0600. Tool paths are fixed `/usr/bin/pg_dump` and `/usr/bin/pg_restore`;
install PostgreSQL 17 clients and verify the actual selected versions. A generic
Ubuntu wrapper selecting another major is not sufficient. No passwords or
operator restore credentials belong in this environment.

Create `/etc/nmcp/backup.pg_service.conf` root:root 0600 with exactly:

```ini
[nmcp_backup_control]
host=/var/run/postgresql
port=5432
dbname=nmcp
user=nmcp_backup_control
sslmode=disable

[nmcp_backup_dump]
host=/var/run/postgresql
port=5432
dbname=nmcp
user=nmcp_backup_dump
sslmode=disable
```

Keep parents root-owned, not group/world-writable. Configure local socket HBA
for the two distinct logins and the backup UID; test actual authentication, not
just the presence of service sections. The dump login must be readonly, have
explicit SELECT/USAGE on **every** application schema/table/sequence across all
owners and owner-specific default privileges, no owner memberships,
`pg_read_all_data`, CREATEDB, CREATEROLE, BYPASSRLS or superuser. Inventory rejects
unsupported RLS/large objects rather than silently omitting them. Control gets
only configuration reads, backup-history/notification persistence and audit
writes required by the application, never migrator/owner privileges. Prove
negative DML/CREATE/SET ROLE/other-database access for dump and privileged-role
isolation with real PostgreSQL 17 tests.

Create root:root 0700 `/etc/nmcp/credentials`; securely provision separate
`backup-dump.pgpass` and `backup-control.pgpass`, root:root 0600, each containing
only its matching source database/login password entry. For a non-default UNIX
socket, libpq and pgx may require two exact host aliases (`localhost` and that
same absolute socket directory); both must use the identical port/DB/login/password.
No wildcard, duplicate alias, other socket, database or login is permitted. Never pass passwords
in argv, shell history, reports or logs. `LoadCredential=` snapshots them into
the service credential namespace. ACL-backed systemd credentials may be
root-owned 0440: libpq rejects those group-class bits even for a private named-user
ACL. Two exact ordinary `ExecStartPre` commands run as `nmcp-backup` and copy the
control/dump credentials to manager-created `/run/nmcp-backup` (0700), producing
service-owned 0600 single-link files. `PGPASSFILE` and
`NMCP_BACKUP_DUMP_PASSFILE` point only to those private copies; `PGSERVICEFILE`
still uses `%d/pg_service.conf`. `RuntimeDirectoryPreserve=no` removes the copies
on stop/failure, and every activation recreates them from current credentials.
The original LoadCredential files, backup UID, namespace hardening and strict
passfile validation are unchanged. Never broaden `/run` write access, preserve
this directory, add root execution prefixes, or chmod the source ACL. Do not put
`${CREDENTIALS_DIRECTORY}` in an EnvironmentFile: systemd does not perform shell
interpolation there. The dump child must receive only dump credentials, not the
control password or routine process environment. Neither credential authorizes
restore, role creation, candidate provisioning or cutover.

## Wake timer, manual runs and results

`scripts/validate-backup-install` runs repository-static checks;
`sudo scripts/validate-backup-install --installed` performs **read-only local**
metadata/configuration and client-version checks. It never installs anything,
contacts a DB, enables units, or performs remote operations. It cannot prove
role ACLs, HBA, served-mount exclusion, effective systemd state or restore.

After separate deployment approval, inspect `systemd-analyze verify`, effective
unit properties/drop-ins, actual database connectivity and the Nginx mount
namespace before enabling the timer. The hourly timer with bounded wake jitter
only calls `backup run --if-due --json`. Current typed database configuration,
last successful backup and the shared advisory lock decide eligibility. Changing
the database interval must not require editing the timer. A persistent missed
wake does not synthesize missed backup successes.

A manual backup uses the same binary and routine control/dump environment but
omits `--if-due`:

```sh
/opt/nmcp/current/bin/nmcp-admin backup run --json
/opt/nmcp/current/bin/nmcp-admin backup list --json
```

These are CLI shapes, not commands to run as an arbitrary interactive user.
An approved manual execution must use the dedicated UID and the same hardened
credential-bearing service namespace (operator-created one-shot with identical
properties and only the ExecStart changed). An interactive root invocation does
not inherit `LoadCredential`; do not copy credentials to a shell-readable temp
file or source the env file as shell code. Manual and timed runs share the lock.
Starting the shipped service manually still performs a **due** check.

Exit 4 has structured JSON outcome `not_due` or `overlap_conflict` and is allowed
by `SuccessExitStatus=4`; neither outcome is a successful backup, a last-success
update, an RPO reset, or a success metric. Unit success alone is not enough:
inspect structured outcome, durable backup history, validated manifest, bytes,
size and digest. Record artifact publication and database history separately;
keep published bytes after a post-publication crash for recovery adjudication.
Do not remove the newest validated successful backup merely because it expired.

## Failure notifications and measurements

Install a reviewed root-owned 0755 executable
`/usr/local/libexec/nmcp/backup-notify`, then add
`NMCP_BACKUP_NOTIFY_HOOK=/usr/local/libexec/nmcp/backup-notify` to backup.env.
It is a fixed executable with no arguments and no shell interpretation. The
application sends bounded JSON metadata on stdin (backup ID, failure phase and
status); no dump bytes, credentials or raw subprocess errors. It runs with a
minimal nonsecret environment and a 10-second deadline; output is discarded.
Keep endpoint secrets outside argv and this routine environment, readable only
through a separately reviewed notification mechanism compatible with the unit.
A missing, timed-out or nonzero hook must persist a sanitized notification
failure and return nonzero, not erase the causal backup failure. Verify that
behavior and restart recovery before depending on alerts.

Record measured RPO from the actual successful snapshot cutoff, not timer wake
or notification time. Measure end-to-end RTO through candidate creation, restore,
validation, operator cutover, pool reconnection and explicit resume, with
monotonic phase timings. No measured RPO/RTO is claimed by these static assets.

## Operator restore boundary (acceptance still required)

The canonical prepare command remains
`nmcp-admin restore --backup-id UUID --yes --json`; it is **not** `--id`.
Operator authority is a local peer identity or single-invocation 0600 credential,
never a routine service credential. Do not give the daemon CREATEDB/superuser.
The operator preprovisions an empty template0 candidate and an external durable
receipt pinning cluster, DB OID/name/owner and empty state. Reject an arbitrary
existing database; allow nonempty continuation only with the same durable
operation authorization. Store the ledger outside the dump and outside served
storage, inaccessible to routine users.

Before restore, candidate socket/HBA restrictions must deny routine API/Worker/
backup identities independently of imported ACLs. Keep that gate during actual
multiowner owner/ACL-preserving restore and validation. Preserve the source
maintenance fence and external authorization despite imported running backup
rows or a normal-mode flag. Candidate validation must include migration checks,
configuration/invariants, filesystem reconciliation and real HTTP/read API
behavior using the actual API login/ACL (not operator-superuser reads).

Cutover is a separate explicit operator action, not an automatic consequence of
prepare: revalidate receipt/digest/target identity, drain fixed API/Worker/backup
units and stop the wake timer, change only reviewed connection configuration,
reconnect **all** pools, prove candidate OIDs/HTTP and old-source disconnection.
Keep source fenced and both databases in maintenance. Pre-cutover failure keeps
the source authoritative and fenced; do not drop it. Fresh validation and a
separate explicit `resume --yes` are mandatory before ordinary writes resume.
Resume the wake timer only after that successful decision. Never infer cutover
or resume from imported backup history.

Real PostgreSQL 17 restricted-role dump, multiowner restore, definer/ACL/sequence/
data equality, negative privileges, candidate HBA gate, actual API reads,
all-pool reconnection and crash/cancellation/failure injection evidence are
**pending until executed and recorded**. Static install checks do not satisfy
these gates, nor authorize any production service or database operation.

The service file is delivered with `LoadCredential=pg_service.conf` alongside
both passfiles: the dedicated UID never needs traversal of root-only `/etc/nmcp`.
`PGSERVICEFILE=%d/pg_service.conf` is set by the unit, not in EnvironmentFile.
Run `scripts/install-backup-config --config-dir /root/nmcp-backup-config` after
release migration and the post-migration ACL phase; it captures trusted input,
installs only fixed paths and validates them before enabling the wake timer.
This is an operator deployment action, not routine restore authority.

## Explicit cutover and separate resume

After `restore --backup-id ID --yes --json` reaches externally recorded
`validated`, invoke `restore --operation-id OP --cutover --yes --json` with the
same temporary operator settings. This is root-only and currently supports a
fresh candidate in the same pinned cluster, never an arbitrary existing DB.
It stops all fixed units, rejects source routine connections, updates all five
fixed pool credentials/configuration destinations, verifies the new live API
and Worker pools, and leaves the candidate in maintenance. On any failure units
stay stopped; never manually restart around a failed operation ledger.

Use a separate operator `maintenance resume --yes --json` invocation with
`NMCP_RESTORE_LEDGER_ROOT=/var/lib/nmcp-restore` and operator candidate service
credentials. Imported DB state alone cannot authorize resume: the external
`cutover_verified` ledger and exact candidate cluster/database identity are
required. The old source is never eligible. Re-enable `nmcp-backup.timer` only
after this command succeeds; it was deliberately stopped during cutover.

Restore additionally requires the chosen artifact's digest, size and complete
manifest to match committed success history in the pinned source database.
A valid-looking orphan or an artifact from another DB is not implicit restore
authority; retain it for explicit operator investigation rather than inventing
success history. Candidate PUBLIC CONNECT remains revoked. Explicit cutover
opens CONNECT only for the five fixed intended logins, behind the maintenance
and HBA fences; it never grants CREATEDB, role membership or PUBLIC access.

## Source-only database login boundary

`install-backup-config` invokes the reviewed root operator
`configure-backup-source-hba` while holding the existing deployment locks,
before its first wake or timer activation. It discovers the actual migrated
PG17 source/HBA through a scrubbed local PostgreSQL administrator connection,
pins the active unit/postmaster/database identity, and atomically installs an
owned block allowing the two backup logins only to the configured source DB.
All other databases are rejected for those logins over local/IPv4/IPv6 routes;
no other database's PUBLIC ACL or unrelated role is changed. Includes, mutated
owned blocks, identity drift and stale reload evidence fail closed. Promotion
retargets this exact policy to the new source. Ordinary runtime users never
receive the administrator credential or permission to invoke this installer.

Actual authentication evidence is separate from file/hash/reload checks. The
mandatory PG17 test proves real source authentication and explicit HBA `28000`
rejection for `postgres`/`template1` despite PUBLIC CONNECT, using correct
credentials over Unix/IPv4/IPv6. Root promotion fixtures independently repeat
the other-database denial on their new target.
