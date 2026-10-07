\set ON_ERROR_STOP on

DO $$
DECLARE
    completion regprocedure := pg_catalog.to_regprocedure(
        'public.nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)'
    );
    table_owner name;
    function_owner name;
    security_definer boolean;
    function_config text[];
BEGIN
    IF completion IS NULL THEN
        RAISE EXCEPTION 'purge completion function is absent';
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
    IF NOT pg_catalog.pg_has_role('nmcp_api','nmcp_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_api','nmcp_worker_runtime','MEMBER')
       OR NOT pg_catalog.pg_has_role('nmcp_worker','nmcp_runtime','MEMBER')
       OR NOT pg_catalog.pg_has_role('nmcp_worker','nmcp_worker_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_api','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker','nmcp_purge_function_owner','MEMBER') THEN
        RAISE EXCEPTION 'unsafe runtime role membership';
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

    IF NOT pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','INSERT')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','DELETE')
       OR pg_catalog.has_table_privilege('nmcp_worker','public.purge_file_progress','TRUNCATE')
       OR pg_catalog.has_table_privilege('nmcp_api','public.purge_file_progress','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_api','public.purge_file_progress','INSERT') THEN
        RAISE EXCEPTION 'unsafe runtime purge progress table ACL';
    END IF;
    IF NOT pg_catalog.has_function_privilege('nmcp_worker',completion,'EXECUTE')
       OR pg_catalog.has_function_privilege('nmcp_api',completion,'EXECUTE') THEN
        RAISE EXCEPTION 'unsafe purge completion EXECUTE ACL';
    END IF;
    IF pg_catalog.has_schema_privilege('nmcp_api','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_worker','public','CREATE')
       OR pg_catalog.has_schema_privilege('nmcp_purge_function_owner','public','CREATE') THEN
        RAISE EXCEPTION 'runtime or function owner can create schema objects';
    END IF;
    IF pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','UPDATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','SELECT')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.purge_file_progress','disposition','UPDATE')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','SELECT')
       OR NOT pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','SELECT')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.maintenance_state','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.media','UPDATE')
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.jobs','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.maintenance_state','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.media','id','UPDATE')
       OR NOT pg_catalog.has_column_privilege('nmcp_purge_function_owner','public.jobs','id','UPDATE') THEN
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
       OR pg_catalog.has_table_privilege('nmcp_purge_function_owner','public.purge_file_progress','TRUNCATE') THEN
        RAISE EXCEPTION 'function owner has broad table mutation privileges';
    END IF;
END;
$$;
