# Core development and configuration

## Pinned dependencies

- Go `1.27.1`. Official download metadata: <https://go.dev/dl/?mode=json>.
  The Linux amd64 archive SHA-256 is
  `63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445`.
- `github.com/jackc/pgx/v5` `v5.11.0`, locked with all transitive module
  checksums in `go.sum`.
- CI uses immutable action commits: `actions/checkout` v7.0.1 at
  `3d3c42e5aac5ba805825da76410c181273ba90b1` and `actions/setup-go` v7.0.0
  at `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`.
- PostgreSQL integration tests run against the `postgres:17-alpine` service.
  This is a test entry point, not a claim that later backup/restore and
  deployment compatibility have already passed.

The repository sets `go 1.27.0` as its language/toolchain floor while CI pins
the security patch release `1.27.1`. Set `GOTOOLCHAIN=local` so an unexpected
toolchain is never downloaded implicitly.

## Commands

```text
go mod verify
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./...
TEST_DATABASE_URL='postgres://...' go test -v -count=1 -run '^TestMigratorIntegration$' ./internal/database
```

`nmcp-admin migrate status` is read-only. It exits `4` when migrations are
pending and `1` for connection failure or migration-history drift.
`nmcp-admin migrate up` uses the database advisory lock and applies embedded
forward migrations. Issue #4 owns the initial Core schema, so issue #3 embeds
no schema SQL and only establishes the runner/history contract.

Migrations are transactional by default and are applied one at a time with
their history row in the same transaction. A PostgreSQL operation that cannot
run in a transaction must begin with the exact first line
`-- nmcp:transaction=off idempotent=true`. The explicit idempotency promise is
required, and such a file contains exactly one executable statement so the
PostgreSQL simple-query protocol cannot create an implicit multi-statement
transaction. Every migration rejects explicit transaction-control statements;
only the runner owns transaction boundaries. The runner writes a
dirty/in-progress row before executing outside a transaction, and after a
process crash reruns only that known, checksum-matched,
dirty tail migration. Unknown, changed, unsafe, or non-tail dirty history is
drift and blocks readiness. One session-level advisory lock covers validation,
transactional migrations, non-transactional execution, and recovery.

## Environment

| Variable | API | Worker | Admin | Rule/default |
|---|---:|---:|---:|---|
| `NMCP_DATABASE_URL` | required | required | required | PostgreSQL DSN; secret, never logged. |
| `NMCP_STORAGE_ROOT` | required | required | - | Absolute clean path; never logged. |
| `NMCP_FILE_BASE_URL` | required | - | - | Absolute HTTPS URL without credentials, query, or fragment. |
| `NMCP_CURSOR_HMAC_KEY` | required | - | - | At least 32 bytes; secret, never logged. |
| `NMCP_API_ADDR` | optional | - | - | `127.0.0.1:8080`; explicit host and valid port. |
| `NMCP_LOG_LEVEL` | optional | optional | optional | `info`; one of `debug`, `info`, `warn`, `error`. |
| `NMCP_SHUTDOWN_TIMEOUT` | optional | optional | optional | `30s`; range `1s` through `5m`. |
| `TEST_DATABASE_URL` | tests | tests | tests | Enables isolated-schema real PostgreSQL migration tests. |

API `GET /healthz` checks only the process handler. `GET /readyz` checks the
database, migration currency/checksums, and a temporary create/sync/remove in a
non-symlink storage root. Dependency failures return only the stable
`unavailable` error and do not expose DSNs, paths, or SQL details.

API and Worker handle SIGINT/SIGTERM. The API stops intake and drains HTTP
requests within the configured timeout; the Worker stops accepting future work
(the job loop is added in issue #10) and closes its database pool.
