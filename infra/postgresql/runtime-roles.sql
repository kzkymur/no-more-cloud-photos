\set ON_ERROR_STOP on

-- Run as a cluster administrator after the migration login exists. These
-- cluster-global NOLOGIN roles are stable migration targets and have no
-- credentials. Application object privileges are granted transactionally by
-- the embedded migrations. Backup-only ACLs require the explicit post-migration
-- phase below; default bootstrap remains independent of application objects.
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

-- Dedicated routine credentials have no role memberships, not even runtime
-- membership. Passwords/HBA provisioning remain an operator responsibility.
SELECT format('CREATE ROLE %I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', role_name)
FROM (VALUES ('nmcp_backup_dump'),('nmcp_backup_control')) AS required(role_name)
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=role_name)
\gexec
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname IN ('nmcp_backup_dump','nmcp_backup_control')
          AND (NOT rolcanlogin OR rolinherit OR rolsuper OR rolcreatedb
               OR rolcreaterole OR rolreplication OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION 'unsafe backup login attributes';
    END IF;
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
    ('nmcp_backup_dump'::pg_catalog.regrole::oid),
    ('nmcp_backup_control'::pg_catalog.regrole::oid),
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
    ) AND (
        SELECT pg_catalog.count(*)
        FROM pg_catalog.pg_auth_members AS actual
        WHERE actual.member=:'nmcp_migrator_role'::pg_catalog.regrole
          AND actual.roleid IN (
              'nmcp_purge_function_owner'::pg_catalog.regrole,
              'nmcp_check_function_owner'::pg_catalog.regrole,
              'nmcp_repair_function_owner'::pg_catalog.regrole
          )
          AND actual.inherit_option
    ) IN (0,3)
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


-- This phase MUST be explicitly invoked after migrations by the local cluster
-- administrator. Never silently accept a partially migrated application.
\if :{?nmcp_backup_grants_after_migration}
\else
\set nmcp_backup_grants_after_migration FALSE
\endif
\if :nmcp_backup_grants_after_migration
CREATE TEMPORARY TABLE nmcp_backup_owner(roleid oid PRIMARY KEY) ON COMMIT DROP;
INSERT INTO nmcp_backup_owner VALUES
    (:'nmcp_migrator_role'::pg_catalog.regrole),
    ('nmcp_purge_function_owner'::pg_catalog.regrole),
    ('nmcp_check_function_owner'::pg_catalog.regrole),
    ('nmcp_repair_function_owner'::pg_catalog.regrole);

DO $$
BEGIN
    -- No unreviewed schema/owner, row-security, foreign source, or large-object
    -- omission may turn a partial dump into apparent success.
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_namespace n
        WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
          AND n.nspowner NOT IN (SELECT roleid FROM nmcp_backup_owner)
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_class c
        JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
        WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
          AND (c.relowner NOT IN (SELECT roleid FROM nmcp_backup_owner)
               OR c.relrowsecurity OR c.relforcerowsecurity OR c.relkind='f')
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_proc p
        JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
        WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
          AND p.proowner NOT IN (SELECT roleid FROM nmcp_backup_owner)
    ) OR EXISTS (SELECT 1 FROM pg_catalog.pg_largeobject_metadata) THEN
        RAISE EXCEPTION 'unsupported backup schema/owner/RLS/foreign table/large object inventory';
    END IF;
    IF to_regclass('public.schema_migrations') IS NULL
       OR to_regclass('public.system_config') IS NULL
       OR to_regclass('public.maintenance_state') IS NULL
       OR to_regclass('public.backup_runs') IS NULL
       OR to_regclass('public.admin_audit') IS NULL THEN
        RAISE EXCEPTION 'backup grants require completed application migrations';
    END IF;
END;
$$;

SELECT format('GRANT CONNECT ON DATABASE %I TO nmcp_backup_dump,nmcp_backup_control', current_database())
\gexec
SELECT format('GRANT USAGE ON SCHEMA %I TO nmcp_backup_dump', nspname)
FROM pg_catalog.pg_namespace
WHERE nspname <> 'information_schema' AND nspname !~ '^pg_'
ORDER BY nspname
\gexec
-- Explicit catalog inventory across ALL supported owners, including private
-- checker/repair/admin/progress relations and sequence state.
SELECT format('GRANT SELECT ON %s %I.%I TO nmcp_backup_dump',
              CASE WHEN c.relkind='S' THEN 'SEQUENCE' ELSE 'TABLE' END,
              n.nspname,c.relname)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
  AND c.relkind IN ('r','p','v','m','S')
ORDER BY n.nspname,c.relname
\gexec
-- Default privileges are creator-specific, NOT inherited from the migrator.
-- Include all three definer owners even when they currently own only functions.
SELECT format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT SELECT ON %s TO nmcp_backup_dump',
              r.rolname,n.nspname,kind)
FROM nmcp_backup_owner o
JOIN pg_catalog.pg_roles r ON r.oid=o.roleid
CROSS JOIN pg_catalog.pg_namespace n
CROSS JOIN (VALUES ('TABLES'),('SEQUENCES')) AS kinds(kind)
WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
ORDER BY r.rolname,n.nspname,kind
\gexec
GRANT USAGE ON SCHEMA public TO nmcp_backup_control;
GRANT SELECT ON public.system_config,public.maintenance_state,public.schema_migrations
    TO nmcp_backup_control;
GRANT SELECT,INSERT,UPDATE ON public.backup_runs TO nmcp_backup_control;
GRANT INSERT ON public.admin_audit TO nmcp_backup_control;
-- No control defaults, DELETE, sequence privileges, or owner memberships.

-- Sole audited application capability exception, pinned to migration0022 at
-- author c48a11f5af0a9843348e2fd69b214ef1d9b453b4. A matching name is NOT enough.
CREATE TEMPORARY TABLE nmcp_backup_fence(oid oid PRIMARY KEY) ON COMMIT DROP;
INSERT INTO nmcp_backup_fence
SELECT p.oid FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname='public' AND p.proname='nmcp_backup_normal_fence'
  AND p.pronargs=0;
SELECT 1 / ((NOT EXISTS (SELECT 1 FROM nmcp_backup_fence) OR EXISTS (
    SELECT 1 FROM pg_catalog.pg_proc p
    JOIN nmcp_backup_fence f ON f.oid=p.oid
    WHERE p.proowner=:'nmcp_migrator_role'::regrole
      AND p.prolang=(SELECT oid FROM pg_catalog.pg_language WHERE lanname='plpgsql')
      AND p.prokind='f' AND p.prorettype='pg_catalog.bool'::regtype
      AND p.proargtypes=''::oidvector AND p.proallargtypes IS NULL
      AND p.proargnames IS NULL AND p.pronargdefaults=0 AND p.proargdefaults IS NULL
      AND p.prosecdef AND NOT p.proretset AND NOT p.proisstrict
      AND NOT p.proleakproof AND p.provolatile='v' AND p.proparallel='u'
      AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
      AND p.prosrc=$fencebody$
        DECLARE current_mode text;
        BEGIN
            SELECT mode INTO current_mode FROM public.maintenance_state
                WHERE id=1 FOR SHARE NOWAIT;
            RETURN COALESCE(current_mode='normal', false);
        END;
        $fencebody$
      AND (SELECT count(*) FROM pg_catalog.pg_proc q
           JOIN pg_catalog.pg_namespace n ON n.oid=q.pronamespace
           WHERE n.nspname='public' AND q.proname='nmcp_backup_normal_fence')=1
      AND NOT EXISTS (
          SELECT 1 FROM pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
          WHERE a.grantee NOT IN (p.proowner,'nmcp_backup_control'::regrole)
             OR a.grantor<>p.proowner OR a.privilege_type<>'EXECUTE' OR a.is_grantable
      )
)))::integer AS nmcp_exact_backup_normal_fence;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM nmcp_backup_fence)
       AND EXISTS (SELECT 1 FROM public.schema_migrations WHERE version>=22) THEN
        RAISE EXCEPTION 'migration22 requires exact backup normal fence';
    END IF;
END;
$$;
SELECT 'GRANT EXECUTE ON FUNCTION public.nmcp_backup_normal_fence() TO nmcp_backup_control'
FROM nmcp_backup_fence
\gexec


DO $$
DECLARE obj record; login text; priv text; expected boolean;
BEGIN
    -- Effective privilege predicates do not detect delegation authority.
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_class c
        CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
        WHERE a.grantee IN ('nmcp_backup_dump'::regrole,'nmcp_backup_control'::regrole)
          AND a.is_grantable
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_attribute c
        CROSS JOIN LATERAL pg_catalog.aclexplode(c.attacl) a
        WHERE a.grantee IN ('nmcp_backup_dump'::regrole,'nmcp_backup_control'::regrole)
          AND a.is_grantable
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_default_acl d
        CROSS JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) a
        WHERE a.grantee IN ('nmcp_backup_dump'::regrole,'nmcp_backup_control'::regrole)
          AND (a.is_grantable OR a.privilege_type <> 'SELECT'
               OR a.grantee='nmcp_backup_control'::regrole)
    ) THEN
        RAISE EXCEPTION 'unsafe backup delegation/default privileges';
    END IF;
    FOREACH login IN ARRAY ARRAY['nmcp_backup_dump','nmcp_backup_control'] LOOP
        -- No backup role may invoke an application capability. In particular,
        -- table ACLs alone do not contain SECURITY DEFINER mutations. Include
        -- effective PUBLIC grants, not just direct ACL entries. Invoker trigger
        -- functions cannot be called as ordinary routines; immutable/stable
        -- invoker validators remain usable by CHECK constraints.
        IF EXISTS (
            SELECT 1 FROM pg_catalog.pg_proc p
            JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
            WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
              AND (p.prosecdef OR p.prokind='p' OR
                   (p.provolatile='v' AND p.prorettype NOT IN
                       ('pg_catalog.trigger'::regtype,'pg_catalog.event_trigger'::regtype)))
              AND has_function_privilege(login,p.oid,'EXECUTE')
              AND NOT (login='nmcp_backup_control' AND p.oid IN (SELECT oid FROM nmcp_backup_fence))
        ) OR EXISTS (
            SELECT 1 FROM pg_catalog.pg_proc p
            JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
            CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
            WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
              AND a.grantee=login::regrole
              AND NOT (login='nmcp_backup_control' AND p.oid IN (SELECT oid FROM nmcp_backup_fence))
        ) THEN
            RAISE EXCEPTION 'unsafe backup application function EXECUTE: %',login;
        END IF;
        IF has_database_privilege(login,current_database(),'CREATE')
           OR has_database_privilege(login,current_database(),'TEMP') THEN
            RAISE EXCEPTION 'unsafe backup database privileges: %',login;
        END IF;
        FOR obj IN
            SELECT oid,nspname FROM pg_catalog.pg_namespace
            WHERE nspname <> 'information_schema' AND nspname !~ '^pg_'
        LOOP
            IF has_schema_privilege(login,obj.oid,'CREATE') THEN
                RAISE EXCEPTION 'unsafe backup schema CREATE: %',login;
            END IF;
        END LOOP;
        FOR obj IN
            SELECT c.oid,c.relkind,n.nspname,c.relname
            FROM pg_catalog.pg_class c
            JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
              AND c.relkind IN ('r','p','v','m','S')
        LOOP
            IF obj.relkind='S' THEN
                FOREACH priv IN ARRAY ARRAY['SELECT','USAGE','UPDATE'] LOOP
                    expected := login='nmcp_backup_dump' AND priv='SELECT';
                    IF has_sequence_privilege(login,obj.oid,priv) IS DISTINCT FROM expected THEN
                        RAISE EXCEPTION 'backup sequence privilege mismatch: % %.% %',login,obj.nspname,obj.relname,priv;
                    END IF;
                END LOOP;
            ELSE
                FOREACH priv IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
                    expected := (login='nmcp_backup_dump' AND priv='SELECT') OR
                        (login='nmcp_backup_control' AND obj.nspname='public' AND (
                            (obj.relname IN ('system_config','maintenance_state','schema_migrations') AND priv='SELECT') OR
                            (obj.relname='backup_runs' AND priv IN ('SELECT','INSERT','UPDATE')) OR
                            (obj.relname='admin_audit' AND priv='INSERT')));
                    IF has_table_privilege(login,obj.oid,priv) IS DISTINCT FROM expected THEN
                        RAISE EXCEPTION 'backup table privilege mismatch: % %.% %',login,obj.nspname,obj.relname,priv;
                    END IF;
                    IF priv IN ('SELECT','INSERT','UPDATE','REFERENCES') AND NOT expected
                       AND has_any_column_privilege(login,obj.oid,priv) THEN
                        RAISE EXCEPTION 'unexpected backup column privilege: % %.% %',login,obj.nspname,obj.relname,priv;
                    END IF;
                END LOOP;
            END IF;
        END LOOP;
    END LOOP;
END;
$$;
-- Stable, reviewable receipt: relation owner, kind, raw ACL and default ACLs.
SELECT n.nspname,c.relname,c.relkind,pg_catalog.pg_get_userbyid(c.relowner) AS owner,c.relacl
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
  AND c.relkind IN ('r','p','v','m','S') ORDER BY n.nspname,c.relname;
SELECT pg_catalog.pg_get_userbyid(d.defaclrole) AS owner,n.nspname,d.defaclobjtype,d.defaclacl
FROM pg_catalog.pg_default_acl d JOIN pg_catalog.pg_namespace n ON n.oid=d.defaclnamespace
WHERE d.defaclrole IN (SELECT roleid FROM nmcp_backup_owner)
ORDER BY owner,n.nspname,d.defaclobjtype;
\endif

\if :{?nmcp_roles_in_transaction}
\else
COMMIT;
\endif
