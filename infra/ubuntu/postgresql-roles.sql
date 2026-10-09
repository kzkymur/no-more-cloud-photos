\set ON_ERROR_STOP on

-- Execute as the local PostgreSQL cluster administrator while connected to the
-- intended nmcp database. Passwords are deliberately set separately with
-- interactive \password commands and never stored in this repository.
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
\ir ../postgresql/runtime-roles.sql

REVOKE nmcp_runtime,nmcp_worker_runtime,nmcp_purge_function_owner,
    nmcp_check_runtime,nmcp_repair_runtime,
    nmcp_check_function_owner,nmcp_repair_function_owner FROM nmcp_api;
REVOKE nmcp_runtime,nmcp_worker_runtime,nmcp_purge_function_owner,
    nmcp_check_runtime,nmcp_repair_runtime,
    nmcp_check_function_owner,nmcp_repair_function_owner FROM nmcp_worker;
GRANT nmcp_runtime TO nmcp_api
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;
GRANT nmcp_worker_runtime TO nmcp_worker
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_auth_members AS membership
        WHERE (membership.member='nmcp_migrator'::pg_catalog.regrole
               AND (membership.roleid NOT IN (
                       'nmcp_purge_function_owner'::pg_catalog.regrole,
                       'nmcp_check_function_owner'::pg_catalog.regrole,
                       'nmcp_repair_function_owner'::pg_catalog.regrole
                   ) OR membership.admin_option OR membership.inherit_option
                   OR NOT membership.set_option))
           OR (membership.member='nmcp_api'::pg_catalog.regrole
               AND (membership.roleid<>'nmcp_runtime'::pg_catalog.regrole
                    OR membership.admin_option OR NOT membership.inherit_option
                    OR membership.set_option))
           OR (membership.member='nmcp_worker'::pg_catalog.regrole
               AND (membership.roleid<>'nmcp_worker_runtime'::pg_catalog.regrole
                    OR membership.admin_option OR NOT membership.inherit_option
                    OR membership.set_option))
    ) OR pg_catalog.pg_has_role('nmcp_migrator','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_migrator','nmcp_repair_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_api','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_api','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_api','nmcp_repair_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker','nmcp_repair_runtime','MEMBER') THEN
        RAISE EXCEPTION 'unexpected PostgreSQL application role membership';
    END IF;
END;
$$;

SELECT format('ALTER DATABASE %I OWNER TO nmcp_migrator', current_database())
\gexec
ALTER SCHEMA public OWNER TO nmcp_migrator;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON DATABASE :DBNAME FROM PUBLIC;
GRANT CONNECT ON DATABASE :DBNAME TO nmcp_migrator,nmcp_api,nmcp_worker;
