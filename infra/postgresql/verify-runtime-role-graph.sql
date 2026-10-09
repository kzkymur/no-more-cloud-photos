\set ON_ERROR_STOP on

-- Independent, object-agnostic proof of the hardened production role graph.
-- This intentionally works even when an application migration failed before
-- creating its first table or function.
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
    FOREACH role_name IN ARRAY ARRAY['nmcp_migrator','nmcp_api','nmcp_worker'] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname=role_name
              AND rolcanlogin AND rolinherit AND NOT rolsuper AND NOT rolcreatedb
              AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
        ) THEN
            RAISE EXCEPTION 'unsafe PostgreSQL login role attributes: %', role_name;
        END IF;
    END LOOP;
END;
$$;

DO $$
BEGIN
    IF EXISTS (
        WITH managed(roleid) AS (VALUES
            ('nmcp_runtime'::pg_catalog.regrole::oid),
            ('nmcp_worker_runtime'::pg_catalog.regrole::oid),
            ('nmcp_purge_function_owner'::pg_catalog.regrole::oid),
            ('nmcp_check_runtime'::pg_catalog.regrole::oid),
            ('nmcp_repair_runtime'::pg_catalog.regrole::oid),
            ('nmcp_check_function_owner'::pg_catalog.regrole::oid),
            ('nmcp_repair_function_owner'::pg_catalog.regrole::oid),
            ('nmcp_migrator'::pg_catalog.regrole::oid),
            ('nmcp_api'::pg_catalog.regrole::oid),
            ('nmcp_worker'::pg_catalog.regrole::oid)
        ), allowed(roleid,member,admin_option,inherit_option,set_option) AS (VALUES
            ('nmcp_runtime'::pg_catalog.regrole::oid,
             'nmcp_worker_runtime'::pg_catalog.regrole::oid,false,true,false),
            ('nmcp_purge_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_check_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_repair_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_runtime'::pg_catalog.regrole::oid,
             'nmcp_api'::pg_catalog.regrole::oid,false,true,false),
            ('nmcp_worker_runtime'::pg_catalog.regrole::oid,
             'nmcp_worker'::pg_catalog.regrole::oid,false,true,false)
        )
        SELECT 1
        FROM pg_catalog.pg_auth_members AS actual
        WHERE (actual.roleid IN (SELECT roleid FROM managed)
               OR actual.member IN (SELECT roleid FROM managed))
          AND NOT EXISTS (
              SELECT 1 FROM allowed
              WHERE allowed.roleid=actual.roleid
                AND allowed.member=actual.member
                AND allowed.admin_option=actual.admin_option
                AND allowed.inherit_option=actual.inherit_option
                AND allowed.set_option=actual.set_option
          )
    ) OR EXISTS (
        WITH allowed(roleid,member,admin_option,inherit_option,set_option) AS (VALUES
            ('nmcp_runtime'::pg_catalog.regrole::oid,
             'nmcp_worker_runtime'::pg_catalog.regrole::oid,false,true,false),
            ('nmcp_purge_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_check_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_repair_function_owner'::pg_catalog.regrole::oid,
             'nmcp_migrator'::pg_catalog.regrole::oid,false,false,true),
            ('nmcp_runtime'::pg_catalog.regrole::oid,
             'nmcp_api'::pg_catalog.regrole::oid,false,true,false),
            ('nmcp_worker_runtime'::pg_catalog.regrole::oid,
             'nmcp_worker'::pg_catalog.regrole::oid,false,true,false)
        )
        SELECT 1 FROM allowed
        WHERE NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_auth_members AS actual
            WHERE actual.roleid=allowed.roleid
              AND actual.member=allowed.member
              AND actual.admin_option=allowed.admin_option
              AND actual.inherit_option=allowed.inherit_option
              AND actual.set_option=allowed.set_option
        )
    ) THEN
        RAISE EXCEPTION 'managed PostgreSQL role graph differs from hardened allowlist';
    END IF;
END;
$$;
