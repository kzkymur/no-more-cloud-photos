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

`0002_profile_recipes.sql` adds the closed candidate MIME/source registry,
immutable evidence-backed output option certifications, strict schema-v1 recipe
validation on every profile write, and the bundled `standard/v1` and
`thumbnail/v1` drafts. Candidate membership is not executable capability: no
certifications are seeded, and activation fails until later codec fixture
evidence records an input/output option envelope.

The `0001` upgrade preflight preserves compatible custom drafts, but fails closed
for incompatible rows, uncertified active rows, or bundled `(key,version)`
conflicts. The shipped `0001` state contains zero profiles and exposes no profile
creation API, so such rows indicate unsupported manual database mutation; repair
uses a reviewed forward migration or tested backup restore rather than silently
adopting or rewriting immutable provenance.
