\set ON_ERROR_STOP on

-- Execute as the local PostgreSQL cluster administrator while connected to the
-- intended nmcp database. Passwords are deliberately set separately with
-- interactive \password commands and never stored in this repository.
BEGIN;

SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', role_name)
FROM (VALUES
    ('nmcp_migrator'),
    ('nmcp_api'),
    ('nmcp_worker')
) AS required(role_name)
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=role_name)
\gexec

DO $$
DECLARE role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['nmcp_migrator','nmcp_api','nmcp_worker'] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname=role_name
              AND rolcanlogin AND rolinherit AND NOT rolsuper AND NOT rolcreatedb
              AND NOT rolcreaterole AND NOT rolreplication
              AND NOT rolbypassrls
        ) THEN
            RAISE EXCEPTION 'unsafe PostgreSQL login role attributes: %', role_name;
        END IF;
    END LOOP;
END;
$$;

\set nmcp_migrator_role nmcp_migrator
\set nmcp_api_role nmcp_api
\set nmcp_worker_role nmcp_worker
\set nmcp_roles_in_transaction 1
\ir ../postgresql/runtime-roles.sql

SELECT format('ALTER DATABASE %I OWNER TO nmcp_migrator', current_database())
\gexec
ALTER SCHEMA public OWNER TO nmcp_migrator;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON DATABASE :DBNAME FROM PUBLIC;
GRANT CONNECT ON DATABASE :DBNAME TO nmcp_migrator,nmcp_api,nmcp_worker;

COMMIT;
