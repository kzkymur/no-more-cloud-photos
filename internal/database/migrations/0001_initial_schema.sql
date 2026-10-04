-- Initial Core metadata schema. This migration is forward-only; repairs are
-- shipped as higher-numbered migrations and rollback restores a tested backup.

CREATE FUNCTION nmcp_is_uuid_v4(value uuid)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN (get_byte(uuid_send(value), 6) >> 4) = 4
   AND (get_byte(uuid_send(value), 8) & 192) = 128;

CREATE DOMAIN nmcp_uuid_v4 AS uuid
CHECK (nmcp_is_uuid_v4(VALUE));

CREATE FUNCTION nmcp_is_sha256(value text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN value ~ '^[0-9a-f]{64}$';

CREATE FUNCTION nmcp_is_relative_path(value text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN value <> ''
   AND value !~ '(^|/)\.\.?(/|$)'
   AND left(value, 1) <> '/';

CREATE FUNCTION nmcp_is_mime_type(value text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN value = lower(value)
   AND value ~ '^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$';

CREATE FUNCTION nmcp_valid_mime_types(mime_values text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN cardinality(mime_values) > 0
   AND array_position(mime_values, NULL) IS NULL
   AND NOT EXISTS (
       SELECT 1 FROM unnest(mime_values) AS mime(value)
       WHERE NOT nmcp_is_mime_type(mime.value)
   )
   AND cardinality(mime_values) = (
       SELECT count(DISTINCT mime.value) FROM unnest(mime_values) AS mime(value)
   );

CREATE FUNCTION nmcp_valid_relative_paths(path_values text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN array_position(path_values, NULL) IS NULL
   AND NOT EXISTS (
       SELECT 1 FROM unnest(path_values) AS path(value)
       WHERE NOT nmcp_is_relative_path(path.value)
   );

CREATE FUNCTION nmcp_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME
        USING ERRCODE = '23514';
END;
$$;

CREATE TABLE system_config (
    id smallint PRIMARY KEY CHECK (id = 1),
    deleted_media_retention_days integer
        CHECK (deleted_media_retention_days >= 0),
    superseded_rendition_retention_days integer
        CHECK (superseded_rendition_retention_days >= 0),
    default_timezone text NOT NULL CHECK (default_timezone <> ''),
    db_backup_interval_hours integer NOT NULL
        CHECK (db_backup_interval_hours > 0),
    db_backup_retention_days integer NOT NULL
        CHECK (db_backup_retention_days > 0),
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp()
);

CREATE FUNCTION nmcp_validate_system_config()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_timezone_names
        WHERE name = NEW.default_timezone
    ) THEN
        RAISE EXCEPTION 'unknown PostgreSQL timezone name: %', NEW.default_timezone
            USING ERRCODE = '23514';
    END IF;
    NEW.updated_at := statement_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER system_config_validate
BEFORE INSERT OR UPDATE ON system_config
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_system_config();

INSERT INTO system_config (
    id,
    deleted_media_retention_days,
    superseded_rendition_retention_days,
    default_timezone,
    db_backup_interval_hours,
    db_backup_retention_days
) VALUES (1, NULL, NULL, 'Asia/Tokyo', 24, 30);

CREATE TABLE media (
    id nmcp_uuid_v4 PRIMARY KEY,
    media_type text NOT NULL CHECK (media_type <> '' AND media_type = lower(media_type)),
    source_metadata jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(source_metadata) = 'object'),
    taken_at timestamptz,
    taken_at_source text NOT NULL
        CHECK (taken_at_source IN ('embedded_offset', 'default_timezone', 'unknown')),
    taken_at_timezone text,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    deleted_at timestamptz,
    purge_after timestamptz,
    CONSTRAINT media_capture_source_check CHECK (
        (taken_at_source = 'unknown' AND taken_at IS NULL AND taken_at_timezone IS NULL)
        OR (taken_at_source = 'embedded_offset' AND taken_at IS NOT NULL AND taken_at_timezone IS NULL)
        OR (taken_at_source = 'default_timezone' AND taken_at IS NOT NULL AND taken_at_timezone IS NOT NULL)
    ),
    CONSTRAINT media_delete_deadline_check CHECK (deleted_at IS NOT NULL OR purge_after IS NULL)
);

CREATE FUNCTION nmcp_validate_media_timezone()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.taken_at_source = 'default_timezone' AND NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_timezone_names
        WHERE name = NEW.taken_at_timezone
    ) THEN
        RAISE EXCEPTION 'unknown captured PostgreSQL timezone name: %', NEW.taken_at_timezone
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER media_timezone_validate
BEFORE INSERT OR UPDATE OF taken_at_source, taken_at_timezone ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_media_timezone();

CREATE INDEX media_list_order_idx
ON media (taken_at DESC NULLS LAST, id DESC);
CREATE INDEX media_due_purge_idx
ON media (purge_after, id)
WHERE deleted_at IS NOT NULL AND purge_after IS NOT NULL;

CREATE TABLE originals (
    id nmcp_uuid_v4 PRIMARY KEY,
    media_id nmcp_uuid_v4 NOT NULL UNIQUE
        REFERENCES media(id) ON DELETE CASCADE,
    sha256 text NOT NULL UNIQUE CHECK (nmcp_is_sha256(sha256)),
    original_filename text
        CHECK (original_filename <> '' AND octet_length(original_filename) <= 255),
    relative_path text NOT NULL UNIQUE CHECK (nmcp_is_relative_path(relative_path)),
    mime_type text NOT NULL CHECK (nmcp_is_mime_type(mime_type)),
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    width integer,
    height integer,
    duration_ms bigint CHECK (duration_ms >= 0),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    exif_json jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(exif_json) = 'object'),
    CONSTRAINT originals_dimensions_check CHECK (
        (width IS NULL AND height IS NULL)
        OR (width > 0 AND height > 0)
    )
);

CREATE TABLE profiles (
    id nmcp_uuid_v4 PRIMARY KEY,
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,63}$'),
    version integer NOT NULL CHECK (version >= 1),
    status text NOT NULL CHECK (status IN ('draft', 'active', 'retired')),
    input_mime_types text[] NOT NULL CHECK (nmcp_valid_mime_types(input_mime_types)),
    processor text NOT NULL CHECK (processor ~ '^[a-z][a-z0-9_-]{0,63}$'),
    parameters_schema_version integer NOT NULL CHECK (parameters_schema_version >= 1),
    parameters jsonb NOT NULL CHECK (jsonb_typeof(parameters) = 'object'),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    activated_at timestamptz,
    retired_at timestamptz,
    UNIQUE (key, version),
    CONSTRAINT profiles_lifecycle_timestamps_check CHECK (
        (status = 'draft' AND activated_at IS NULL AND retired_at IS NULL)
        OR (status = 'active' AND activated_at IS NOT NULL AND retired_at IS NULL)
        OR (status = 'retired' AND activated_at IS NOT NULL AND retired_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX profiles_one_active_key_idx
ON profiles (key) WHERE status = 'active';
CREATE INDEX profiles_list_idx ON profiles (key ASC, version DESC);

CREATE FUNCTION nmcp_profile_lifecycle()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    greatest_activated integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.activated_at IS NOT NULL OR NEW.retired_at IS NOT NULL THEN
            RAISE EXCEPTION 'profiles must be inserted as draft'
                USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.key IS DISTINCT FROM OLD.key
       OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.input_mime_types IS DISTINCT FROM OLD.input_mime_types
       OR NEW.processor IS DISTINCT FROM OLD.processor
       OR NEW.parameters_schema_version IS DISTINCT FROM OLD.parameters_schema_version
       OR NEW.parameters IS DISTINCT FROM OLD.parameters
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'profile recipe and identity are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.status = 'draft' AND NEW.status = 'active' THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(1313686352, pg_catalog.hashtext(OLD.key));
        SELECT max(version) INTO greatest_activated
        FROM profiles
        WHERE key = OLD.key AND activated_at IS NOT NULL AND id <> OLD.id;
        IF greatest_activated IS NOT NULL AND OLD.version <= greatest_activated THEN
            RAISE EXCEPTION 'profile version % is not greater than activated version %', OLD.version, greatest_activated
                USING ERRCODE = '23514';
        END IF;
        UPDATE profiles
        SET status = 'retired'
        WHERE key = OLD.key AND status = 'active' AND id <> OLD.id;
        NEW.activated_at := statement_timestamp();
        NEW.retired_at := NULL;
        RETURN NEW;
    ELSIF OLD.status = 'active' AND NEW.status = 'retired' THEN
        NEW.activated_at := OLD.activated_at;
        NEW.retired_at := statement_timestamp();
        RETURN NEW;
    ELSIF NEW.status = OLD.status
          AND NEW.activated_at IS NOT DISTINCT FROM OLD.activated_at
          AND NEW.retired_at IS NOT DISTINCT FROM OLD.retired_at THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'invalid profile lifecycle transition: % to %', OLD.status, NEW.status
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER profiles_lifecycle
BEFORE INSERT OR UPDATE ON profiles
FOR EACH ROW EXECUTE FUNCTION nmcp_profile_lifecycle();

CREATE TABLE jobs (
    id nmcp_uuid_v4 PRIMARY KEY,
    type text NOT NULL CHECK (type IN ('transform', 'purge')),
    original_id nmcp_uuid_v4 REFERENCES originals(id) ON DELETE SET NULL,
    media_id_snapshot nmcp_uuid_v4 NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts integer NOT NULL CHECK (max_attempts > 0 AND max_attempts >= attempts),
    available_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    lease_token nmcp_uuid_v4,
    lease_expires_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    error_code text,
    error_message text,
    cancelled_at timestamptz,
    cancel_reason text,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CONSTRAINT jobs_lease_check CHECK (
        (status = 'running' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL AND started_at IS NOT NULL)
        OR (status <> 'running' AND lease_token IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT jobs_finished_check CHECK (
        (status IN ('succeeded', 'failed', 'cancelled') AND finished_at IS NOT NULL)
        OR (status IN ('queued', 'running') AND finished_at IS NULL)
    ),
    CONSTRAINT jobs_error_check CHECK ((error_code IS NULL) = (error_message IS NULL)),
    CONSTRAINT jobs_cancel_check CHECK (
        (status = 'cancelled' AND type = 'purge' AND cancelled_at IS NOT NULL
         AND cancel_reason = 'media_restored' AND started_at IS NULL)
        OR (status <> 'cancelled' AND cancelled_at IS NULL AND cancel_reason IS NULL)
    )
);

CREATE FUNCTION nmcp_validate_job_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    original_media_id nmcp_uuid_v4;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id
           OR NEW.type IS DISTINCT FROM OLD.type
           OR NEW.media_id_snapshot IS DISTINCT FROM OLD.media_id_snapshot
           OR (OLD.original_id IS NULL AND NEW.original_id IS NOT NULL)
           OR (OLD.original_id IS NOT NULL AND NEW.original_id IS NOT NULL
               AND NEW.original_id IS DISTINCT FROM OLD.original_id) THEN
            RAISE EXCEPTION 'job identity and input snapshot are immutable'
                USING ERRCODE = '23514';
        END IF;
    END IF;

    IF NEW.type = 'purge' THEN
        IF NEW.original_id IS NOT NULL THEN
            RAISE EXCEPTION 'purge job cannot reference an original'
                USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.original_id IS NULL THEN
        IF TG_OP = 'INSERT' THEN
            RAISE EXCEPTION 'new transform job requires an original'
                USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    SELECT media_id INTO original_media_id FROM originals WHERE id = NEW.original_id;
    IF original_media_id IS NULL OR original_media_id <> NEW.media_id_snapshot THEN
        RAISE EXCEPTION 'transform job original does not match media snapshot'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER jobs_identity_validate
BEFORE INSERT OR UPDATE OF id, type, original_id, media_id_snapshot ON jobs
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_job_identity();

CREATE INDEX jobs_original_id_idx ON jobs (original_id);
CREATE INDEX jobs_list_idx ON jobs (created_at DESC, id DESC);
CREATE INDEX jobs_status_list_idx ON jobs (status, created_at DESC, id DESC);
CREATE INDEX jobs_media_list_idx ON jobs (media_id_snapshot, created_at DESC, id DESC);
CREATE INDEX jobs_dequeue_idx ON jobs (available_at, created_at, id)
WHERE status = 'queued';
CREATE UNIQUE INDEX jobs_one_active_purge_idx ON jobs (media_id_snapshot)
WHERE type = 'purge' AND status IN ('queued', 'running', 'failed');

CREATE TABLE job_targets (
    id nmcp_uuid_v4 PRIMARY KEY,
    job_id nmcp_uuid_v4 NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    profile_id nmcp_uuid_v4 NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    status text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    error_code text,
    error_message text,
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    UNIQUE (job_id, profile_id),
    CONSTRAINT job_targets_error_check CHECK (
        ((error_code IS NULL) = (error_message IS NULL))
        AND (status = 'failed' OR error_code IS NULL)
    )
);

CREATE FUNCTION nmcp_validate_job_target_lifecycle()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'pending' OR NEW.attempts <> 0 OR NEW.error_code IS NOT NULL THEN
            RAISE EXCEPTION 'job targets must be inserted pending without attempts or errors'
                USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.job_id IS DISTINCT FROM OLD.job_id
       OR NEW.profile_id IS DISTINCT FROM OLD.profile_id
       OR NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'job target identity is immutable and attempts cannot decrease'
            USING ERRCODE = '23514';
    END IF;
    IF NOT (
        NEW.status = OLD.status
        OR (OLD.status = 'pending' AND NEW.status IN ('succeeded', 'failed'))
        OR (OLD.status = 'failed' AND NEW.status = 'pending')
    ) THEN
        RAISE EXCEPTION 'invalid job target transition: % to %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER job_targets_lifecycle
BEFORE INSERT OR UPDATE ON job_targets
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_job_target_lifecycle();

CREATE INDEX job_targets_job_id_idx ON job_targets (job_id);
CREATE INDEX job_targets_profile_id_idx ON job_targets (profile_id);

CREATE FUNCTION nmcp_check_job_target_cardinality()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    checked_job_id nmcp_uuid_v4;
    checked_type text;
    target_count bigint;
BEGIN
    IF TG_TABLE_NAME = 'jobs' THEN
        checked_job_id := NEW.id;
    ELSIF TG_OP = 'DELETE' THEN
        checked_job_id := OLD.job_id;
    ELSE
        checked_job_id := NEW.job_id;
    END IF;
    SELECT type INTO checked_type FROM jobs WHERE id = checked_job_id;
    IF checked_type IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO target_count FROM job_targets WHERE job_id = checked_job_id;
    IF checked_type = 'transform' AND target_count < 1 THEN
        RAISE EXCEPTION 'transform job requires at least one target'
            USING ERRCODE = '23514';
    ELSIF checked_type = 'purge' AND target_count <> 0 THEN
        RAISE EXCEPTION 'purge job cannot have targets'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER jobs_target_cardinality
AFTER INSERT OR UPDATE ON jobs
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_job_target_cardinality();
CREATE CONSTRAINT TRIGGER job_targets_cardinality
AFTER INSERT OR UPDATE OR DELETE ON job_targets
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_job_target_cardinality();

CREATE TABLE renditions (
    id nmcp_uuid_v4 PRIMARY KEY,
    media_id nmcp_uuid_v4 NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    job_target_id nmcp_uuid_v4 NOT NULL UNIQUE REFERENCES job_targets(id) ON DELETE RESTRICT,
    profile_key text NOT NULL,
    is_current boolean NOT NULL DEFAULT false,
    purge_after timestamptz,
    relative_path text NOT NULL UNIQUE CHECK (nmcp_is_relative_path(relative_path)),
    mime_type text NOT NULL CHECK (nmcp_is_mime_type(mime_type)),
    width integer,
    height integer,
    duration_ms bigint CHECK (duration_ms >= 0),
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    sha256 text NOT NULL CHECK (nmcp_is_sha256(sha256)),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CONSTRAINT renditions_dimensions_check CHECK (
        (width IS NULL AND height IS NULL)
        OR (width > 0 AND height > 0)
    ),
    CONSTRAINT renditions_current_deadline_check CHECK (NOT is_current OR purge_after IS NULL)
);

CREATE INDEX renditions_media_id_idx ON renditions (media_id);
CREATE INDEX renditions_job_target_id_idx ON renditions (job_target_id);
CREATE UNIQUE INDEX renditions_one_current_key_idx
ON renditions (media_id, profile_key) WHERE is_current;
CREATE INDEX renditions_cleanup_idx ON renditions (purge_after, media_id, id)
WHERE NOT is_current AND purge_after IS NOT NULL;

CREATE FUNCTION nmcp_validate_rendition_provenance()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_media_id nmcp_uuid_v4;
    expected_profile_key text;
    job_type text;
BEGIN
    SELECT o.media_id, p.key, j.type
    INTO expected_media_id, expected_profile_key, job_type
    FROM job_targets AS jt
    JOIN jobs AS j ON j.id = jt.job_id
    JOIN originals AS o ON o.id = j.original_id
    JOIN profiles AS p ON p.id = jt.profile_id
    WHERE jt.id = NEW.job_target_id;

    IF expected_media_id IS NULL OR job_type <> 'transform' OR expected_media_id <> NEW.media_id THEN
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
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    ) THEN
        RAISE EXCEPTION 'rendition identity and file metadata are immutable'
            USING ERRCODE = '23514';
    END IF;
    NEW.profile_key := expected_profile_key;
    RETURN NEW;
END;
$$;

CREATE TRIGGER renditions_provenance
BEFORE INSERT OR UPDATE ON renditions
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_rendition_provenance();

CREATE FUNCTION nmcp_check_target_rendition()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    checked_target_id nmcp_uuid_v4;
    target_status text;
    rendition_count bigint;
BEGIN
    IF TG_TABLE_NAME = 'job_targets' THEN
        checked_target_id := NEW.id;
    ELSE
        checked_target_id := NEW.job_target_id;
    END IF;
    SELECT status INTO target_status FROM job_targets WHERE id = checked_target_id;
    IF target_status IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO rendition_count FROM renditions WHERE job_target_id = checked_target_id;
    IF target_status = 'succeeded' AND rendition_count <> 1 THEN
        RAISE EXCEPTION 'succeeded target requires exactly one rendition'
            USING ERRCODE = '23514';
    ELSIF target_status <> 'succeeded' AND rendition_count <> 0 THEN
        RAISE EXCEPTION 'only a succeeded target may own a rendition'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER job_targets_rendition_cardinality
AFTER INSERT OR UPDATE ON job_targets
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_target_rendition();
CREATE CONSTRAINT TRIGGER renditions_target_cardinality
AFTER INSERT OR UPDATE ON renditions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_check_target_rendition();

CREATE TABLE idempotency_requests (
    scope text NOT NULL CHECK (scope = 'POST /media'),
    key text NOT NULL CHECK (key ~ '^[!-~]{1,128}$'),
    request_hash text NOT NULL CHECK (nmcp_is_sha256(request_hash)),
    http_status integer NOT NULL CHECK (http_status IN (201, 409)),
    response_body jsonb NOT NULL CHECK (jsonb_typeof(response_body) = 'object'),
    media_id_snapshot nmcp_uuid_v4,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (scope, key)
);

CREATE TRIGGER idempotency_requests_immutable
BEFORE UPDATE OR DELETE ON idempotency_requests
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();

CREATE TABLE change_feed_state (
    id smallint PRIMARY KEY CHECK (id = 1),
    last_position bigint NOT NULL CHECK (last_position >= 0)
);
INSERT INTO change_feed_state (id, last_position) VALUES (1, 0);

CREATE TABLE change_events (
    id nmcp_uuid_v4 NOT NULL UNIQUE,
    position bigint PRIMARY KEY CHECK (position > 0),
    event_type text NOT NULL CHECK (event_type IN ('media_upsert', 'media_deleted', 'media_purged')),
    reason text NOT NULL,
    media_id nmcp_uuid_v4 NOT NULL,
    payload jsonb,
    occurred_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CONSTRAINT change_events_shape_check CHECK (
        (event_type = 'media_upsert' AND reason IN ('upload', 'rendition_current', 'restore')
         AND jsonb_typeof(payload) = 'object')
        OR (event_type = 'media_deleted' AND reason = 'logical_delete' AND payload IS NULL)
        OR (event_type = 'media_purged' AND reason = 'physical_purge' AND payload IS NULL)
    )
);

CREATE TRIGGER change_events_immutable
BEFORE UPDATE OR DELETE ON change_events
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();

CREATE TABLE backup_runs (
    id nmcp_uuid_v4 PRIMARY KEY,
    status text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    started_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    finished_at timestamptz,
    config_snapshot jsonb NOT NULL CHECK (jsonb_typeof(config_snapshot) = 'object'),
    final_relative_path text CHECK (final_relative_path IS NULL OR nmcp_is_relative_path(final_relative_path)),
    size_bytes bigint CHECK (size_bytes IS NULL OR size_bytes >= 0),
    sha256 text CHECK (sha256 IS NULL OR nmcp_is_sha256(sha256)),
    manifest jsonb CHECK (manifest IS NULL OR jsonb_typeof(manifest) = 'object'),
    postgres_version text,
    tool_version text,
    error_code text,
    error_message text,
    notification_status text NOT NULL DEFAULT 'pending'
        CHECK (notification_status IN ('pending', 'succeeded', 'failed')),
    notification_error text,
    CONSTRAINT backup_runs_error_check CHECK ((error_code IS NULL) = (error_message IS NULL)),
    CONSTRAINT backup_runs_notification_check CHECK (
        (notification_status = 'failed' AND notification_error IS NOT NULL)
        OR (notification_status <> 'failed' AND notification_error IS NULL)
    ),
    CONSTRAINT backup_runs_state_check CHECK (
        (status = 'running' AND finished_at IS NULL AND final_relative_path IS NULL
         AND size_bytes IS NULL AND sha256 IS NULL AND manifest IS NULL AND error_code IS NULL)
        OR (status = 'succeeded' AND finished_at IS NOT NULL AND final_relative_path IS NOT NULL
            AND size_bytes IS NOT NULL AND sha256 IS NOT NULL AND manifest IS NOT NULL
            AND postgres_version IS NOT NULL AND tool_version IS NOT NULL AND error_code IS NULL)
        OR (status = 'failed' AND finished_at IS NOT NULL AND error_code IS NOT NULL)
    )
);

CREATE INDEX backup_runs_succeeded_idx
ON backup_runs (finished_at DESC, id DESC) WHERE status = 'succeeded';
CREATE INDEX backup_runs_running_idx ON backup_runs (started_at, id) WHERE status = 'running';

CREATE TABLE maintenance_state (
    id smallint PRIMARY KEY CHECK (id = 1),
    mode text NOT NULL CHECK (mode IN ('normal', 'maintenance')),
    reason text,
    owner text,
    entered_at timestamptz,
    successful_check_report jsonb
        CHECK (successful_check_report IS NULL OR jsonb_typeof(successful_check_report) = 'object'),
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CONSTRAINT maintenance_state_shape_check CHECK (
        (mode = 'normal' AND reason IS NULL AND owner IS NULL AND entered_at IS NULL)
        OR (mode = 'maintenance' AND reason IS NOT NULL AND owner IS NOT NULL AND entered_at IS NOT NULL)
    )
);
INSERT INTO maintenance_state (id, mode) VALUES (1, 'normal');

CREATE TABLE admin_audit (
    id nmcp_uuid_v4 PRIMARY KEY,
    command text NOT NULL CHECK (command <> ''),
    actor text NOT NULL CHECK (actor <> ''),
    host text NOT NULL CHECK (host <> ''),
    sanitized_arguments jsonb NOT NULL CHECK (jsonb_typeof(sanitized_arguments) = 'object'),
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'partial')),
    affected_ids nmcp_uuid_v4[] NOT NULL DEFAULT '{}',
    error_code text,
    error_message text,
    started_at timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    CONSTRAINT admin_audit_error_check CHECK ((error_code IS NULL) = (error_message IS NULL)),
    CONSTRAINT admin_audit_time_check CHECK (finished_at >= started_at)
);

CREATE TRIGGER admin_audit_immutable
BEFORE UPDATE OR DELETE ON admin_audit
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();

CREATE TABLE admin_batches (
    id nmcp_uuid_v4 PRIMARY KEY,
    identity_key text NOT NULL UNIQUE CHECK (identity_key <> ''),
    operation text NOT NULL CHECK (operation IN ('regenerate', 'jobs_retry')),
    status text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    profile_id nmcp_uuid_v4 REFERENCES profiles(id) ON DELETE RESTRICT,
    config_snapshot jsonb NOT NULL CHECK (jsonb_typeof(config_snapshot) = 'object'),
    high_water jsonb NOT NULL CHECK (jsonb_typeof(high_water) = 'object'),
    checkpoint jsonb NOT NULL CHECK (jsonb_typeof(checkpoint) = 'object'),
    total_count bigint NOT NULL DEFAULT 0 CHECK (total_count >= 0),
    succeeded_count bigint NOT NULL DEFAULT 0 CHECK (succeeded_count >= 0),
    failed_count bigint NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    error_code text,
    error_message text,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    finished_at timestamptz,
    CONSTRAINT admin_batches_counts_check CHECK (succeeded_count + failed_count <= total_count),
    CONSTRAINT admin_batches_error_check CHECK ((error_code IS NULL) = (error_message IS NULL)),
    CONSTRAINT admin_batches_finished_check CHECK (
        (status = 'running' AND finished_at IS NULL)
        OR (status <> 'running' AND finished_at IS NOT NULL)
    )
);

CREATE INDEX admin_batches_profile_id_idx ON admin_batches (profile_id);
CREATE INDEX admin_batches_resume_idx ON admin_batches (status, updated_at, id);

CREATE TABLE reconciliation_reports (
    id nmcp_uuid_v4 PRIMARY KEY,
    scope text NOT NULL CHECK (scope IN ('files', 'db', 'all')),
    findings jsonb NOT NULL CHECK (jsonb_typeof(findings) = 'array'),
    repair_disposition jsonb NOT NULL CHECK (jsonb_typeof(repair_disposition) = 'object'),
    quarantine_paths text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    completed_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CHECK (completed_at >= created_at),
    CHECK (nmcp_valid_relative_paths(quarantine_paths))
);

CREATE TRIGGER reconciliation_reports_immutable
BEFORE UPDATE OR DELETE ON reconciliation_reports
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
