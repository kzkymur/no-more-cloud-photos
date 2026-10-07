\set ON_ERROR_STOP on

-- Run as a cluster administrator after the migration login exists. These
-- cluster-global NOLOGIN roles are stable migration targets and have no
-- credentials. Application object privileges are granted transactionally by
-- the embedded migrations, never by this bootstrap file.
SELECT format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', role_name)
FROM (VALUES
    ('nmcp_runtime'),
    ('nmcp_worker_runtime'),
    ('nmcp_purge_function_owner')
) AS required(role_name)
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=role_name)
\gexec

DO $$
DECLARE role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY[
        'nmcp_runtime',
        'nmcp_worker_runtime',
        'nmcp_purge_function_owner'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname=role_name
              AND NOT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb
              AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
              AND (role_name='nmcp_purge_function_owner' OR rolinherit)
        ) THEN
            RAISE EXCEPTION 'unsafe PostgreSQL role attributes: %', role_name;
        END IF;
    END LOOP;
END;
$$;

SELECT :'nmcp_migrator_role'::pg_catalog.regrole;
GRANT nmcp_runtime TO nmcp_worker_runtime;
REVOKE nmcp_purge_function_owner FROM nmcp_runtime,nmcp_worker_runtime;
REVOKE nmcp_runtime,nmcp_worker_runtime FROM nmcp_purge_function_owner;
GRANT nmcp_purge_function_owner TO :"nmcp_migrator_role";

SELECT :'nmcp_migrator_role'::pg_catalog.regrole::oid AS nmcp_migrator_oid \gset
-- Divide-by-zero makes unexpected direct membership fail under ON_ERROR_STOP.
-- This closes indirect SET ROLE paths from either runtime group to the owner.
SELECT 1 / (NOT EXISTS (
    SELECT 1
    FROM pg_catalog.pg_auth_members AS membership
    WHERE (membership.member='nmcp_runtime'::pg_catalog.regrole)
       OR (membership.member='nmcp_worker_runtime'::pg_catalog.regrole
           AND membership.roleid<>'nmcp_runtime'::pg_catalog.regrole)
       OR (membership.member='nmcp_purge_function_owner'::pg_catalog.regrole)
       OR (membership.roleid='nmcp_purge_function_owner'::pg_catalog.regrole
           AND membership.member<>:nmcp_migrator_oid)
))::integer AS nmcp_safe_stable_role_membership;
