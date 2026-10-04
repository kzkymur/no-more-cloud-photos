# Database migrations

Forward-only PostgreSQL migrations live in this directory and are embedded into
the administrative binary. Migration filenames use `NNNN_description.sql`, where
the version is positive and the description contains lowercase ASCII letters,
digits, underscores, or hyphens.

Never edit a migration after it has shipped. Add a higher-numbered migration to
repair or evolve the schema. Applied file contents are protected by a SHA-256
checksum in `schema_migrations`; rollback is performed by restoring a tested
backup or by deploying a forward repair migration.

Migrations run in their own transaction by default. A statement PostgreSQL
forbids in a transaction must use this exact first line:

```sql
-- nmcp:transaction=off idempotent=true
```

Such a migration must contain exactly one executable statement and be safe to
rerun after an interrupted attempt. The migrator rejects explicit transaction
control in every migration and rejects transaction-off files without that
explicit idempotence marker.

`0001_initial_schema.sql` is the transactional, forward-only Core metadata
baseline. It creates the relational invariants and singleton configuration/feed/
maintenance seeds. It deliberately creates no profile rows: complete bundled
profile recipes are owned by issue #6, after processor/schema capability
validation exists.
