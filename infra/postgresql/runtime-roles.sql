\set ON_ERROR_STOP on

-- Run as a cluster administrator after the migration login exists. These
-- cluster-global NOLOGIN roles are stable migration targets and have no
-- credentials. Application object privileges are granted transactionally by
-- the embedded migrations, never by this bootstrap file.
--
-- Callers that already opened a transaction set nmcp_roles_in_transaction.
-- Production callers also set nmcp_api_role and nmcp_worker_role so this one
-- transaction can validate the complete managed graph.
\if :{?nmcp_migration_compatibility}
\else
\set nmcp_migration_compatibility FALSE
\endif
\if :nmcp_migration_compatibility
\set nmcp_owner_inherit TRUE
\else
\set nmcp_owner_inherit FALSE
\endif

\if :{?nmcp_roles_in_transaction}
\else
BEGIN;
\endif

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

CREATE TEMPORARY TABLE nmcp_managed_role(roleid oid PRIMARY KEY) ON COMMIT DROP;
CREATE TEMPORARY TABLE nmcp_allowed_membership(
    roleid oid NOT NULL,
    member oid NOT NULL,
    admin_option boolean NOT NULL,
    inherit_option boolean NOT NULL,
    set_option boolean NOT NULL,
    PRIMARY KEY(roleid,member)
) ON COMMIT DROP;

INSERT INTO nmcp_managed_role(roleid)
SELECT role_oid
FROM (VALUES
    ('nmcp_runtime'::pg_catalog.regrole::oid),
    ('nmcp_worker_runtime'::pg_catalog.regrole::oid),
    ('nmcp_purge_function_owner'::pg_catalog.regrole::oid),
    ('nmcp_check_runtime'::pg_catalog.regrole::oid),
    ('nmcp_repair_runtime'::pg_catalog.regrole::oid),
    ('nmcp_check_function_owner'::pg_catalog.regrole::oid),
    ('nmcp_repair_function_owner'::pg_catalog.regrole::oid),
    (:'nmcp_migrator_role'::pg_catalog.regrole::oid)
) AS managed(role_oid);

INSERT INTO nmcp_allowed_membership
    (roleid,member,admin_option,inherit_option,set_option)
VALUES
    ('nmcp_runtime'::pg_catalog.regrole,
     'nmcp_worker_runtime'::pg_catalog.regrole,false,true,false),
    ('nmcp_purge_function_owner'::pg_catalog.regrole,
     :'nmcp_migrator_role'::pg_catalog.regrole,false,:nmcp_owner_inherit,true),
    ('nmcp_check_function_owner'::pg_catalog.regrole,
     :'nmcp_migrator_role'::pg_catalog.regrole,false,:nmcp_owner_inherit,true),
    ('nmcp_repair_function_owner'::pg_catalog.regrole,
     :'nmcp_migrator_role'::pg_catalog.regrole,false,:nmcp_owner_inherit,true);

\if :{?nmcp_api_role}
INSERT INTO nmcp_managed_role VALUES (:'nmcp_api_role'::pg_catalog.regrole::oid);
INSERT INTO nmcp_allowed_membership VALUES (
    'nmcp_runtime'::pg_catalog.regrole,
    :'nmcp_api_role'::pg_catalog.regrole,false,true,false
);
\endif

\if :{?nmcp_worker_role}
INSERT INTO nmcp_managed_role VALUES (:'nmcp_worker_role'::pg_catalog.regrole::oid);
INSERT INTO nmcp_allowed_membership VALUES (
    'nmcp_worker_runtime'::pg_catalog.regrole,
    :'nmcp_worker_role'::pg_catalog.regrole,false,true,false
);
\endif

-- Compatibility mode may start only from the exact hardened graph or from an
-- exact preexisting compatibility graph left by interruption. In particular,
-- it never normalizes a rogue edge or an ADMIN/SET option before enabling
-- inheritance. The enclosing transaction leaves the graph unchanged on error.
\if :nmcp_migration_compatibility
SELECT 1 / ((
    NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_auth_members AS actual
        WHERE (
            actual.roleid IN (SELECT roleid FROM nmcp_managed_role)
            OR actual.member IN (SELECT roleid FROM nmcp_managed_role)
        ) AND NOT EXISTS (
            SELECT 1 FROM nmcp_allowed_membership AS allowed
            WHERE allowed.roleid=actual.roleid
              AND allowed.member=actual.member
              AND allowed.admin_option=actual.admin_option
              AND allowed.set_option=actual.set_option
              AND (
                  allowed.inherit_option=actual.inherit_option
                  OR (
                      allowed.roleid IN (
                          'nmcp_purge_function_owner'::pg_catalog.regrole,
                          'nmcp_check_function_owner'::pg_catalog.regrole,
                          'nmcp_repair_function_owner'::pg_catalog.regrole
                      )
                      AND allowed.member=:'nmcp_migrator_role'::pg_catalog.regrole
                      AND NOT actual.inherit_option
                  )
              )
        )
    ) AND NOT EXISTS (
        SELECT 1
        FROM nmcp_allowed_membership AS allowed
        WHERE NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_auth_members AS actual
            WHERE actual.roleid=allowed.roleid
              AND actual.member=allowed.member
              AND actual.admin_option=allowed.admin_option
              AND actual.set_option=allowed.set_option
              AND (
                  actual.inherit_option=allowed.inherit_option
                  OR (
                      allowed.roleid IN (
                          'nmcp_purge_function_owner'::pg_catalog.regrole,
                          'nmcp_check_function_owner'::pg_catalog.regrole,
                          'nmcp_repair_function_owner'::pg_catalog.regrole
                      )
                      AND allowed.member=:'nmcp_migrator_role'::pg_catalog.regrole
                      AND NOT actual.inherit_option
                  )
              )
        )
    )
))::integer AS nmcp_safe_pre_migration_role_graph;
\endif

GRANT nmcp_runtime TO nmcp_worker_runtime
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;
GRANT nmcp_purge_function_owner,nmcp_check_function_owner,nmcp_repair_function_owner
    TO :"nmcp_migrator_role" WITH ADMIN FALSE, INHERIT :nmcp_owner_inherit, SET TRUE;
\if :{?nmcp_api_role}
GRANT nmcp_runtime TO :"nmcp_api_role"
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;
\endif
\if :{?nmcp_worker_role}
GRANT nmcp_worker_runtime TO :"nmcp_worker_role"
    WITH ADMIN FALSE, INHERIT TRUE, SET FALSE;
\endif

-- Every direct edge touching a managed login or stable role must match this
-- exact allowlist. Closing both sides of every edge also closes all transitive
-- paths from arbitrary cluster roles into the migrator, owners, capabilities,
-- and application runtimes.
SELECT 1 / ((
    NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_auth_members AS actual
        WHERE (
            actual.roleid IN (SELECT roleid FROM nmcp_managed_role)
            OR actual.member IN (SELECT roleid FROM nmcp_managed_role)
        ) AND NOT EXISTS (
            SELECT 1 FROM nmcp_allowed_membership AS allowed
            WHERE allowed.roleid=actual.roleid
              AND allowed.member=actual.member
              AND allowed.admin_option=actual.admin_option
              AND allowed.inherit_option=actual.inherit_option
              AND allowed.set_option=actual.set_option
        )
    ) AND NOT EXISTS (
        SELECT 1
        FROM nmcp_allowed_membership AS allowed
        WHERE NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_auth_members AS actual
            WHERE actual.roleid=allowed.roleid
              AND actual.member=allowed.member
              AND actual.admin_option=allowed.admin_option
              AND actual.inherit_option=allowed.inherit_option
              AND actual.set_option=allowed.set_option
        )
    )
))::integer AS nmcp_safe_managed_role_graph;

\if :{?nmcp_roles_in_transaction}
\else
COMMIT;
\endif
