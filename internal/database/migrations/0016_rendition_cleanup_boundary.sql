-- Historical Rendition cleanup is a single transaction ordered as
-- maintenance -> Media -> started purge Jobs -> Rendition -> progress.

DO $$
DECLARE required_role text;
BEGIN
    FOREACH required_role IN ARRAY ARRAY['nmcp_runtime','nmcp_worker_runtime','nmcp_purge_function_owner'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=required_role) THEN
            RAISE EXCEPTION 'required database role is absent: %',required_role USING ERRCODE='55000';
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname IN ('nmcp_runtime','nmcp_worker_runtime','nmcp_purge_function_owner') AND rolcanlogin)
       OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_purge_function_owner','MEMBER') THEN
        RAISE EXCEPTION 'NMCP database roles do not match cleanup boundary' USING ERRCODE='55000';
    END IF;
END;
$$;

-- The legacy direct insert path acquired this table before its trigger took
-- Media/Rendition locks. Drain it first during the one-time cutover.
LOCK TABLE rendition_cleanup_progress IN ACCESS EXCLUSIVE MODE;
LOCK TABLE maintenance_state IN ACCESS EXCLUSIVE MODE;
LOCK TABLE media IN ACCESS EXCLUSIVE MODE;
LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE renditions IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM rendition_cleanup_progress WHERE disposition='pending') THEN
        RAISE EXCEPTION 'cannot install cleanup boundary with pending progress';
    END IF;
    IF EXISTS (SELECT 1 FROM rendition_cleanup_progress p JOIN renditions r ON r.id=p.rendition_id) THEN
        RAISE EXCEPTION 'completed cleanup progress still has a Rendition';
    END IF;
END;
$$;

CREATE FUNCTION nmcp_require_completed_rendition_cleanup_progress()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM rendition_cleanup_progress WHERE id=NEW.id AND disposition='pending') THEN
        RAISE EXCEPTION 'pending Rendition cleanup progress cannot commit: %',NEW.id USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER rendition_cleanup_progress_completed
AFTER INSERT OR UPDATE ON rendition_cleanup_progress
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_require_completed_rendition_cleanup_progress();

CREATE FUNCTION nmcp_complete_rendition_cleanup(
    cleanup_progress_id nmcp_uuid_v4,
    cleanup_media_id nmcp_uuid_v4,
    cleanup_rendition_id nmcp_uuid_v4,
    cleanup_disposition text
) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE
    progress_row rendition_cleanup_progress%ROWTYPE;
    rendition_row renditions%ROWTYPE;
    candidate_version integer;
    current_version integer;
    affected bigint;
BEGIN
    IF cleanup_disposition NOT IN ('deleted','missing') THEN
        RAISE EXCEPTION 'invalid Rendition cleanup disposition' USING ERRCODE='23514';
    END IF;
    PERFORM 1 FROM maintenance_state WHERE id=1 AND mode='normal' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'maintenance mode is active' USING ERRCODE='55000'; END IF;
    PERFORM 1 FROM media WHERE id=cleanup_media_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'cleanup requires existing Media' USING ERRCODE='23514'; END IF;
    PERFORM 1 FROM jobs
      WHERE type='purge' AND media_id_snapshot=cleanup_media_id AND started_at IS NOT NULL
      ORDER BY id FOR UPDATE;
    IF FOUND THEN RAISE EXCEPTION 'started purge owns Media files' USING ERRCODE='23514'; END IF;
    SELECT * INTO rendition_row FROM renditions
      WHERE id=cleanup_rendition_id AND media_id=cleanup_media_id FOR UPDATE;
    IF NOT FOUND OR rendition_row.is_current OR rendition_row.purge_after IS NULL
       OR rendition_row.purge_after>clock_timestamp() THEN
        RAISE EXCEPTION 'cleanup requires due non-current Rendition' USING ERRCODE='23514';
    END IF;
    SELECT candidate_profile.version,current_profile.version
      INTO candidate_version,current_version
      FROM renditions candidate
      JOIN job_targets candidate_target ON candidate_target.id=candidate.job_target_id AND candidate_target.status='succeeded'
      JOIN jobs candidate_job ON candidate_job.id=candidate_target.job_id AND candidate_job.type='transform' AND candidate_job.media_id_snapshot=candidate.media_id
      JOIN profiles candidate_profile ON candidate_profile.id=candidate_target.profile_id AND candidate_profile.key=candidate.profile_key
      JOIN renditions current_rendition ON current_rendition.media_id=candidate.media_id AND current_rendition.profile_key=candidate.profile_key AND current_rendition.is_current
      JOIN job_targets current_target ON current_target.id=current_rendition.job_target_id AND current_target.status='succeeded'
      JOIN jobs current_job ON current_job.id=current_target.job_id AND current_job.type='transform' AND current_job.media_id_snapshot=candidate.media_id
      JOIN profiles current_profile ON current_profile.id=current_target.profile_id AND current_profile.key=current_rendition.profile_key
      WHERE candidate.id=cleanup_rendition_id;
    IF NOT FOUND OR current_version<candidate_version THEN
        RAISE EXCEPTION 'cleanup requires same-key equal-or-newer current Rendition' USING ERRCODE='23514';
    END IF;
    SELECT * INTO progress_row FROM rendition_cleanup_progress
      WHERE id=cleanup_progress_id AND media_id_snapshot=cleanup_media_id
        AND rendition_id=cleanup_rendition_id FOR UPDATE;
    IF NOT FOUND OR progress_row.disposition<>'pending'
       OR progress_row.job_target_id<>rendition_row.job_target_id
       OR progress_row.relative_path<>rendition_row.relative_path
       OR progress_row.size_bytes<>rendition_row.size_bytes
       OR progress_row.purge_after<>rendition_row.purge_after THEN
        RAISE EXCEPTION 'cleanup progress does not match Rendition' USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.set_config('nmcp.rendition_cleanup_progress_id',cleanup_progress_id::text,true);
    UPDATE rendition_cleanup_progress SET disposition=cleanup_disposition
      WHERE id=cleanup_progress_id AND disposition='pending';
    GET DIAGNOSTICS affected=ROW_COUNT;
    IF affected<>1 THEN RAISE EXCEPTION 'cleanup progress changed' USING ERRCODE='23514'; END IF;
    DELETE FROM renditions WHERE id=cleanup_rendition_id AND media_id=cleanup_media_id;
    GET DIAGNOSTICS affected=ROW_COUNT;
    IF affected<>1 THEN RAISE EXCEPTION 'cleanup Rendition changed' USING ERRCODE='23514'; END IF;
END;
$$;

CREATE FUNCTION nmcp_require_ordered_rendition_cleanup_boundary()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE boundary_owner name;
BEGIN
    SELECT pg_catalog.pg_get_userbyid(proowner) INTO STRICT boundary_owner
      FROM pg_catalog.pg_proc
      WHERE oid='nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)'::regprocedure;
    IF current_user<>boundary_owner THEN
        RAISE EXCEPTION 'Rendition cleanup completion requires ordered boundary' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER rendition_cleanup_progress_00_ordered_boundary
BEFORE UPDATE ON rendition_cleanup_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_require_ordered_rendition_cleanup_boundary();

DO $$
DECLARE target_schema text:=current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.nmcp_require_completed_rendition_cleanup_progress() SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_require_ordered_rendition_cleanup_boundary() SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text) OWNER TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text) SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_purge_function_owner',target_schema);
END;
$$;

REVOKE SELECT,INSERT,UPDATE,DELETE,TRUNCATE ON rendition_cleanup_progress FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
GRANT SELECT,INSERT ON rendition_cleanup_progress TO nmcp_worker_runtime;
REVOKE DELETE ON renditions FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;

GRANT SELECT ON maintenance_state,media,jobs,job_targets,profiles,renditions,rendition_cleanup_progress TO nmcp_purge_function_owner;
GRANT UPDATE(id) ON maintenance_state,media,jobs,renditions TO nmcp_purge_function_owner;
GRANT UPDATE(disposition) ON rendition_cleanup_progress TO nmcp_purge_function_owner;
GRANT DELETE ON renditions TO nmcp_purge_function_owner;

REVOKE ALL ON FUNCTION nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text) FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
GRANT EXECUTE ON FUNCTION nmcp_complete_rendition_cleanup(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text) TO nmcp_worker_runtime;
REVOKE ALL ON FUNCTION nmcp_require_completed_rendition_cleanup_progress(),nmcp_require_ordered_rendition_cleanup_boundary() FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
