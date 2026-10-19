-- Runtime processes must inspect migration readiness but must never mutate
-- migration history or apply migrations.

GRANT SELECT ON TABLE schema_migrations TO nmcp_runtime;
