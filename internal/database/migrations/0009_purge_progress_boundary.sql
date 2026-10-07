-- Funnel application-role purge progress completion through the global
-- maintenance -> Media -> Job -> progress lock order.

CREATE FUNCTION nmcp_require_ordered_purge_progress_boundary()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF current_user <> pg_catalog.pg_get_userbyid(
        (SELECT c.relowner FROM pg_catalog.pg_class AS c WHERE c.oid = TG_RELID)
    ) THEN
        RAISE EXCEPTION 'purge progress completion requires the ordered boundary'
            USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER purge_file_progress_00_ordered_boundary
BEFORE UPDATE ON purge_file_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_require_ordered_purge_progress_boundary();

REVOKE ALL ON FUNCTION nmcp_require_ordered_purge_progress_boundary() FROM PUBLIC;

CREATE FUNCTION nmcp_complete_purge_file_progress(
    purge_job_id nmcp_uuid_v4,
    purge_media_id nmcp_uuid_v4,
    purge_object_kind text,
    purge_object_id nmcp_uuid_v4,
    purge_token text,
    purge_disposition text
)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
AS $$
DECLARE
    maintenance_mode text;
    affected bigint;
BEGIN
    IF purge_disposition NOT IN ('deleted', 'missing') THEN
        RAISE EXCEPTION 'invalid purge progress disposition'
            USING ERRCODE = '23514';
    END IF;
    SELECT mode INTO STRICT maintenance_mode
    FROM maintenance_state WHERE id=1 FOR SHARE;
    IF maintenance_mode <> 'normal' THEN
        RAISE EXCEPTION 'maintenance mode is active'
            USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM media
    WHERE id=purge_media_id AND deleted_at IS NOT NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'purge progress requires deleted Media: %', purge_media_id
            USING ERRCODE = '23514';
    END IF;
    PERFORM 1 FROM jobs
    WHERE id=purge_job_id
      AND type='purge'
      AND media_id_snapshot=purge_media_id
      AND status='running'
      AND lease_token::text=purge_token
      AND lease_expires_at>clock_timestamp()
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'purge progress completion requires its live lease: %', purge_job_id
            USING ERRCODE = '23514';
    END IF;
    PERFORM pg_catalog.set_config('nmcp.purge_lease_token',purge_token,true);
    UPDATE purge_file_progress
    SET disposition=purge_disposition
    WHERE job_id=purge_job_id
      AND object_kind=purge_object_kind
      AND object_id=purge_object_id
      AND disposition='pending';
    GET DIAGNOSTICS affected = ROW_COUNT;
    IF affected <> 1 THEN
        RAISE EXCEPTION 'pending purge progress row changed: %/%/%', purge_job_id, purge_object_kind, purge_object_id
            USING ERRCODE = '23514';
    END IF;
END;
$$;

REVOKE ALL ON FUNCTION nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text) FROM PUBLIC;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_require_ordered_purge_progress_boundary() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_complete_purge_file_progress(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text) SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;
