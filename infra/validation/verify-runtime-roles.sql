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
-- closed catalog allowlist. Checking only representative or known functions
-- lets an unexpected owner-controlled definer escape review.
CREATE TEMPORARY TABLE expected_reconciliation_definers (
    signature text PRIMARY KEY,
    owner_name text NOT NULL,
    acl_grantee text,
    effective_grantees text[] NOT NULL
);
INSERT INTO expected_reconciliation_definers VALUES
    ('nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_complete_check_source(nmcp_uuid_v4,text,text)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_append_check_finding_once(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)','nmcp_check_function_owner',NULL,ARRAY[]::text[]),
    ('nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_read_check_database_references(timestamptz)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_read_check_database_references_v18(timestamptz)','nmcp_check_function_owner',NULL,ARRAY[]::text[]),
    ('nmcp_read_check_attempt_owners()','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_read_check_report_outcome(nmcp_uuid_v4)','nmcp_check_function_owner','nmcp_check_runtime',ARRAY['nmcp_check_runtime']),
    ('nmcp_require_normal_maintenance()','nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']),
    ('nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']),
    ('nmcp_heartbeat_upload_attempt(nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']),
    ('nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']),
    ('nmcp_complete_upload_attempt(nmcp_uuid_v4,text)','nmcp_repair_function_owner','nmcp_runtime',ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_api','nmcp_worker']),
    ('nmcp_record_transform_storage_attempt()','nmcp_repair_function_owner',NULL,ARRAY[]::text[]),
    ('nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','nmcp_repair_function_owner',NULL,ARRAY[]::text[]),
    ('nmcp_prepare_repair_manifest_item(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner',NULL,ARRAY[]::text[]),
    ('nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_read_repair_plan_digest(nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_read_repair_run_outcome(nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_read_repair_run_by_authorization(nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_finish_repair_run(nmcp_uuid_v4)','nmcp_repair_function_owner','nmcp_repair_runtime',ARRAY['nmcp_repair_runtime']),
    ('nmcp_require_terminal_repair_attempt()','nmcp_repair_function_owner',NULL,ARRAY[]::text[]);
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
    FOR boundary IN SELECT * FROM expected_reconciliation_definers
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
              AND NOT acl.is_grantable
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
    IF EXISTS (
        WITH actual AS (
            SELECT p.oid,
                   pg_catalog.pg_get_userbyid(p.proowner)::text AS owner_name
            FROM pg_catalog.pg_proc AS p
            JOIN pg_catalog.pg_namespace AS n ON n.oid=p.pronamespace
            WHERE n.nspname='public' AND p.prosecdef
              AND p.proowner IN ('nmcp_check_function_owner'::pg_catalog.regrole,
                                 'nmcp_repair_function_owner'::pg_catalog.regrole)
        ), expected AS (
            SELECT pg_catalog.to_regprocedure('public.'||signature)::oid AS oid,owner_name
            FROM expected_reconciliation_definers
        )
        (SELECT * FROM expected EXCEPT SELECT * FROM actual)
        UNION ALL
        (SELECT * FROM actual EXCEPT SELECT * FROM expected)
    ) THEN
        RAISE EXCEPTION 'reconciliation SECURITY DEFINER set differs from exact allowlist';
    END IF;
END;
$$;
DROP TABLE expected_reconciliation_definers;

-- Fixed-shape readers and replay wrappers are an application/database ABI.
-- Compare the catalog-rendered result exactly so reordered or widened rows do
-- not pass merely because the callable signature and ACL remained unchanged.
DO $$
DECLARE
    expected record;
    function_oid oid;
BEGIN
    FOR expected IN SELECT * FROM (VALUES
        ('nmcp_read_check_database_references(timestamptz)',
         'TABLE(row_kind text, subject_id uuid, media_id uuid, original_id uuid, job_id uuid, job_target_id uuid, relative_key text, expected_state text, size_bytes bigint, sha256 text, is_current boolean, provenance_valid boolean, current_valid boolean, due_at timestamp with time zone)'),
        ('nmcp_read_check_database_references_v18(timestamptz)',
         'TABLE(row_kind text, subject_id uuid, media_id uuid, original_id uuid, job_id uuid, job_target_id uuid, relative_key text, expected_state text, size_bytes bigint, sha256 text, is_current boolean, provenance_valid boolean, current_valid boolean, due_at timestamp with time zone)'),
        ('nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)',
         'TABLE(ordinal bigint, finding_id uuid, source_type text, subject_id uuid, media_id uuid, job_id uuid, job_target_id uuid, storage_attempt_id uuid, source_relative_key text, expected_size_bytes bigint, report_observed_sha256 text, observed_mtime timestamp with time zone, observed_ctime timestamp with time zone)'),
        ('nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)',
         'TABLE(run_id uuid, source_report_id uuid, authorization_id uuid, authorized_actor text, created boolean)'),
        ('nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
         'TABLE(item_id uuid, report_ordinal bigint, destination_relative_key text, created boolean)'),
        ('nmcp_read_repair_plan_digest(nmcp_uuid_v4)','text'),
        ('nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text)',
         'TABLE(plan_id uuid, item_count bigint, plan_digest text, created boolean)'),
        ('nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
         'TABLE(decision text, reason_code text, source_type text, subject_id uuid, media_id uuid, job_id uuid, job_target_id uuid, storage_attempt_id uuid, source_relative_key text, expected_size_bytes bigint, expected_sha256 text, destination_relative_key text, owner_attempt_id uuid, owner_kind text, owner_coverage text, owner_event_type text, owner_lease_expires_at timestamp with time zone, owner_event_occurred_at timestamp with time zone)'),
        ('nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
         'TABLE(decision text, reason_code text, source_type text, subject_id uuid, media_id uuid, job_id uuid, job_target_id uuid, storage_attempt_id uuid, source_relative_key text, expected_size_bytes bigint, expected_sha256 text, destination_relative_key text, owner_attempt_id uuid, owner_kind text, owner_coverage text, owner_event_type text, owner_lease_expires_at timestamp with time zone, owner_event_occurred_at timestamp with time zone)'),
        ('nmcp_read_repair_run_outcome(nmcp_uuid_v4)',
         'TABLE(run_id uuid, source_report_id uuid, authorization_id uuid, authorized_actor text, started_at timestamp with time zone, plan_id uuid, plan_version smallint, plan_item_count bigint, plan_digest text, result text, quarantined_count bigint, stale_count bigint, resolved_count bigint, failed_count bigint, unknown_count bigint, completed_at timestamp with time zone)'),
        ('nmcp_read_repair_run_by_authorization(nmcp_uuid_v4)',
         'TABLE(run_id uuid, source_report_id uuid, authorization_id uuid, authorized_actor text, started_at timestamp with time zone, plan_id uuid, plan_version smallint, plan_item_count bigint, plan_digest text, result text, quarantined_count bigint, stale_count bigint, resolved_count bigint, failed_count bigint, unknown_count bigint, completed_at timestamp with time zone)'),
        ('nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer)',
         'TABLE(ordinal bigint, item_id uuid, finding_id uuid, quarantine_id uuid, source_type text, subject_id uuid, media_id uuid, job_id uuid, job_target_id uuid, storage_attempt_id uuid, source_relative_key text, expected_size_bytes bigint, expected_sha256 text, observed_mtime timestamp with time zone, observed_ctime timestamp with time zone, owner_snapshot text, destination_relative_key text, effective_outcome text, active_attempt_id uuid, latest_attempt_number integer)'),
        ('nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer)',
         'TABLE(attempt_number integer, attempt_id uuid, started_at timestamp with time zone, sequence integer, event_id uuid, event_type text, outcome_code text, error_code text, observed_size_bytes bigint, observed_sha256 text, transaction_id xid8, occurred_at timestamp with time zone, physical_effect_possible boolean)')
    ) AS shapes(signature,result_shape)
    LOOP
        function_oid := pg_catalog.to_regprocedure('public.'||expected.signature);
        IF function_oid IS NULL OR pg_catalog.pg_get_function_result(function_oid)<>expected.result_shape THEN
            RAISE EXCEPTION 'reconciliation fixed result shape differs on %',expected.signature;
        END IF;
    END LOOP;
END;
$$;

-- Check the managed role graph after function boundaries so an inherited
-- privilege counterexample is rejected by the effective-ACL assertion itself.
DO $$
BEGIN
    IF EXISTS (
        WITH expected(table_name,owner_name,relation_kind) AS (VALUES
            ('change_events','nmcp_migrator','r'::"char"),
            ('reconciliation_repair_plan_seals','nmcp_migrator','r'::"char"),
            ('reconciliation_repair_manifest_items','nmcp_migrator','r'::"char"),
            ('reconciliation_repair_attempts','nmcp_migrator','r'::"char"),
            ('reconciliation_repair_events','nmcp_migrator','r'::"char")
        ), actual AS (
            SELECT c.relname::text,pg_catalog.pg_get_userbyid(c.relowner)::text,c.relkind
            FROM pg_catalog.pg_class AS c
            JOIN pg_catalog.pg_namespace AS n ON n.oid=c.relnamespace
            WHERE n.nspname='public' AND c.relname IN (
                'change_events','reconciliation_repair_plan_seals','reconciliation_repair_manifest_items',
                'reconciliation_repair_attempts','reconciliation_repair_events'
            )
        )
        (SELECT * FROM expected EXCEPT SELECT * FROM actual)
        UNION ALL
        (SELECT * FROM actual EXCEPT SELECT * FROM expected)
    ) THEN
        RAISE EXCEPTION 'checker source relation owner or kind differs from exact allowlist';
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
END;
$$;

-- Migration 0018 deliberately gives the checker owner only the columns read by
-- its SECURITY DEFINER sources. Compare the underlying ACL catalogs so table
-- grants, write privileges, and grant options cannot hide
-- behind an effective-privilege spot check.
DO $$
DECLARE
    source_table text;
    source_privilege text;
BEGIN
    IF EXISTS (
        WITH expected_base(table_name,column_name,grantee,privilege_type,is_grantable) AS (VALUES
            ('change_events','position','nmcp_check_function_owner','SELECT',false),
            ('change_events','event_type','nmcp_check_function_owner','SELECT',false),
            ('change_events','reason','nmcp_check_function_owner','SELECT',false),
            ('change_events','media_id','nmcp_check_function_owner','SELECT',false),
            ('change_events','payload','nmcp_check_function_owner','SELECT',false),
            ('change_events','occurred_at','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','quarantine_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','media_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','job_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','job_target_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','destination_relative_key','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','expected_size_bytes','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','report_observed_sha256','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','prepared_at','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','report_ordinal','nmcp_repair_function_owner','SELECT',false),
            ('reconciliation_repair_attempts','id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_attempts','manifest_item_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_attempts','attempt_number','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','attempt_id','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','sequence','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','event_type','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','outcome_code','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','error_code','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','observed_size_bytes','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','observed_sha256','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_events','occurred_at','nmcp_check_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','id','nmcp_repair_function_owner','UPDATE',false),
            ('reconciliation_repair_attempts','id','nmcp_repair_function_owner','UPDATE',false)
        ), expected AS (
            SELECT table_name,column_name,'nmcp_migrator'::text AS grantor,
                   grantee,privilege_type,is_grantable
            FROM expected_base
        ), actual AS (
            SELECT c.relname::text,a.attname::text,
                    pg_catalog.pg_get_userbyid(acl.grantor)::text,
                    CASE WHEN acl.grantee=0 THEN 'PUBLIC'
                         ELSE pg_catalog.pg_get_userbyid(acl.grantee)::text END,
                   acl.privilege_type,acl.is_grantable
            FROM pg_catalog.pg_class AS c
            JOIN pg_catalog.pg_namespace AS n ON n.oid=c.relnamespace
            JOIN pg_catalog.pg_attribute AS a ON a.attrelid=c.oid
            CROSS JOIN LATERAL pg_catalog.aclexplode(a.attacl) AS acl
            WHERE n.nspname='public' AND c.relname IN (
                'change_events','reconciliation_repair_plan_seals','reconciliation_repair_manifest_items',
                'reconciliation_repair_attempts','reconciliation_repair_events'
            ) AND a.attnum>0 AND NOT a.attisdropped
        )
        (SELECT * FROM expected EXCEPT SELECT * FROM actual)
        UNION ALL
        (SELECT * FROM actual EXCEPT SELECT * FROM expected)
    ) THEN
        RAISE EXCEPTION 'checker source column ACL catalog differs from exact allowlist';
    END IF;

    IF EXISTS (
        WITH expected(table_name,grantee,privilege_type,is_grantable) AS (VALUES
            ('change_events','nmcp_purge_function_owner','SELECT',false),
            ('change_events','nmcp_runtime','SELECT',false),
            ('change_events','nmcp_runtime','INSERT',false),
            ('change_events','nmcp_runtime','UPDATE',false),
            ('change_events','nmcp_runtime','DELETE',false),
            ('reconciliation_repair_manifest_items','nmcp_repair_function_owner','SELECT',false),
            ('reconciliation_repair_manifest_items','nmcp_repair_function_owner','INSERT',false),
            ('reconciliation_repair_attempts','nmcp_repair_function_owner','SELECT',false),
            ('reconciliation_repair_attempts','nmcp_repair_function_owner','INSERT',false),
            ('reconciliation_repair_events','nmcp_repair_function_owner','SELECT',false),
            ('reconciliation_repair_events','nmcp_repair_function_owner','INSERT',false),
            ('reconciliation_repair_plan_seals','nmcp_repair_function_owner','SELECT',false),
            ('reconciliation_repair_plan_seals','nmcp_repair_function_owner','INSERT',false)
        ), actual AS (
            SELECT c.relname::text,pg_catalog.pg_get_userbyid(acl.grantee)::text,
                   acl.privilege_type,acl.is_grantable
            FROM pg_catalog.pg_class AS c
            JOIN pg_catalog.pg_namespace AS n ON n.oid=c.relnamespace
            CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) AS acl
            WHERE n.nspname='public' AND c.relname IN (
                'change_events','reconciliation_repair_plan_seals','reconciliation_repair_manifest_items',
                'reconciliation_repair_attempts','reconciliation_repair_events'
            ) AND acl.grantee<>c.relowner
        )
        (SELECT * FROM expected EXCEPT SELECT * FROM actual)
        UNION ALL
        (SELECT * FROM actual EXCEPT SELECT * FROM expected)
    ) THEN
        RAISE EXCEPTION 'checker source table ACL catalog differs from exact allowlist';
    END IF;

    FOREACH source_table IN ARRAY ARRAY[
        'change_events','reconciliation_repair_plan_seals','reconciliation_repair_manifest_items',
        'reconciliation_repair_attempts','reconciliation_repair_events'
    ] LOOP
        FOREACH source_privilege IN ARRAY ARRAY[
            'SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER'
        ] LOOP
            IF pg_catalog.has_table_privilege(
                'nmcp_check_function_owner','public.'||source_table,source_privilege
            ) THEN
                RAISE EXCEPTION 'checker owner has effective table % on %',source_privilege,source_table;
            END IF;
        END LOOP;
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
        'reconciliation_repair_plan_seals',
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
    IF NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_read_repair_run_outcome(nmcp_uuid_v4)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer)','EXECUTE')
       OR NOT pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_finish_repair_run(nmcp_uuid_v4)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_prepare_repair_manifest_item(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_check_runtime','public.nmcp_read_repair_run_outcome(nmcp_uuid_v4)','EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_repair_runtime','public.nmcp_read_check_database_references(timestamptz)','EXECUTE') THEN
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
