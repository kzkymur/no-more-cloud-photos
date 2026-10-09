\set ON_ERROR_STOP on

-- Run as a cluster administrator after the migration login exists. These
-- cluster-global NOLOGIN roles are stable migration targets and have no
-- credentials. Application object privileges are granted transactionally by
-- the embedded migrations, never by this bootstrap file.
SELECT format(
    'CREATE ROLE %I NOLOGIN %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION',
    role_name,inheritance
)
FROM (VALUES
    ('nmcp_runtime','INHERIT'),
    ('nmcp_worker_runtime','INHERIT'),
    ('nmcp_purge_function_owner','INHERIT'),
    ('nmcp_check_runtime','NOINHERIT'),
    ('nmcp_repair_runtime','NOINHERIT'),
    ('nmcp_check_function_owner','NOINHERIT'),
    ('nmcp_repair_function_owner','NOINHERIT')
) AS required(role_name,inheritance)
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=role_name)
\gexec

DO $$
DECLARE role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY[
        'nmcp_runtime',
        'nmcp_worker_runtime',
        'nmcp_purge_function_owner',
        'nmcp_check_runtime',
        'nmcp_repair_runtime',
        'nmcp_check_function_owner',
        'nmcp_repair_function_owner'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname=role_name
              AND NOT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb
              AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
              AND CASE
                  WHEN role_name IN (
                      'nmcp_check_runtime','nmcp_repair_runtime',
                      'nmcp_check_function_owner','nmcp_repair_function_owner'
                  ) THEN NOT rolinherit
                  ELSE rolinherit
              END
        ) THEN
            RAISE EXCEPTION 'unsafe PostgreSQL role attributes: %', role_name;
        END IF;
    END LOOP;
END;
$$;

SELECT :'nmcp_migrator_role'::pg_catalog.regrole;
-- Remove every stable-role nesting edge before restoring the one reviewed
-- Worker-to-common-runtime relationship. Capability roles deliberately have
-- no members until the operational admin logins are designed in issue #20.
SELECT format('REVOKE %I FROM %I', granted.role_name,member.role_name)
FROM (VALUES
    ('nmcp_runtime'),('nmcp_worker_runtime'),('nmcp_purge_function_owner'),
    ('nmcp_check_runtime'),('nmcp_repair_runtime'),
    ('nmcp_check_function_owner'),('nmcp_repair_function_owner')
) AS granted(role_name)
CROSS JOIN (VALUES
    ('nmcp_runtime'),('nmcp_worker_runtime'),('nmcp_purge_function_owner'),
    ('nmcp_check_runtime'),('nmcp_repair_runtime'),
    ('nmcp_check_function_owner'),('nmcp_repair_function_owner')
) AS member(role_name)
WHERE granted.role_name<>member.role_name
  AND NOT (
      granted.role_name='nmcp_runtime'
      AND member.role_name='nmcp_worker_runtime'
  )
\gexec

GRANT nmcp_runtime TO nmcp_worker_runtime
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;
REVOKE nmcp_check_runtime,nmcp_repair_runtime FROM :"nmcp_migrator_role";
GRANT nmcp_purge_function_owner,nmcp_check_function_owner,nmcp_repair_function_owner
    TO :"nmcp_migrator_role" WITH ADMIN FALSE, INHERIT FALSE, SET TRUE;

SELECT :'nmcp_migrator_role'::pg_catalog.regrole::oid AS nmcp_migrator_oid \gset
-- Divide-by-zero makes unexpected direct membership fail under ON_ERROR_STOP.
-- This closes indirect SET ROLE paths among stable roles, permits only the
-- reviewed Worker inheritance edge, and reserves owner membership to the
-- migrator. Check/repair LOGIN membership remains forbidden until issue #20
-- adds the reviewed operational credentials and updates this bootstrap.
SELECT 1 / (NOT EXISTS (
    SELECT 1
    FROM pg_catalog.pg_auth_members AS membership
    WHERE (
        membership.member IN (
            'nmcp_runtime'::pg_catalog.regrole,
            'nmcp_worker_runtime'::pg_catalog.regrole,
            'nmcp_purge_function_owner'::pg_catalog.regrole,
            'nmcp_check_runtime'::pg_catalog.regrole,
            'nmcp_repair_runtime'::pg_catalog.regrole,
            'nmcp_check_function_owner'::pg_catalog.regrole,
            'nmcp_repair_function_owner'::pg_catalog.regrole
        ) AND NOT (
            membership.member='nmcp_worker_runtime'::pg_catalog.regrole
            AND membership.roleid='nmcp_runtime'::pg_catalog.regrole
            AND NOT membership.admin_option
            AND membership.inherit_option
            AND NOT membership.set_option
        )
    ) OR (
        membership.roleid IN (
            'nmcp_purge_function_owner'::pg_catalog.regrole,
            'nmcp_check_function_owner'::pg_catalog.regrole,
            'nmcp_repair_function_owner'::pg_catalog.regrole
        ) AND (
            membership.member<>:nmcp_migrator_oid
            OR membership.admin_option
            OR membership.inherit_option
            OR NOT membership.set_option
        )
    ) OR (
        membership.roleid IN (
            'nmcp_check_runtime'::pg_catalog.regrole,
            'nmcp_repair_runtime'::pg_catalog.regrole
        )
    )
))::integer AS nmcp_safe_stable_role_membership;
