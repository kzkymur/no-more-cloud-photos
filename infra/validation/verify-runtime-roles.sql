\set ON_ERROR_STOP on

DO $$
DECLARE
    completion regprocedure := pg_catalog.to_regprocedure(
        'public.nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)'
    );
    cleanup_completion regprocedure := pg_catalog.to_regprocedure(
        'public.nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)'
    );
    table_owner name;
    function_owner name;
    security_definer boolean;
    function_config text[];
    cleanup_table_owner name;
    cleanup_function_owner name;
    cleanup_security_definer boolean;
    cleanup_function_config text[];
    cleanup_public_execute boolean;
BEGIN
    IF completion IS NULL THEN
        RAISE EXCEPTION 'purge completion function is absent';
    END IF;
    IF cleanup_completion IS NULL THEN
        RAISE EXCEPTION 'rendition cleanup completion function is absent';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_runtime' AND NOT rolcanlogin AND rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_worker_runtime' AND NOT rolcanlogin AND rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_purge_function_owner' AND NOT rolcanlogin
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_check_runtime' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_repair_runtime' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_check_function_owner' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_repair_function_owner' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) THEN
        RAISE EXCEPTION 'unsafe stable role attributes';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_migrator' AND rolcanlogin AND rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_api' AND rolcanlogin AND rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_worker' AND rolcanlogin AND rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) THEN
        RAISE EXCEPTION 'unsafe runtime login role attributes';
    END IF;
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
        RAISE EXCEPTION 'managed PostgreSQL role graph differs from exact allowlist';
    END IF;

    SELECT pg_catalog.pg_get_userbyid(c.relowner)
    INTO table_owner
    FROM pg_catalog.pg_class AS c
    WHERE c.oid='public.purge_file_progress'::regclass;
    SELECT pg_catalog.pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig
    INTO function_owner,security_definer,function_config
    FROM pg_catalog.pg_proc AS p WHERE p.oid=completion;
    IF table_owner <> 'nmcp_migrator'
       OR function_owner <> 'nmcp_purge_function_owner'
       OR table_owner = function_owner
       OR NOT security_definer
       OR NOT COALESCE(function_config @> ARRAY['search_path=public, pg_catalog, pg_temp'],false) THEN
        RAISE EXCEPTION 'unsafe purge function ownership or configuration';
    END IF;

    SELECT pg_catalog.pg_get_userbyid(c.relowner)
    INTO cleanup_table_owner
    FROM pg_catalog.pg_class AS c
    WHERE c.oid='public.rendition_cleanup_progress'::regclass;
    SELECT pg_catalog.pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig,
           EXISTS (
               SELECT 1
               FROM pg_catalog.aclexplode(
                   COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))
               ) AS acl
               WHERE acl.grantee=0 AND acl.privilege_type='EXECUTE'
           )
    INTO cleanup_function_owner,cleanup_security_definer,
         cleanup_function_config,cleanup_public_execute
    FROM pg_catalog.pg_proc AS p WHERE p.oid=cleanup_completion;
    IF cleanup_table_owner <> 'nmcp_migrator'
       OR cleanup_function_owner <> 'nmcp_purge_function_owner'
       OR cleanup_table_owner = cleanup_function_owner
       OR NOT cleanup_security_definer
       OR NOT COALESCE(
           cleanup_function_config @> ARRAY['search_path=public, pg_catalog, pg_temp'],
           false
       )
       OR cleanup_public_execute THEN
        RAISE EXCEPTION 'unsafe rendition cleanup function ownership or configuration';
    END IF;

    IF NOT pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.purge_file_progress','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.purge_file_progress','INSERT') THEN
        RAISE EXCEPTION 'unsafe runtime purge progress table ACL';
    END IF;
    IF NOT pg_catalog.has_table_privilege('nmcp_api','public.schema_migrations','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker','public.schema_migrations','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.schema_migrations','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.schema_migrations','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.schema_migrations','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.schema_migrations','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.schema_migrations','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.schema_migrations','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.schema_migrations','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.schema_migrations','TRUNCATE') THEN
        RAISE EXCEPTION 'runtime migration history ACL is not read-only';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_worker',completion,'EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_api',completion,'EXECUTE') THEN
        RAISE EXCEPTION 'unsafe purge completion EXECUTE ACL';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_worker',cleanup_completion,'EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_api',cleanup_completion,'EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_runtime',cleanup_completion,'EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_worker_runtime',cleanup_completion,'EXECUTE') THEN
        RAISE EXCEPTION 'unsafe rendition cleanup completion EXECUTE ACL';
    END IF;

    IF pg_catalog.has_table_privilege('nmcp_runtime','public.rendition_cleanup_progress','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_runtime','public.rendition_cleanup_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_runtime','public.rendition_cleanup_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_runtime','public.rendition_cleanup_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_runtime','public.rendition_cleanup_progress','TRUNCATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker_runtime','public.rendition_cleanup_progress','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker_runtime','public.rendition_cleanup_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.rendition_cleanup_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.rendition_cleanup_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.rendition_cleanup_progress','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.rendition_cleanup_progress','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.rendition_cleanup_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.rendition_cleanup_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.rendition_cleanup_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.rendition_cleanup_progress','TRUNCATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker','public.rendition_cleanup_progress','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker','public.rendition_cleanup_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.rendition_cleanup_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.rendition_cleanup_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.rendition_cleanup_progress','TRUNCATE') THEN
        RAISE EXCEPTION 'unsafe runtime rendition cleanup progress table ACL';
    END IF;
    IF pg_catalog.has_table_privilege('nmcp_runtime','public.renditions','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.renditions','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.renditions','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.renditions','DELETE') THEN
        RAISE EXCEPTION 'runtime can directly delete renditions';
    END IF;
    IF pg_catalog.has_schema_privilege('nmcp_api','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_worker','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_purge_function_owner','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_check_runtime','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_repair_runtime','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_check_function_owner','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_repair_function_owner','public','CREATE') THEN
        RAISE EXCEPTION 'runtime or function owner can create schema objects';
    END IF;
    IF pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','UPDATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','SELECT')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.purge_file_progress','disposition','UPDATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.job_targets','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.profiles','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.renditions','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.renditions','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.maintenance_state','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.media','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.jobs','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.renditions','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','disposition','UPDATE') THEN
        RAISE EXCEPTION 'function owner row-lock/update privileges are not column-scoped';
    END IF;
    IF pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.job_targets','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.job_targets','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.job_targets','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.job_targets','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.profiles','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.profiles','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.profiles','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.profiles','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.renditions','INSERT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.renditions','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.renditions','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.rendition_cleanup_progress','TRUNCATE') THEN
        RAISE EXCEPTION 'function owner has broad table mutation privileges';
    END IF;
END;
$$;

-- Treat every reconciliation/upload/repair SECURITY DEFINER entry point as one
-- catalog allowlist.  Checking only representative functions lets ownership,
-- invoker mode, search_path, or EXECUTE drift hide elsewhere in the boundary.
DO $$
DECLARE
    boundary record;
    boundary_oid oid;
    boundary_owner name;
    boundary_security_definer boolean;
    boundary_config text[];
    unintended_role text;
    target_search_path text[] := ARRAY['search_path=public, pg_catalog, pg_temp'];
BEGIN
    FOR boundary IN
        SELECT * FROM (VALUES
            ('nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)',
             'nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']::text[]),
            ('nmcp_complete_check_source(nmcp_uuid_v4,text,text)',
             'nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']::text[]),
            ('nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)',
             'nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']::text[]),
            ('nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)',
             'nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']::text[]),
            ('nmcp_require_normal_maintenance()',
             'nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']::text[]),
            ('nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']::text[]),
            ('nmcp_heartbeat_upload_attempt(nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']::text[]),
            ('nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']::text[]),
            ('nmcp_complete_upload_attempt(nmcp_uuid_v4,text)',
             'nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']::text[]),
            ('nmcp_record_transform_storage_attempt()',
             'nmcp_repair_function_owner',NULL,ARRAY[]::text[]),
            ('nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)',
             'nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']::text[]),
            ('nmcp_prepare_repair_manifest_item(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']::text[]),
            ('nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']::text[]),
            ('nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text)',
             'nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']::text[]),
            ('nmcp_finish_repair_run(nmcp_uuid_v4)',
             'nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']::text[]),
            ('nmcp_require_terminal_repair_attempt()',
             'nmcp_repair_function_owner',NULL,ARRAY[]::text[])
        ) AS expected(signature,owner_name,acl_grantee,effective_grantees)
    LOOP
        boundary_oid := pg_catalog.to_regprocedure('public.'||boundary.signature);
        IF boundary_oid IS NULL THEN
            RAISE EXCEPTION 'reconciliation boundary function is absent: %',boundary.signature;
        END IF;
        SELECT pg_catalog.pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig
        INTO boundary_owner,boundary_security_definer,boundary_config
        FROM pg_catalog.pg_proc AS p WHERE p.oid=boundary_oid;
        IF boundary_owner<>boundary.owner_name
           OR NOT boundary_security_definer
           OR boundary_config IS DISTINCT FROM target_search_path THEN
            RAISE EXCEPTION 'unsafe owner, SECURITY DEFINER, or search_path on %',boundary.signature;
        END IF;

        IF boundary.acl_grantee IS NOT NULL AND NOT EXISTS (
            SELECT 1
            FROM pg_catalog.pg_proc AS p
            CROSS JOIN LATERAL pg_catalog.aclexplode(
                COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))
            ) AS acl
            WHERE p.oid=boundary_oid
              AND acl.grantee=boundary.acl_grantee::pg_catalog.regrole::oid
              AND acl.privilege_type='EXECUTE'
        ) THEN
            RAISE EXCEPTION 'missing intended EXECUTE ACL on %',boundary.signature;
        END IF;
        SELECT CASE WHEN acl.grantee=0 THEN 'PUBLIC'
                    ELSE pg_catalog.pg_get_userbyid(acl.grantee)::text END
        INTO unintended_role
        FROM pg_catalog.pg_proc AS p
        CROSS JOIN LATERAL pg_catalog.aclexplode(
            COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))
        ) AS acl
        WHERE p.oid=boundary_oid
          AND acl.privilege_type='EXECUTE'
          AND acl.grantee<>p.proowner
          AND (boundary.acl_grantee IS NULL
               OR acl.grantee<>boundary.acl_grantee::pg_catalog.regrole::oid)
        LIMIT 1;
        IF unintended_role IS NOT NULL THEN
            RAISE EXCEPTION 'unintended EXECUTE ACL for % on %',unintended_role,boundary.signature;
        END IF;

        SELECT candidate.role_name INTO unintended_role
        FROM unnest(ARRAY[
            'nmcp_runtime','nmcp_worker_runtime','nmcp_check_runtime','nmcp_repair_runtime',
            'nmcp_api','nmcp_worker'
        ]) AS candidate(role_name)
        WHERE NOT candidate.role_name=ANY(boundary.effective_grantees)
          AND pg_catalog.has_function_privilege(candidate.role_name,boundary_oid,'EXECUTE')
        LIMIT 1;
        IF unintended_role IS NOT NULL THEN
            RAISE EXCEPTION 'unintended effective EXECUTE for % on %',unintended_role,boundary.signature;
        END IF;
        IF EXISTS (
            SELECT 1 FROM unnest(boundary.effective_grantees) AS intended(role_name)
            WHERE NOT pg_catalog.has_function_privilege(intended.role_name,boundary_oid,'EXECUTE')
        ) THEN
            RAISE EXCEPTION 'missing intended effective EXECUTE on %',boundary.signature;
        END IF;
    END LOOP;
END;
$$;

DO $$
DECLARE
    evidence_table text;
    target_search_path text := 'search_path=public, pg_catalog, pg_temp';
BEGIN
    FOREACH evidence_table IN ARRAY ARRAY[
        'reconciliation_reports','reconciliation_check_reports','reconciliation_check_findings','reconciliation_check_source_results',
        'reconciliation_check_report_seals','reconciliation_repair_runs',
        'reconciliation_repair_manifest_items','reconciliation_repair_attempts',
        'reconciliation_repair_events','reconciliation_repair_results','storage_attempts','storage_attempt_events'
    ] LOOP
        IF pg_catalog.has_table_privilege('nmcp_runtime','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
           OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
           OR pg_catalog.has_table_privilege('nmcp_check_runtime','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
           OR pg_catalog.has_table_privilege('nmcp_repair_runtime','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
           OR pg_catalog.has_table_privilege('nmcp_api','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
           OR pg_catalog.has_table_privilege('nmcp_worker','public.'||evidence_table,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE') THEN
            RAISE EXCEPTION 'runtime has direct reconciliation evidence privilege on %',evidence_table;
        END IF;
    END LOOP;
    IF pg_catalog.has_table_privilege('nmcp_runtime','public.maintenance_state','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_worker_runtime','public.maintenance_state','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.maintenance_state','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.maintenance_state','SELECT,INSERT,UPDATE,DELETE,TRUNCATE') THEN
        RAISE EXCEPTION 'runtime has direct maintenance privilege';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_complete_check_source(nmcp_uuid_v4,text,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_complete_check_source(nmcp_uuid_v4,text,text)','EXECUTE')
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_proc p CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
           WHERE p.oid='public.nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)'::regprocedure
             AND a.grantee=0 AND a.privilege_type='EXECUTE') THEN
        RAISE EXCEPTION 'unsafe check publication EXECUTE ACL';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_runtime','public.nmcp_require_normal_maintenance()','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_runtime','public.nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_runtime','public.nmcp_heartbeat_upload_attempt(nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_runtime','public.nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_runtime','public.nmcp_complete_upload_attempt(nmcp_uuid_v4,text)','EXECUTE')
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_proc p CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
           WHERE p.oid='public.nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)'::regprocedure
             AND a.grantee=0 AND a.privilege_type='EXECUTE') THEN
        RAISE EXCEPTION 'unsafe upload attempt boundary EXECUTE ACL';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_proc
        WHERE oid IN (
            'public.nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)'::regprocedure,
            'public.nmcp_complete_check_source(nmcp_uuid_v4,text,text)'::regprocedure,
            'public.nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)'::regprocedure,
            'public.nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)'::regprocedure
        ) AND (pg_catalog.pg_get_userbyid(proowner)<>'nmcp_check_function_owner' OR proconfig IS DISTINCT FROM ARRAY[target_search_path])
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_proc
        WHERE oid IN (
            'public.nmcp_require_normal_maintenance()'::regprocedure,
            'public.nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)'::regprocedure,
            'public.nmcp_heartbeat_upload_attempt(nmcp_uuid_v4)'::regprocedure,
            'public.nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)'::regprocedure,
            'public.nmcp_complete_upload_attempt(nmcp_uuid_v4,text)'::regprocedure,
            'public.nmcp_record_transform_storage_attempt()'::regprocedure
        ) AND (pg_catalog.pg_get_userbyid(proowner)<>'nmcp_repair_function_owner' OR proconfig IS DISTINCT FROM ARRAY[target_search_path])
    ) THEN
        RAISE EXCEPTION 'unsafe reconciliation function owner or search_path';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_prepare_repair_manifest_item(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_finish_repair_run(nmcp_uuid_v4)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','EXECUTE') THEN
        RAISE EXCEPTION 'unsafe repair publication EXECUTE ACL';
    END IF;
    IF NOT pg_catalog.has_table_privilege('nmcp_check_function_owner','public.reconciliation_check_source_results','SELECT,INSERT')
       OR pg_catalog.has_table_privilege('nmcp_check_function_owner','public.reconciliation_check_source_results','UPDATE,DELETE,TRUNCATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_repair_function_owner','public.reconciliation_check_source_results','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_repair_function_owner','public.reconciliation_check_source_results','INSERT,UPDATE,DELETE,TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_repair_function_owner','public.reconciliation_check_report_seals','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_repair_function_owner','public.reconciliation_check_report_seals','report_id','UPDATE') THEN
        RAISE EXCEPTION 'unsafe reconciliation source/seal owner ACL';
    END IF;
END;
$$;
