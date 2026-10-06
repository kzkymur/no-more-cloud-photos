-- Make transform completion an atomic aggregate state and retain the bounded
-- processor evidence used to produce every new Rendition.

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    PERFORM pg_catalog.set_config(
        'search_path', pg_catalog.quote_ident(target_schema) || ', pg_catalog, pg_temp', true
    );
END;
$$;

-- Take every lock needed by preflight and DDL up front, in parent-before-child
-- order. This closes the gap between validating legacy rows and installing the
-- deferred guards, and makes later writers wait for the completed migration.
LOCK TABLE jobs, job_targets, renditions IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE invalid_job record;
BEGIN
    SELECT job.id, job.status
    INTO invalid_job
    FROM jobs AS job
    WHERE job.type = 'transform'
      AND (job.status = 'succeeded') IS DISTINCT FROM (NOT EXISTS (
          SELECT 1
          FROM job_targets AS target
          WHERE target.job_id = job.id AND target.status <> 'succeeded'
      ))
    ORDER BY job.id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'existing transform job aggregate is incompatible: job %, status %',
            invalid_job.id, invalid_job.status
            USING ERRCODE = '23514';
    END IF;
END;
$$;

CREATE FUNCTION nmcp_check_transform_job_success()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    checked_job_id nmcp_uuid_v4;
    checked_type text;
    checked_status text;
    every_target_succeeded boolean;
BEGIN
    IF TG_TABLE_NAME = 'jobs' THEN
        checked_job_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
    ELSE
        checked_job_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.job_id ELSE NEW.job_id END;
    END IF;

    -- The parent lock serializes commits that complete different targets of the
    -- same job, preventing a concurrent last-target write skew.
    SELECT type, status
    INTO checked_type, checked_status
    FROM jobs
    WHERE id = checked_job_id
    FOR UPDATE;
    IF NOT FOUND OR checked_type <> 'transform' THEN
        RETURN NULL;
    END IF;

    SELECT NOT EXISTS (
        SELECT 1
        FROM job_targets
        WHERE job_id = checked_job_id AND status <> 'succeeded'
    ) INTO every_target_succeeded;
    IF (checked_status = 'succeeded') IS DISTINCT FROM every_target_succeeded THEN
        RAISE EXCEPTION 'transform job succeeded status must exactly match all targets succeeded: job %',
            checked_job_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE FUNCTION nmcp_lock_job_target_parent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE checked_job_id nmcp_uuid_v4;
BEGIN
    checked_job_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.job_id ELSE NEW.job_id END;
    PERFORM 1 FROM jobs WHERE id = checked_job_id FOR UPDATE;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

-- Serialize target writers before their modifying statement finishes. Their
-- later deferred checks then receive a fresh snapshot that includes the prior
-- writer, rather than allowing two partial completions to create an invalid
-- all-succeeded/running aggregate by write skew.
CREATE TRIGGER job_targets_transform_success_serialize
BEFORE INSERT OR UPDATE OR DELETE ON job_targets
FOR EACH ROW EXECUTE FUNCTION nmcp_lock_job_target_parent();

CREATE CONSTRAINT TRIGGER jobs_transform_success_aggregate
AFTER INSERT OR UPDATE ON jobs
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_transform_job_success();
CREATE CONSTRAINT TRIGGER job_targets_transform_success_aggregate
AFTER INSERT OR UPDATE OR DELETE ON job_targets
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_transform_job_success();

-- The temporary default backfills legacy Renditions without fabricating
-- processor evidence. Dropping it makes every future publication explicit.
ALTER TABLE renditions
ADD COLUMN processor_audit jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE renditions
ALTER COLUMN processor_audit DROP DEFAULT;
ALTER TABLE renditions
ADD CONSTRAINT renditions_processor_audit_check CHECK (
    jsonb_typeof(processor_audit) = 'object'
    AND octet_length(processor_audit::text) <= 1048576
) NOT VALID;
ALTER TABLE renditions
VALIDATE CONSTRAINT renditions_processor_audit_check;

CREATE OR REPLACE FUNCTION nmcp_validate_rendition_provenance()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_media_id nmcp_uuid_v4;
    expected_media_snapshot nmcp_uuid_v4;
    expected_profile_key text;
    job_type text;
BEGIN
    SELECT o.media_id, j.media_id_snapshot, p.key, j.type
    INTO expected_media_id, expected_media_snapshot, expected_profile_key, job_type
    FROM job_targets AS jt
    JOIN jobs AS j ON j.id = jt.job_id
    JOIN originals AS o ON o.id = j.original_id
    JOIN profiles AS p ON p.id = jt.profile_id
    WHERE jt.id = NEW.job_target_id;

    IF expected_media_id IS NULL OR job_type <> 'transform'
       OR expected_media_id <> NEW.media_id OR expected_media_snapshot <> NEW.media_id THEN
        RAISE EXCEPTION 'rendition target provenance does not match media'
            USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'UPDATE' AND (
        NEW.id IS DISTINCT FROM OLD.id
        OR NEW.media_id IS DISTINCT FROM OLD.media_id
        OR NEW.job_target_id IS DISTINCT FROM OLD.job_target_id
        OR NEW.relative_path IS DISTINCT FROM OLD.relative_path
        OR NEW.mime_type IS DISTINCT FROM OLD.mime_type
        OR NEW.width IS DISTINCT FROM OLD.width
        OR NEW.height IS DISTINCT FROM OLD.height
        OR NEW.duration_ms IS DISTINCT FROM OLD.duration_ms
        OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
        OR NEW.sha256 IS DISTINCT FROM OLD.sha256
        OR NEW.processor_audit IS DISTINCT FROM OLD.processor_audit
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    ) THEN
        RAISE EXCEPTION 'rendition identity, file metadata, and processor audit are immutable'
            USING ERRCODE = '23514';
    END IF;
    NEW.profile_key := expected_profile_key;
    RETURN NEW;
END;
$$;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_check_transform_job_success() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_lock_job_target_parent() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_validate_rendition_provenance() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;
