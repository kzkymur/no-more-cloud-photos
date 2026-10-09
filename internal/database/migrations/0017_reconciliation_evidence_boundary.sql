-- Reconciliation evidence is normalized, append-only, and isolated behind
-- distinct check and repair capabilities. Storage attempt ownership becomes
-- durable before reconciliation can classify an attempt temp as abandoned.

DO $$
DECLARE required_role text;
BEGIN
    FOREACH required_role IN ARRAY ARRAY[
        'nmcp_runtime','nmcp_worker_runtime','nmcp_check_runtime','nmcp_repair_runtime',
        'nmcp_check_function_owner','nmcp_repair_function_owner'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=required_role) THEN
            RAISE EXCEPTION 'required database role is absent: %',required_role USING ERRCODE='55000';
        END IF;
    END LOOP;
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname IN ('nmcp_check_runtime','nmcp_repair_runtime','nmcp_check_function_owner','nmcp_repair_function_owner')
          AND (rolcanlogin OR rolinherit)
    ) OR pg_catalog.pg_has_role('nmcp_check_runtime','nmcp_repair_runtime','MEMBER')
      OR pg_catalog.pg_has_role('nmcp_repair_runtime','nmcp_check_runtime','MEMBER')
      OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_check_runtime','MEMBER')
      OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_repair_runtime','MEMBER')
      OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_check_runtime','MEMBER')
      OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_repair_runtime','MEMBER') THEN
        RAISE EXCEPTION 'NMCP reconciliation roles do not match the isolated boundary' USING ERRCODE='55000';
    END IF;
END;
$$;

LOCK TABLE maintenance_state IN ACCESS EXCLUSIVE MODE;
LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE reconciliation_reports IN ACCESS EXCLUSIVE MODE;

ALTER TABLE reconciliation_reports
ADD COLUMN format text NOT NULL DEFAULT 'legacy'
    CHECK (format='legacy');

CREATE FUNCTION nmcp_reject_legacy_reconciliation_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'legacy reconciliation reports are closed' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER reconciliation_reports_no_insert
BEFORE INSERT ON reconciliation_reports
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_legacy_reconciliation_insert();

CREATE TABLE reconciliation_check_reports (
    id nmcp_uuid_v4 PRIMARY KEY,
    format_version smallint NOT NULL CHECK (format_version=1),
    classifier_version smallint NOT NULL CHECK (classifier_version=1),
    scope text NOT NULL CHECK (scope IN ('files','db','all')),
    migration_version bigint NOT NULL CHECK (migration_version>=17),
    migration_name text NOT NULL CHECK (migration_name<>''),
    migration_checksum text NOT NULL CHECK (nmcp_is_sha256(migration_checksum)),
    db_snapshot_started_at timestamptz NOT NULL,
    db_cutoff_at timestamptz NOT NULL,
    db_snapshot_ended_at timestamptz NOT NULL,
    fs_scan_started_at timestamptz NOT NULL,
    temp_cutoff_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (db_snapshot_started_at<=db_cutoff_at AND db_cutoff_at<=db_snapshot_ended_at),
    CHECK (db_snapshot_ended_at<=fs_scan_started_at),
    CHECK (temp_cutoff_at=db_cutoff_at-interval '48 hours')
);

CREATE TABLE reconciliation_check_findings (
    id nmcp_uuid_v4 PRIMARY KEY,
    report_id nmcp_uuid_v4 NOT NULL REFERENCES reconciliation_check_reports(id) ON DELETE RESTRICT,
    ordinal bigint NOT NULL CHECK (ordinal>0),
    kind text NOT NULL CHECK (kind IN (
        'aged_attempt_temp','recent_or_live_temp','final_orphan','referenced_missing',
        'size_mismatch','sha256_mismatch','invalid_current','invalid_provenance',
        'expired_media_candidate','expired_rendition_candidate','terminal_delete_residue',
        'unexpected_path','unexpected_type','symlink','unstable_observation',
        'quarantine_missing','quarantine_mismatch'
    )),
    reason_code text NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    actionability text NOT NULL CHECK (actionability IN ('repairable','manual','non_actionable')),
    subject_type text NOT NULL CHECK (subject_type IN (
        'original','rendition','original_attempt_temp','rendition_attempt_temp',
        'quarantine','media','job','job_target','path'
    )),
    subject_id nmcp_uuid_v4,
    media_id nmcp_uuid_v4,
    job_id nmcp_uuid_v4,
    job_target_id nmcp_uuid_v4,
    attempt_id nmcp_uuid_v4,
    manifest_item_id nmcp_uuid_v4,
    relative_key text CHECK (relative_key IS NULL OR nmcp_is_relative_path(relative_key)),
    expected_state text CHECK (expected_state IS NULL OR expected_state IN (
        'referenced','unreferenced','current','non_current','purged','cleanup_deleted','quarantined'
    )),
    expected_size_bytes bigint CHECK (expected_size_bytes IS NULL OR expected_size_bytes>=0),
    expected_sha256 text CHECK (expected_sha256 IS NULL OR nmcp_is_sha256(expected_sha256)),
    observed_type text NOT NULL CHECK (observed_type IN (
        'regular','missing','directory','symlink','fifo','socket','device','other','unknown'
    )),
    observed_size_bytes bigint CHECK (observed_size_bytes IS NULL OR observed_size_bytes>=0),
    observed_sha256 text CHECK (observed_sha256 IS NULL OR nmcp_is_sha256(observed_sha256)),
    observed_mtime timestamptz,
    observed_ctime timestamptz,
    observed_at timestamptz NOT NULL,
    UNIQUE(report_id,ordinal),
    UNIQUE(report_id,id),
    CHECK (relative_key IS NOT NULL OR subject_id IS NOT NULL OR media_id IS NOT NULL
        OR job_id IS NOT NULL OR job_target_id IS NOT NULL OR manifest_item_id IS NOT NULL),
    CHECK (observed_type='regular' OR observed_sha256 IS NULL),
    CHECK (actionability<>'repairable' OR (
        kind IN ('aged_attempt_temp','final_orphan') AND observed_type='regular'
        AND relative_key IS NOT NULL AND observed_size_bytes IS NOT NULL
        AND observed_sha256 IS NOT NULL AND observed_mtime IS NOT NULL AND observed_ctime IS NOT NULL
    ))
);
CREATE INDEX reconciliation_check_findings_page_idx ON reconciliation_check_findings(report_id,ordinal);
CREATE INDEX reconciliation_check_findings_kind_idx ON reconciliation_check_findings(report_id,kind,ordinal);

CREATE TABLE reconciliation_check_report_seals (
    report_id nmcp_uuid_v4 PRIMARY KEY REFERENCES reconciliation_check_reports(id) ON DELETE RESTRICT,
    finding_count bigint NOT NULL CHECK (finding_count>=0),
    fs_scan_ended_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (completed_at>=fs_scan_ended_at)
);
CREATE INDEX reconciliation_check_reports_completed_idx
ON reconciliation_check_report_seals(completed_at DESC,report_id DESC);

CREATE TABLE reconciliation_repair_runs (
    id nmcp_uuid_v4 PRIMARY KEY,
    source_report_id nmcp_uuid_v4 NOT NULL REFERENCES reconciliation_check_report_seals(report_id) ON DELETE RESTRICT,
    authorization_id nmcp_uuid_v4 NOT NULL UNIQUE,
    authorized_actor text NOT NULL CHECK (authorized_actor<>'' AND octet_length(authorized_actor)<=255),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(id,source_report_id)
);

CREATE TABLE reconciliation_repair_manifest_items (
    id nmcp_uuid_v4 PRIMARY KEY,
    run_id nmcp_uuid_v4 NOT NULL,
    source_report_id nmcp_uuid_v4 NOT NULL,
    finding_id nmcp_uuid_v4 NOT NULL,
    quarantine_id nmcp_uuid_v4 NOT NULL UNIQUE,
    source_type text NOT NULL CHECK (source_type IN (
        'original_final','rendition_final','original_attempt_temp','rendition_attempt_temp'
    )),
    source_relative_key text NOT NULL CHECK (nmcp_is_relative_path(source_relative_key)),
    expected_size_bytes bigint NOT NULL CHECK (expected_size_bytes>=0),
    report_observed_sha256 text NOT NULL CHECK (nmcp_is_sha256(report_observed_sha256)),
    destination_relative_key text NOT NULL CHECK (nmcp_is_relative_path(destination_relative_key)),
    planned_action text NOT NULL CHECK (planned_action='quarantine'),
    prepared_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(run_id,id),
    UNIQUE(run_id,finding_id),
    FOREIGN KEY(run_id,source_report_id) REFERENCES reconciliation_repair_runs(id,source_report_id) ON DELETE RESTRICT,
    FOREIGN KEY(source_report_id,finding_id) REFERENCES reconciliation_check_findings(report_id,id) ON DELETE RESTRICT,
    CHECK (destination_relative_key='.quarantine/'||quarantine_id::text)
);
CREATE INDEX reconciliation_repair_manifest_report_idx
ON reconciliation_repair_manifest_items(source_report_id,finding_id);

CREATE TABLE reconciliation_repair_attempts (
    id nmcp_uuid_v4 PRIMARY KEY,
    manifest_item_id nmcp_uuid_v4 NOT NULL REFERENCES reconciliation_repair_manifest_items(id) ON DELETE RESTRICT,
    attempt_number integer NOT NULL CHECK (attempt_number>0),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(manifest_item_id,attempt_number),
    UNIQUE(manifest_item_id,id)
);

CREATE TABLE reconciliation_repair_events (
    id nmcp_uuid_v4 PRIMARY KEY,
    attempt_id nmcp_uuid_v4 NOT NULL REFERENCES reconciliation_repair_attempts(id) ON DELETE RESTRICT,
    sequence integer NOT NULL CHECK (sequence>0),
    event_type text NOT NULL CHECK (event_type IN (
        'prepared','applying','revalidated','rename','source_directory_fsync',
        'destination_directory_fsync','completed'
    )),
    outcome_code text NOT NULL CHECK (outcome_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    error_code text CHECK (error_code IS NULL OR error_code IN (
        'permission','read_only','cross_device','no_space','quota','collision','wrong_type',
        'source_missing','source_changed','newly_referenced','live_owner','recent_owner',
        'unknown_owner','manifest_mismatch','quarantine_mismatch','durability',
        'outcome_uncertain','cancelled','internal'
    )),
    observed_size_bytes bigint CHECK (observed_size_bytes IS NULL OR observed_size_bytes>=0),
    observed_sha256 text CHECK (observed_sha256 IS NULL OR nmcp_is_sha256(observed_sha256)),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(attempt_id,sequence),
    UNIQUE(attempt_id,id)
);

CREATE TABLE reconciliation_repair_results (
    run_id nmcp_uuid_v4 PRIMARY KEY REFERENCES reconciliation_repair_runs(id) ON DELETE RESTRICT,
    result text NOT NULL CHECK (result IN ('succeeded','partial','failed','unknown')),
    manifest_count bigint NOT NULL CHECK (manifest_count>=0),
    quarantined_count bigint NOT NULL CHECK (quarantined_count>=0),
    stale_count bigint NOT NULL CHECK (stale_count>=0),
    resolved_count bigint NOT NULL CHECK (resolved_count>=0),
    failed_count bigint NOT NULL CHECK (failed_count>=0),
    unknown_count bigint NOT NULL CHECK (unknown_count>=0),
    completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (quarantined_count+stale_count+resolved_count+failed_count+unknown_count=manifest_count)
);

CREATE TABLE storage_attempts (
    id nmcp_uuid_v4 PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('upload','transform')),
    original_id nmcp_uuid_v4 NOT NULL,
    job_id nmcp_uuid_v4 REFERENCES jobs(id) ON DELETE SET NULL,
    temp_relative_key text,
    coverage text NOT NULL CHECK (coverage IN ('native','legacy_active')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind='upload' AND job_id IS NULL AND temp_relative_key IS NOT NULL
            AND temp_relative_key='originals/'||left(original_id::text,2)||'/'||original_id::text||'/.original.'||id::text||'.tmp')
        OR (kind='transform' AND job_id IS NOT NULL AND temp_relative_key IS NULL)),
    UNIQUE(job_id,id)
);

CREATE TABLE storage_attempt_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    attempt_id nmcp_uuid_v4 NOT NULL REFERENCES storage_attempts(id) ON DELETE RESTRICT,
    sequence integer NOT NULL CHECK (sequence>0),
    event_type text NOT NULL CHECK (event_type IN (
        'registered','claimed','heartbeat','published','aborted','released','expired','reclaimed'
    )),
    lease_expires_at timestamptz,
    job_status text CHECK (job_status IS NULL OR job_status IN ('queued','running','succeeded','failed')),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(attempt_id,sequence),
    CHECK ((event_type IN ('registered','claimed','heartbeat') AND lease_expires_at IS NOT NULL)
        OR (event_type NOT IN ('registered','claimed','heartbeat') AND lease_expires_at IS NULL))
);
CREATE INDEX storage_attempt_events_latest_idx ON storage_attempt_events(attempt_id,sequence DESC);
CREATE INDEX storage_attempt_events_lease_idx ON storage_attempt_events(lease_expires_at,attempt_id)
WHERE lease_expires_at IS NOT NULL;

DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'reconciliation_check_reports','reconciliation_check_findings','reconciliation_check_report_seals',
        'reconciliation_repair_runs','reconciliation_repair_manifest_items','reconciliation_repair_attempts',
        'reconciliation_repair_events','reconciliation_repair_results','storage_attempts','storage_attempt_events'
    ] LOOP
        EXECUTE format('CREATE TRIGGER %I_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation()',table_name,table_name);
        EXECUTE format('CREATE TRIGGER %I_no_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation()',table_name,table_name);
    END LOOP;
END;
$$;

CREATE FUNCTION nmcp_guard_check_finding_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM reconciliation_check_reports WHERE id=NEW.report_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'check report is absent' USING ERRCODE='23503'; END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_check_report_seals WHERE report_id=NEW.report_id) THEN
        RAISE EXCEPTION 'sealed check report is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER reconciliation_check_findings_insert_guard
BEFORE INSERT ON reconciliation_check_findings
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_check_finding_insert();

CREATE FUNCTION nmcp_guard_repair_event_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected_sequence integer; previous_type text;
BEGIN
    PERFORM 1 FROM reconciliation_repair_attempts WHERE id=NEW.attempt_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair attempt is absent' USING ERRCODE='23503'; END IF;
    SELECT COALESCE(max(sequence),0)+1 INTO expected_sequence
      FROM reconciliation_repair_events WHERE attempt_id=NEW.attempt_id;
    SELECT event_type INTO previous_type FROM reconciliation_repair_events
      WHERE attempt_id=NEW.attempt_id ORDER BY sequence DESC LIMIT 1;
    IF NEW.sequence<>expected_sequence THEN RAISE EXCEPTION 'repair event sequence is not contiguous' USING ERRCODE='23514'; END IF;
    IF NEW.sequence=1 AND NEW.event_type<>'prepared' THEN RAISE EXCEPTION 'repair attempt must begin prepared' USING ERRCODE='23514'; END IF;
    IF previous_type='completed' THEN RAISE EXCEPTION 'completed repair attempt is terminal' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER reconciliation_repair_events_insert_guard
BEFORE INSERT ON reconciliation_repair_events
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_repair_event_insert();

CREATE FUNCTION nmcp_guard_storage_attempt_event_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected_sequence integer; previous_type text;
BEGIN
    PERFORM 1 FROM storage_attempts WHERE id=NEW.attempt_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'storage attempt is absent' USING ERRCODE='23503'; END IF;
    SELECT COALESCE(max(sequence),0)+1 INTO expected_sequence FROM storage_attempt_events WHERE attempt_id=NEW.attempt_id;
    SELECT event_type INTO previous_type FROM storage_attempt_events WHERE attempt_id=NEW.attempt_id ORDER BY sequence DESC LIMIT 1;
    IF NEW.sequence<>expected_sequence THEN RAISE EXCEPTION 'storage attempt event sequence is not contiguous' USING ERRCODE='23514'; END IF;
    IF previous_type IN ('published','aborted','released','expired','reclaimed') THEN
        RAISE EXCEPTION 'storage attempt is terminal' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER storage_attempt_events_insert_guard
BEFORE INSERT ON storage_attempt_events
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_storage_attempt_event_insert();

CREATE FUNCTION nmcp_require_normal_maintenance()
RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    PERFORM 1 FROM maintenance_state WHERE id=1 AND mode='normal' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'maintenance mode is active' USING ERRCODE='55000'; END IF;
END;
$$;

CREATE FUNCTION nmcp_register_upload_attempt(upload_attempt_id nmcp_uuid_v4,upload_original_id nmcp_uuid_v4)
RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE expiry timestamptz:=clock_timestamp()+interval '2 minutes';
BEGIN
    PERFORM nmcp_require_normal_maintenance();
    INSERT INTO storage_attempts(id,kind,original_id,temp_relative_key,coverage)
      VALUES(upload_attempt_id,'upload',upload_original_id,
        'originals/'||left(upload_original_id::text,2)||'/'||upload_original_id::text||'/.original.'||upload_attempt_id::text||'.tmp','native');
    INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at)
      VALUES(upload_attempt_id,1,'registered',expiry);
    RETURN expiry;
END;
$$;

CREATE FUNCTION nmcp_heartbeat_upload_attempt(upload_attempt_id nmcp_uuid_v4)
RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE previous storage_attempt_events%ROWTYPE; next_sequence integer; expiry timestamptz:=clock_timestamp()+interval '2 minutes';
BEGIN
    PERFORM nmcp_require_normal_maintenance();
    PERFORM 1 FROM storage_attempts WHERE id=upload_attempt_id AND kind='upload' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'upload attempt is absent' USING ERRCODE='23514'; END IF;
    SELECT * INTO previous FROM storage_attempt_events WHERE attempt_id=upload_attempt_id ORDER BY sequence DESC LIMIT 1;
    -- A heartbeat racing a committed publication is harmless. Return the
    -- candidate expiry so callers that scan the declared timestamptz result do
    -- not turn that terminal race into an application error.
    IF previous.event_type='published' THEN RETURN expiry; END IF;
    IF previous.event_type NOT IN ('registered','heartbeat') OR previous.lease_expires_at<=clock_timestamp() THEN
        RAISE EXCEPTION 'upload attempt lease is not live' USING ERRCODE='55000';
    END IF;
    next_sequence:=previous.sequence+1;
    INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at)
      VALUES(upload_attempt_id,next_sequence,'heartbeat',expiry);
    RETURN expiry;
END;
$$;

CREATE FUNCTION nmcp_require_live_upload_attempt(upload_attempt_id nmcp_uuid_v4,upload_original_id nmcp_uuid_v4)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE previous storage_attempt_events%ROWTYPE;
BEGIN
    PERFORM 1 FROM storage_attempts
      WHERE id=upload_attempt_id AND kind='upload' AND original_id=upload_original_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'upload attempt is absent' USING ERRCODE='23514'; END IF;
    SELECT * INTO previous FROM storage_attempt_events WHERE attempt_id=upload_attempt_id ORDER BY sequence DESC LIMIT 1;
    IF previous.event_type NOT IN ('registered','heartbeat') OR previous.lease_expires_at<=clock_timestamp() THEN
        RAISE EXCEPTION 'upload attempt lease is not live' USING ERRCODE='55000';
    END IF;
END;
$$;

CREATE FUNCTION nmcp_complete_upload_attempt(upload_attempt_id nmcp_uuid_v4,terminal_event text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE previous storage_attempt_events%ROWTYPE;
BEGIN
    IF terminal_event NOT IN ('published','aborted') THEN RAISE EXCEPTION 'invalid upload terminal event' USING ERRCODE='23514'; END IF;
    PERFORM 1 FROM storage_attempts WHERE id=upload_attempt_id AND kind='upload' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'upload attempt is absent' USING ERRCODE='23514'; END IF;
    SELECT * INTO previous FROM storage_attempt_events WHERE attempt_id=upload_attempt_id ORDER BY sequence DESC LIMIT 1;
    IF previous.event_type=terminal_event THEN RETURN; END IF;
    IF previous.event_type IN ('published','aborted') THEN RAISE EXCEPTION 'upload attempt has opposite terminal result' USING ERRCODE='23514'; END IF;
    IF terminal_event='published' AND previous.lease_expires_at<=clock_timestamp() THEN
        RAISE EXCEPTION 'upload attempt lease expired before publication' USING ERRCODE='55000';
    END IF;
    INSERT INTO storage_attempt_events(attempt_id,sequence,event_type)
      VALUES(upload_attempt_id,previous.sequence+1,terminal_event);
END;
$$;

CREATE FUNCTION nmcp_record_transform_storage_attempt()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE next_sequence integer; terminal_event text;
BEGIN
    IF NEW.type<>'transform' THEN RETURN NEW; END IF;
    IF OLD.status='queued' AND NEW.status='running' AND NEW.lease_token IS NOT NULL THEN
        INSERT INTO storage_attempts(id,kind,original_id,job_id,coverage,created_at)
          VALUES(NEW.lease_token,'transform',NEW.original_id,NEW.id,'native',NEW.updated_at);
        INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at,job_status,occurred_at)
          VALUES(NEW.lease_token,1,'claimed',NEW.lease_expires_at,NEW.status,NEW.updated_at);
    ELSIF OLD.status='running' AND NEW.status='running' AND OLD.lease_token=NEW.lease_token
          AND OLD.lease_expires_at IS DISTINCT FROM NEW.lease_expires_at THEN
        SELECT COALESCE(max(sequence),0)+1 INTO next_sequence FROM storage_attempt_events WHERE attempt_id=NEW.lease_token;
        INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at,job_status,occurred_at)
          VALUES(NEW.lease_token,next_sequence,'heartbeat',NEW.lease_expires_at,NEW.status,NEW.updated_at);
    ELSIF OLD.status='running' AND NEW.status<>'running' AND OLD.lease_token IS NOT NULL THEN
        terminal_event:=CASE WHEN NEW.status='succeeded' THEN 'published'
          WHEN NEW.error_code='lease_expired' AND NEW.status='queued' THEN 'reclaimed'
          WHEN NEW.error_code='lease_expired' THEN 'expired'
          ELSE 'released' END;
        SELECT COALESCE(max(sequence),0)+1 INTO next_sequence FROM storage_attempt_events WHERE attempt_id=OLD.lease_token;
        INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,job_status,occurred_at)
          VALUES(OLD.lease_token,next_sequence,terminal_event,NEW.status,NEW.updated_at);
    END IF;
    RETURN NEW;
END;
$$;

INSERT INTO storage_attempts(id,kind,original_id,job_id,coverage,created_at)
SELECT lease_token,'transform',original_id,id,'legacy_active',COALESCE(started_at,updated_at)
FROM jobs WHERE type='transform' AND status='running';
INSERT INTO storage_attempt_events(attempt_id,sequence,event_type,lease_expires_at,job_status,occurred_at)
SELECT lease_token,1,'claimed',lease_expires_at,status,updated_at
FROM jobs WHERE type='transform' AND status='running';

CREATE TRIGGER jobs_transform_storage_attempt_history
AFTER UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION nmcp_record_transform_storage_attempt();

CREATE FUNCTION nmcp_begin_check_report(
    report_id nmcp_uuid_v4,report_scope text,report_classifier_version smallint,
    snapshot_started timestamptz,cutoff_at timestamptz,snapshot_ended timestamptz,scan_started timestamptz
) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE migration_row schema_migrations%ROWTYPE;
BEGIN
    SELECT * INTO STRICT migration_row FROM schema_migrations WHERE version=(SELECT max(version) FROM schema_migrations);
    INSERT INTO reconciliation_check_reports(
        id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
        db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at
    ) VALUES(report_id,1,report_classifier_version,report_scope,migration_row.version,migration_row.name,migration_row.checksum,
        snapshot_started,cutoff_at,snapshot_ended,scan_started,cutoff_at-interval '48 hours');
END;
$$;

CREATE FUNCTION nmcp_append_check_finding(
    finding_id nmcp_uuid_v4,finding_report_id nmcp_uuid_v4,finding_kind text,finding_reason text,
    finding_actionability text,finding_subject_type text,finding_subject_id nmcp_uuid_v4,
    finding_media_id nmcp_uuid_v4,finding_job_id nmcp_uuid_v4,finding_target_id nmcp_uuid_v4,
    finding_attempt_id nmcp_uuid_v4,finding_manifest_item_id nmcp_uuid_v4,finding_relative_key text,
    finding_expected_state text,finding_expected_size bigint,finding_expected_sha text,
    finding_observed_type text,finding_observed_size bigint,finding_observed_sha text,
    finding_observed_mtime timestamptz,finding_observed_ctime timestamptz,finding_observed_at timestamptz
) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE next_ordinal bigint;
BEGIN
    PERFORM 1 FROM reconciliation_check_reports WHERE id=finding_report_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'check report is absent' USING ERRCODE='23514'; END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_check_report_seals WHERE report_id=finding_report_id) THEN
        RAISE EXCEPTION 'sealed check report is immutable' USING ERRCODE='23514';
    END IF;
    SELECT COALESCE(max(ordinal),0)+1 INTO next_ordinal
      FROM reconciliation_check_findings WHERE report_id=finding_report_id;
    INSERT INTO reconciliation_check_findings(
        id,report_id,ordinal,kind,reason_code,actionability,subject_type,subject_id,media_id,job_id,
        job_target_id,attempt_id,manifest_item_id,relative_key,expected_state,expected_size_bytes,
        expected_sha256,observed_type,observed_size_bytes,observed_sha256,observed_mtime,observed_ctime,observed_at
    ) VALUES(
        finding_id,finding_report_id,next_ordinal,finding_kind,finding_reason,finding_actionability,
        finding_subject_type,finding_subject_id,finding_media_id,finding_job_id,finding_target_id,
        finding_attempt_id,finding_manifest_item_id,finding_relative_key,finding_expected_state,
        finding_expected_size,finding_expected_sha,finding_observed_type,finding_observed_size,
        finding_observed_sha,finding_observed_mtime,finding_observed_ctime,finding_observed_at
    );
    RETURN next_ordinal;
END;
$$;

CREATE FUNCTION nmcp_seal_check_report(sealed_report_id nmcp_uuid_v4,expected_findings bigint,scan_ended timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE actual_count bigint; first_ordinal bigint; last_ordinal bigint;
BEGIN
    PERFORM 1 FROM reconciliation_check_reports WHERE id=sealed_report_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'check report is absent' USING ERRCODE='23514'; END IF;
    SELECT count(*),min(ordinal),max(ordinal) INTO actual_count,first_ordinal,last_ordinal
      FROM reconciliation_check_findings WHERE report_id=sealed_report_id;
    IF actual_count<>expected_findings OR (actual_count>0 AND (first_ordinal<>1 OR last_ordinal<>actual_count)) THEN
        RAISE EXCEPTION 'check report findings are incomplete' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_check_report_seals(report_id,finding_count,fs_scan_ended_at)
      VALUES(sealed_report_id,actual_count,scan_ended);
END;
$$;

DO $$
DECLARE target_schema text:=current_schema(); function_signature text;
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
    FOREACH function_signature IN ARRAY ARRAY[
        'nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz)',
        'nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)',
        'nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_check_function_owner',target_schema,function_signature);
        EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    FOREACH function_signature IN ARRAY ARRAY[
        'nmcp_require_normal_maintenance()',
        'nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)',
        'nmcp_heartbeat_upload_attempt(nmcp_uuid_v4)',
        'nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)',
        'nmcp_complete_upload_attempt(nmcp_uuid_v4,text)',
        'nmcp_record_transform_storage_attempt()'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_repair_function_owner',target_schema,function_signature);
        EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    FOREACH function_signature IN ARRAY ARRAY[
        'nmcp_reject_legacy_reconciliation_insert()','nmcp_guard_check_finding_insert()',
        'nmcp_guard_repair_event_insert()','nmcp_guard_storage_attempt_event_insert()'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO nmcp_check_runtime,nmcp_repair_runtime',target_schema);
END;
$$;

REVOKE ALL ON reconciliation_reports FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
REVOKE ALL ON admin_audit,admin_batches FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
REVOKE ALL ON maintenance_state FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
REVOKE ALL ON reconciliation_check_reports,reconciliation_check_findings,reconciliation_check_report_seals,
    reconciliation_repair_runs,reconciliation_repair_manifest_items,reconciliation_repair_attempts,
    reconciliation_repair_events,reconciliation_repair_results,storage_attempts,storage_attempt_events
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;

GRANT SELECT ON schema_migrations TO nmcp_check_function_owner;
GRANT SELECT,INSERT ON reconciliation_check_reports,reconciliation_check_findings,reconciliation_check_report_seals TO nmcp_check_function_owner;
GRANT UPDATE(id) ON reconciliation_check_reports TO nmcp_check_function_owner;
GRANT SELECT ON system_config,media,originals,profiles,jobs,job_targets,renditions,purge_file_progress,
    rendition_cleanup_progress,media_purge_identity_guard,storage_attempts,storage_attempt_events
TO nmcp_check_function_owner;

GRANT SELECT ON maintenance_state,media,originals,jobs,job_targets,renditions,media_purge_identity_guard,
    purge_file_progress,rendition_cleanup_progress,reconciliation_check_reports,reconciliation_check_findings,
    reconciliation_check_report_seals,reconciliation_repair_runs,reconciliation_repair_manifest_items,
    reconciliation_repair_attempts,reconciliation_repair_events,reconciliation_repair_results,
    storage_attempts,storage_attempt_events TO nmcp_repair_function_owner;
GRANT INSERT ON reconciliation_repair_runs,reconciliation_repair_manifest_items,reconciliation_repair_attempts,
    reconciliation_repair_events,reconciliation_repair_results,storage_attempts,storage_attempt_events
TO nmcp_repair_function_owner;
GRANT UPDATE(id) ON maintenance_state,media,originals,jobs,job_targets,renditions,
    reconciliation_repair_runs,reconciliation_repair_manifest_items,reconciliation_repair_attempts,storage_attempts
TO nmcp_repair_function_owner;
GRANT UPDATE(media_id) ON media_purge_identity_guard TO nmcp_repair_function_owner;
GRANT USAGE,SELECT ON SEQUENCE storage_attempt_events_id_seq TO nmcp_repair_function_owner;

REVOKE ALL ON FUNCTION nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz),
    nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz),
    nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_begin_check_report(nmcp_uuid_v4,text,smallint,timestamptz,timestamptz,timestamptz,timestamptz),
    nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz),
    nmcp_seal_check_report(nmcp_uuid_v4,bigint,timestamptz) TO nmcp_check_runtime;

REVOKE ALL ON FUNCTION nmcp_require_normal_maintenance(),
    nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4),
    nmcp_heartbeat_upload_attempt(nmcp_uuid_v4),nmcp_complete_upload_attempt(nmcp_uuid_v4,text),
    nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4),
    nmcp_record_transform_storage_attempt()
FROM PUBLIC,nmcp_check_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_require_normal_maintenance(),
    nmcp_register_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4),
    nmcp_heartbeat_upload_attempt(nmcp_uuid_v4),nmcp_complete_upload_attempt(nmcp_uuid_v4,text)
    ,nmcp_require_live_upload_attempt(nmcp_uuid_v4,nmcp_uuid_v4)
TO nmcp_runtime;

REVOKE ALL ON FUNCTION nmcp_reject_legacy_reconciliation_insert(),nmcp_guard_check_finding_insert(),
    nmcp_guard_repair_event_insert(),nmcp_guard_storage_attempt_event_insert()
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
