-- Bound deleted-media retention and enforce lifecycle invariants even for
-- direct SQL writers. Purge jobs intentionally retain no Media foreign key so
-- their history survives physical deletion.

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    PERFORM pg_catalog.set_config(
        'search_path', pg_catalog.quote_ident(target_schema) || ', pg_catalog, pg_temp', true
    );
END;
$$;

-- Close the gap between preflight and constraint/trigger installation. Acquire
-- coordination tables in the same global order as runtime lifecycle writers:
-- maintenance prefix, Media, Jobs, then configuration.
LOCK TABLE maintenance_state IN ACCESS EXCLUSIVE MODE;
LOCK TABLE media IN ACCESS EXCLUSIVE MODE;
LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE system_config IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    invalid_job_id text;
    invalid_media_id text;
    invalid_status text;
BEGIN
    SELECT j.id::text, j.media_id_snapshot::text, j.status
    INTO invalid_job_id, invalid_media_id, invalid_status
    FROM jobs AS j
    LEFT JOIN media AS m ON m.id = j.media_id_snapshot
    WHERE j.type = 'purge'
      AND j.status IN ('queued', 'running', 'failed')
      AND (m.id IS NULL OR m.deleted_at IS NULL)
    ORDER BY j.id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'unfinished purge job requires existing deleted Media: job=% media=% status=%',
            invalid_job_id, invalid_media_id, invalid_status
            USING ERRCODE = '23514';
    END IF;
END;
$$;

DO $$
DECLARE invalid_retention integer;
BEGIN
    SELECT deleted_media_retention_days
    INTO invalid_retention
    FROM system_config
    WHERE deleted_media_retention_days IS NOT NULL
      AND deleted_media_retention_days NOT BETWEEN 0 AND 36500
    ORDER BY id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'existing deleted media retention is outside 0..36500 days: %',
            invalid_retention
            USING ERRCODE = '23514';
    END IF;
END;
$$;

ALTER TABLE system_config
ADD CONSTRAINT system_config_deleted_media_retention_max_check
CHECK (deleted_media_retention_days <= 36500) NOT VALID;
ALTER TABLE system_config
VALIDATE CONSTRAINT system_config_deleted_media_retention_max_check;

ALTER TABLE media
ADD CONSTRAINT media_delete_deadline_order_check
CHECK (purge_after IS NULL OR (deleted_at IS NOT NULL AND purge_after >= deleted_at)) NOT VALID;
ALTER TABLE media
VALIDATE CONSTRAINT media_delete_deadline_order_check;

CREATE FUNCTION nmcp_guard_purge_job_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    media_deleted_at timestamptz;
BEGIN
    IF NEW.type <> 'purge' THEN
        RETURN NEW;
    END IF;

    SELECT deleted_at
    INTO media_deleted_at
    FROM media
    WHERE id = NEW.media_id_snapshot
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'purge job requires existing Media: %', NEW.media_id_snapshot
            USING ERRCODE = '23514';
    END IF;
    IF media_deleted_at IS NULL THEN
        RAISE EXCEPTION 'purge job requires deleted Media: %', NEW.media_id_snapshot
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER jobs_purge_media_guard
BEFORE INSERT ON jobs
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_purge_job_insert();

CREATE FUNCTION nmcp_guard_media_undelete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.deleted_at IS NULL THEN
        RETURN NEW;
    END IF;

    IF NEW.deleted_at IS NOT NULL OR NEW.purge_after IS NOT NULL THEN
        IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at
           OR NEW.purge_after IS DISTINCT FROM OLD.purge_after THEN
            RAISE EXCEPTION 'deleted Media snapshot is immutable: %', OLD.id
                USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    -- Updating Media acquired its row lock before this trigger. Lock every
    -- related purge Job in stable order before deciding whether restore is safe.
    PERFORM 1
    FROM jobs
    WHERE type = 'purge' AND media_id_snapshot = OLD.id
    ORDER BY id
    FOR UPDATE;

    IF EXISTS (
        SELECT 1
        FROM jobs
        WHERE type = 'purge'
          AND media_id_snapshot = OLD.id
          AND (started_at IS NOT NULL OR status IN ('queued', 'running', 'failed'))
    ) THEN
        RAISE EXCEPTION 'Media cannot be restored while purge work is blocking: %', OLD.id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER media_undelete_guard
BEFORE UPDATE OF deleted_at, purge_after ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_media_undelete();

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_purge_job_insert() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_media_undelete() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;
