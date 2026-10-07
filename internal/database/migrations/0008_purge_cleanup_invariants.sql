-- Persist purge file progress across Media cascade and constrain destructive
-- operations to the lifecycle/cleanup paths that can prove their preconditions.

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    PERFORM pg_catalog.set_config(
        'search_path', pg_catalog.quote_ident(target_schema) || ', pg_catalog, pg_temp', true
    );
END;
$$;

-- Match the global runtime lock order before installing destructive-operation
-- guards. The progress/report tables are new in this migration.
LOCK TABLE maintenance_state IN ACCESS EXCLUSIVE MODE;
LOCK TABLE media IN ACCESS EXCLUSIVE MODE;
LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE job_targets IN ACCESS EXCLUSIVE MODE;
LOCK TABLE originals IN ACCESS EXCLUSIVE MODE;
LOCK TABLE renditions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE reconciliation_reports IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE invalid_target_id text;
BEGIN
    SELECT jt.id::text
    INTO invalid_target_id
    FROM job_targets AS jt
    JOIN jobs AS j ON j.id = jt.job_id
    JOIN media AS m ON m.id = j.media_id_snapshot
    WHERE jt.status = 'succeeded'
      AND j.type = 'transform'
      AND NOT EXISTS (SELECT 1 FROM renditions AS r WHERE r.job_target_id = jt.id)
    ORDER BY jt.id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'live succeeded Target is missing its pre-migration Rendition: %', invalid_target_id
            USING ERRCODE = '23514';
    END IF;
END;
$$;

CREATE TABLE purge_file_progress (
    job_id nmcp_uuid_v4 NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    media_id_snapshot nmcp_uuid_v4 NOT NULL,
    object_kind text NOT NULL CHECK (object_kind IN ('original', 'rendition')),
    object_id nmcp_uuid_v4 NOT NULL,
    relative_path text NOT NULL CHECK (nmcp_is_relative_path(relative_path)),
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    disposition text NOT NULL DEFAULT 'pending'
        CHECK (disposition IN ('pending', 'deleted', 'missing')),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY (job_id, object_kind, object_id),
    UNIQUE (job_id, relative_path),
    CONSTRAINT purge_file_progress_completion_check CHECK (
        (disposition = 'pending' AND completed_at IS NULL)
        OR (disposition IN ('deleted', 'missing') AND completed_at IS NOT NULL)
    )
);

CREATE INDEX purge_file_progress_pending_idx
ON purge_file_progress (job_id, object_kind, object_id)
WHERE disposition = 'pending';

CREATE FUNCTION nmcp_validate_purge_file_progress()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_path text;
    expected_size bigint;
    purge_lease_token text := pg_catalog.current_setting('nmcp.purge_lease_token', true);
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.job_id IS DISTINCT FROM OLD.job_id
           OR NEW.media_id_snapshot IS DISTINCT FROM OLD.media_id_snapshot
           OR NEW.object_kind IS DISTINCT FROM OLD.object_kind
           OR NEW.object_id IS DISTINCT FROM OLD.object_id
           OR NEW.relative_path IS DISTINCT FROM OLD.relative_path
           OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
           OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'purge file progress identity and snapshot are immutable'
                USING ERRCODE = '23514';
        END IF;
        IF OLD.disposition <> 'pending'
           OR NEW.disposition NOT IN ('deleted', 'missing') THEN
            RAISE EXCEPTION 'purge file progress may complete exactly once'
                USING ERRCODE = '23514';
        END IF;
        PERFORM 1
        FROM jobs
        WHERE id = NEW.job_id
          AND type = 'purge'
          AND media_id_snapshot = NEW.media_id_snapshot
          AND status = 'running'
          AND lease_token::text = purge_lease_token
          AND lease_expires_at > clock_timestamp()
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'purge progress completion requires its live lease: %', NEW.job_id
                USING ERRCODE = '23514';
        END IF;
        NEW.completed_at := clock_timestamp();
        RETURN NEW;
    END IF;

    IF NEW.disposition <> 'pending' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'new purge file progress must start pending'
            USING ERRCODE = '23514';
    END IF;
    NEW.created_at := statement_timestamp();

    -- An inserting caller must already follow maintenance -> Media -> Job.
    -- Re-taking those locks in the same order makes direct SQL obey the same
    -- ordering and proves the progress belongs to a live started purge.
    PERFORM 1 FROM media WHERE id = NEW.media_id_snapshot AND deleted_at IS NOT NULL FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'purge progress requires existing deleted Media: %', NEW.media_id_snapshot
            USING ERRCODE = '23514';
    END IF;
    PERFORM 1
    FROM jobs
    WHERE id = NEW.job_id
      AND type = 'purge'
      AND media_id_snapshot = NEW.media_id_snapshot
      AND status = 'running'
      AND started_at IS NOT NULL
      AND lease_token::text = purge_lease_token
      AND lease_expires_at > clock_timestamp()
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'purge progress requires matching running purge Job: %', NEW.job_id
            USING ERRCODE = '23514';
    END IF;

    IF NEW.object_kind = 'original' THEN
        SELECT relative_path, size_bytes
        INTO expected_path, expected_size
        FROM originals
        WHERE id = NEW.object_id AND media_id = NEW.media_id_snapshot;
    ELSE
        SELECT relative_path, size_bytes
        INTO expected_path, expected_size
        FROM renditions
        WHERE id = NEW.object_id AND media_id = NEW.media_id_snapshot;
    END IF;
    IF NOT FOUND OR expected_path IS DISTINCT FROM NEW.relative_path
       OR expected_size IS DISTINCT FROM NEW.size_bytes THEN
        RAISE EXCEPTION 'purge progress snapshot does not match referenced object: %', NEW.object_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER purge_file_progress_validate
BEFORE INSERT OR UPDATE ON purge_file_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_purge_file_progress();
CREATE TRIGGER purge_file_progress_no_delete
BEFORE DELETE ON purge_file_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER purge_file_progress_no_truncate
BEFORE TRUNCATE ON purge_file_progress
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

CREATE TABLE rendition_cleanup_progress (
    id nmcp_uuid_v4 PRIMARY KEY,
    media_id_snapshot nmcp_uuid_v4 NOT NULL,
    rendition_id nmcp_uuid_v4 NOT NULL UNIQUE,
    job_target_id nmcp_uuid_v4 NOT NULL UNIQUE,
    relative_path text NOT NULL UNIQUE CHECK (nmcp_is_relative_path(relative_path)),
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    purge_after timestamptz NOT NULL,
    disposition text NOT NULL DEFAULT 'pending'
        CHECK (disposition IN ('pending', 'deleted', 'missing')),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    completed_at timestamptz,
    CONSTRAINT rendition_cleanup_progress_completion_check CHECK (
        (disposition = 'pending' AND completed_at IS NULL)
        OR (disposition IN ('deleted', 'missing') AND completed_at IS NOT NULL)
    )
);

CREATE INDEX rendition_cleanup_progress_pending_idx
ON rendition_cleanup_progress (purge_after, media_id_snapshot, rendition_id)
WHERE disposition = 'pending';

CREATE FUNCTION nmcp_validate_rendition_cleanup_progress()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_target_id nmcp_uuid_v4;
    expected_path text;
    expected_size bigint;
    expected_purge_after timestamptz;
    expected_current boolean;
    cleanup_progress_id text := pg_catalog.current_setting('nmcp.rendition_cleanup_progress_id', true);
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id
           OR NEW.media_id_snapshot IS DISTINCT FROM OLD.media_id_snapshot
           OR NEW.rendition_id IS DISTINCT FROM OLD.rendition_id
           OR NEW.job_target_id IS DISTINCT FROM OLD.job_target_id
           OR NEW.relative_path IS DISTINCT FROM OLD.relative_path
           OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
           OR NEW.purge_after IS DISTINCT FROM OLD.purge_after
           OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'Rendition cleanup progress identity and snapshot are immutable'
                USING ERRCODE = '23514';
        END IF;
        IF OLD.disposition <> 'pending'
           OR NEW.disposition NOT IN ('deleted', 'missing') THEN
            RAISE EXCEPTION 'Rendition cleanup progress may complete exactly once'
                USING ERRCODE = '23514';
        END IF;
        IF cleanup_progress_id IS NULL OR cleanup_progress_id <> NEW.id::text THEN
            RAISE EXCEPTION 'Rendition cleanup completion requires its operation boundary'
                USING ERRCODE = '23514';
        END IF;
        NEW.completed_at := clock_timestamp();
        RETURN NEW;
    END IF;

    IF NEW.disposition <> 'pending' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'new Rendition cleanup progress must start pending'
            USING ERRCODE = '23514';
    END IF;
    NEW.created_at := statement_timestamp();

    PERFORM 1 FROM media WHERE id = NEW.media_id_snapshot FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Rendition cleanup progress requires existing Media: %', NEW.media_id_snapshot
            USING ERRCODE = '23514';
    END IF;
    SELECT job_target_id, relative_path, size_bytes, purge_after, is_current
    INTO expected_target_id, expected_path, expected_size, expected_purge_after, expected_current
    FROM renditions
    WHERE id = NEW.rendition_id AND media_id = NEW.media_id_snapshot
    FOR UPDATE;
    IF NOT FOUND OR expected_current OR expected_purge_after IS NULL
       OR expected_purge_after > clock_timestamp()
       OR expected_target_id IS DISTINCT FROM NEW.job_target_id
       OR expected_path IS DISTINCT FROM NEW.relative_path
       OR expected_size IS DISTINCT FROM NEW.size_bytes
       OR expected_purge_after IS DISTINCT FROM NEW.purge_after THEN
        RAISE EXCEPTION 'cleanup progress requires an exact due non-current Rendition: %', NEW.rendition_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER rendition_cleanup_progress_validate
BEFORE INSERT OR UPDATE ON rendition_cleanup_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_rendition_cleanup_progress();
CREATE TRIGGER rendition_cleanup_progress_no_delete
BEFORE DELETE ON rendition_cleanup_progress
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER rendition_cleanup_progress_no_truncate
BEFORE TRUNCATE ON rendition_cleanup_progress
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

CREATE FUNCTION nmcp_guard_rendition_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE cleanup_progress_id text := pg_catalog.current_setting('nmcp.rendition_cleanup_progress_id', true);
BEGIN
    -- The owning Media is already absent when an ON DELETE CASCADE reaches the
    -- child. Preserve canonical Media purge without opening direct deletion.
    IF NOT EXISTS (SELECT 1 FROM media WHERE id = OLD.media_id) THEN
        RETURN OLD;
    END IF;
    PERFORM 1 FROM media WHERE id = OLD.media_id FOR UPDATE;
    IF cleanup_progress_id IS NULL OR NOT EXISTS (
        SELECT 1
        FROM rendition_cleanup_progress AS p
        WHERE p.id::text = cleanup_progress_id
          AND p.media_id_snapshot = OLD.media_id
          AND p.rendition_id = OLD.id
          AND p.job_target_id = OLD.job_target_id
          AND p.relative_path = OLD.relative_path
          AND p.size_bytes = OLD.size_bytes
          AND p.purge_after = OLD.purge_after
          AND p.disposition IN ('deleted', 'missing')
    ) THEN
        RAISE EXCEPTION 'Rendition deletion requires completed cleanup progress'
            USING ERRCODE = '23514';
    END IF;
    IF OLD.is_current OR OLD.purge_after IS NULL OR OLD.purge_after > clock_timestamp() THEN
        RAISE EXCEPTION 'only due non-current Rendition may be deleted: %', OLD.id
            USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER renditions_delete_guard
BEFORE DELETE ON renditions
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_rendition_delete();
CREATE TRIGGER renditions_no_truncate
BEFORE TRUNCATE ON renditions
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

CREATE OR REPLACE FUNCTION nmcp_check_target_rendition()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    checked_target_id nmcp_uuid_v4;
    target_status text;
    rendition_count bigint;
    live_media boolean;
    authorized_cleanup boolean;
BEGIN
    IF TG_TABLE_NAME = 'job_targets' THEN
        checked_target_id := COALESCE(NEW.id, OLD.id);
    ELSE
        checked_target_id := COALESCE(NEW.job_target_id, OLD.job_target_id);
    END IF;
    SELECT jt.status,
           EXISTS (SELECT 1 FROM jobs AS j JOIN media AS m ON m.id=j.media_id_snapshot WHERE j.id=jt.job_id),
           EXISTS (SELECT 1 FROM rendition_cleanup_progress AS p WHERE p.job_target_id=jt.id AND p.disposition IN ('deleted','missing'))
    INTO target_status, live_media, authorized_cleanup
    FROM job_targets AS jt
    WHERE jt.id = checked_target_id;
    IF target_status IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO rendition_count FROM renditions WHERE job_target_id = checked_target_id;
    IF target_status = 'succeeded' AND rendition_count <> 1
       AND NOT (rendition_count = 0 AND (NOT live_media OR authorized_cleanup)) THEN
        RAISE EXCEPTION 'succeeded target requires one retained or authorized-removed rendition'
            USING ERRCODE = '23514';
    ELSIF target_status <> 'succeeded' AND rendition_count <> 0 THEN
        RAISE EXCEPTION 'only a succeeded target may own a rendition'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER renditions_target_cardinality ON renditions;
CREATE CONSTRAINT TRIGGER renditions_target_cardinality
AFTER INSERT OR UPDATE OR DELETE ON renditions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_target_rendition();

CREATE FUNCTION nmcp_guard_media_purge_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    purge_job_id text := pg_catalog.current_setting('nmcp.purge_job_id', true);
    purge_lease_token text := pg_catalog.current_setting('nmcp.purge_lease_token', true);
BEGIN
    IF OLD.deleted_at IS NULL OR purge_job_id IS NULL THEN
        RAISE EXCEPTION 'Media deletion requires a live purge operation'
            USING ERRCODE = '23514';
    END IF;
    PERFORM 1
    FROM jobs
    WHERE id::text = purge_job_id
      AND type = 'purge'
      AND media_id_snapshot = OLD.id
      AND status = 'running'
      AND started_at IS NOT NULL
      AND lease_token::text = purge_lease_token
      AND lease_expires_at > clock_timestamp()
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Media deletion requires its live running purge Job: %', OLD.id
            USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM purge_file_progress
        WHERE job_id::text = purge_job_id AND disposition = 'pending'
    ) OR EXISTS (
        SELECT 1
        FROM originals AS o
        WHERE o.media_id = OLD.id
          AND NOT EXISTS (
              SELECT 1 FROM purge_file_progress AS p
              WHERE p.job_id::text = purge_job_id
                AND p.media_id_snapshot = OLD.id
                AND p.object_kind = 'original'
                AND p.object_id = o.id
                AND p.relative_path = o.relative_path
                AND p.size_bytes = o.size_bytes
                AND p.disposition IN ('deleted', 'missing')
          )
    ) OR EXISTS (
        SELECT 1
        FROM renditions AS r
        WHERE r.media_id = OLD.id
          AND NOT EXISTS (
              SELECT 1 FROM purge_file_progress AS p
              WHERE p.job_id::text = purge_job_id
                AND p.media_id_snapshot = OLD.id
                AND p.object_kind = 'rendition'
                AND p.object_id = r.id
                AND p.relative_path = r.relative_path
                AND p.size_bytes = r.size_bytes
                AND p.disposition IN ('deleted', 'missing')
          )
    ) THEN
        RAISE EXCEPTION 'Media deletion requires completed progress for every owned file: %', OLD.id
            USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER media_purge_delete_guard
BEFORE DELETE ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_media_purge_delete();
CREATE TRIGGER media_no_truncate
BEFORE TRUNCATE ON media
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

ALTER TABLE reconciliation_reports
ADD COLUMN report_kind text NOT NULL DEFAULT 'check'
    CHECK (report_kind IN ('check', 'repair')),
ADD COLUMN source_report_id nmcp_uuid_v4
    REFERENCES reconciliation_reports(id) ON DELETE RESTRICT;
ALTER TABLE reconciliation_reports
ADD CONSTRAINT reconciliation_reports_kind_shape_check CHECK (
    (report_kind = 'check' AND source_report_id IS NULL)
    OR (report_kind = 'repair' AND source_report_id IS NOT NULL)
);
CREATE INDEX reconciliation_reports_source_idx
ON reconciliation_reports (source_report_id, created_at, id) WHERE report_kind = 'repair';

CREATE FUNCTION nmcp_validate_reconciliation_report_link()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.report_kind = 'repair' AND NOT EXISTS (
        SELECT 1 FROM reconciliation_reports
        WHERE id = NEW.source_report_id AND report_kind = 'check'
    ) THEN
        RAISE EXCEPTION 'repair report must reference an existing check report: %', NEW.source_report_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER reconciliation_reports_link_validate
BEFORE INSERT ON reconciliation_reports
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_reconciliation_report_link();

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_validate_purge_file_progress() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_rendition_delete() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_validate_rendition_cleanup_progress() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_check_target_rendition() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_media_purge_delete() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_validate_reconciliation_report_link() SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;
