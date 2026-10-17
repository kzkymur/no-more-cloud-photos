-- Make the ordered purge-progress boundary deployable without granting the
-- Worker a progress-first row lock or allowing the migration owner to bypass it.

DO $$
DECLARE
    required_role text;
BEGIN
    FOREACH required_role IN ARRAY ARRAY[
        'nmcp_runtime',
        'nmcp_worker_runtime',
        'nmcp_purge_function_owner'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=required_role) THEN
            RAISE EXCEPTION 'required database role is absent: %', required_role
                USING ERRCODE = '55000';
        END IF;
    END LOOP;
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname IN ('nmcp_runtime','nmcp_worker_runtime','nmcp_purge_function_owner')
          AND rolcanlogin
    ) THEN
        RAISE EXCEPTION 'NMCP capability and function-owner roles must be NOLOGIN'
            USING ERRCODE = '55000';
    END IF;
    IF pg_catalog.pg_has_role('nmcp_runtime','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_purge_function_owner','MEMBER') THEN
        RAISE EXCEPTION 'NMCP database role memberships do not match the runtime boundary'
            USING ERRCODE = '55000';
    END IF;
END;
$$;

LOCK TABLE purge_file_progress IN ACCESS EXCLUSIVE MODE;

CREATE OR REPLACE FUNCTION nmcp_require_ordered_purge_progress_boundary()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    completion_owner name;
BEGIN
    SELECT pg_catalog.pg_get_userbyid(p.proowner)
    INTO STRICT completion_owner
    FROM pg_catalog.pg_proc AS p
    WHERE p.oid='nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)'::regprocedure;
    IF current_user <> completion_owner THEN
        RAISE EXCEPTION 'purge progress completion requires the ordered boundary'
            USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END;
$$;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_purge_function_owner',target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text) OWNER TO nmcp_purge_function_owner',
        target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text) SET search_path = %I, pg_catalog, pg_temp',
        target_schema,target_schema
    );
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_purge_function_owner',target_schema);
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO nmcp_runtime,nmcp_worker_runtime,nmcp_purge_function_owner',target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_require_ordered_purge_progress_boundary() SET search_path = %I, pg_catalog, pg_temp',
        target_schema,target_schema
    );
END;
$$;

REVOKE ALL ON FUNCTION nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
GRANT EXECUTE ON FUNCTION nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text)
TO nmcp_worker_runtime;

GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE
    system_config,
    media,
    originals,
    profiles,
    jobs,
    job_targets,
    renditions,
    idempotency_requests,
    change_feed_state,
    change_events,
    backup_runs,
    maintenance_state,
    admin_audit,
    admin_batches,
    reconciliation_reports,
    profile_processor_capabilities,
    profile_processor_certifications,
    rendition_cleanup_progress
TO nmcp_runtime;

REVOKE ALL ON TABLE purge_file_progress FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
GRANT SELECT,INSERT ON TABLE purge_file_progress TO nmcp_worker_runtime;

GRANT SELECT ON TABLE maintenance_state,media,jobs,purge_file_progress TO nmcp_purge_function_owner;
GRANT UPDATE(id) ON TABLE maintenance_state,media,jobs TO nmcp_purge_function_owner;
GRANT UPDATE(disposition) ON TABLE purge_file_progress TO nmcp_purge_function_owner;
