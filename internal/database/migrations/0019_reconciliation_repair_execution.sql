-- Explicit repair plans are complete, replayable, and sealed before execution.
-- Filesystem effects remain correlated with the immutable v17 repair journal;
-- this migration adds no second journal or persistent applying state.

DO $$
DECLARE required_role text;
BEGIN
    FOREACH required_role IN ARRAY ARRAY[
        'nmcp_check_runtime','nmcp_repair_runtime',
        'nmcp_check_function_owner','nmcp_repair_function_owner'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname=required_role AND NOT rolcanlogin AND NOT rolinherit
        ) THEN
            RAISE EXCEPTION 'repair execution role topology is unsafe: %',required_role
                USING ERRCODE='55000';
        END IF;
    END LOOP;
    IF pg_catalog.pg_has_role('nmcp_check_runtime','nmcp_repair_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_repair_runtime','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_repair_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_check_runtime','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_repair_runtime','MEMBER') THEN
        RAISE EXCEPTION 'repair and check capabilities must remain isolated' USING ERRCODE='55000';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid='reconciliation_repair_manifest_items'::regclass
          AND conname='reconciliation_repair_manifest_items_source_relative_key_key'
          AND contype='u'
    ) THEN
        RAISE EXCEPTION 'repair source-path baseline constraint is absent' USING ERRCODE='55000';
    END IF;
END;
$$;

-- Drain checker/repair and entity writers before changing the authority graph.
LOCK TABLE maintenance_state,media,jobs,job_targets,originals,renditions,
    storage_attempts,storage_attempt_events,
    reconciliation_check_reports,reconciliation_check_source_results,
    reconciliation_check_findings,reconciliation_check_report_seals,
    reconciliation_repair_runs,reconciliation_repair_manifest_items,
    reconciliation_repair_attempts,reconciliation_repair_events,
    reconciliation_repair_results
IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM reconciliation_repair_runs AS r
        WHERE NOT EXISTS (
            SELECT 1 FROM reconciliation_repair_results AS result WHERE result.run_id=r.id
        )
    ) THEN
        RAISE EXCEPTION 'unfinished pre-v19 repair run requires operator resolution'
            USING ERRCODE='55000';
    END IF;
    IF EXISTS (
        SELECT 1 FROM reconciliation_repair_manifest_items
        GROUP BY run_id,source_relative_key HAVING count(*)>1
    ) THEN
        RAISE EXCEPTION 'pre-v19 repair run contains duplicate source paths'
            USING ERRCODE='55000';
    END IF;
END;
$$;

ALTER TABLE reconciliation_repair_manifest_items
    ADD COLUMN report_ordinal bigint CHECK (report_ordinal IS NULL OR report_ordinal>0);
ALTER TABLE reconciliation_repair_manifest_items
    ADD COLUMN owner_snapshot text;
ALTER TABLE reconciliation_repair_manifest_items
    DROP CONSTRAINT reconciliation_repair_manifest_items_source_relative_key_key;
CREATE UNIQUE INDEX reconciliation_repair_manifest_run_ordinal_idx
    ON reconciliation_repair_manifest_items(run_id,report_ordinal)
    WHERE report_ordinal IS NOT NULL;
CREATE UNIQUE INDEX reconciliation_repair_manifest_run_path_idx
    ON reconciliation_repair_manifest_items(run_id,source_relative_key);
CREATE INDEX reconciliation_repair_manifest_source_path_idx
    ON reconciliation_repair_manifest_items(source_relative_key,run_id);

CREATE TABLE reconciliation_repair_plan_seals (
    id nmcp_uuid_v4 PRIMARY KEY,
    run_id nmcp_uuid_v4 NOT NULL UNIQUE
        REFERENCES reconciliation_repair_runs(id) ON DELETE RESTRICT,
    source_report_id nmcp_uuid_v4 NOT NULL,
    report_format_version smallint NOT NULL CHECK (report_format_version=1),
    report_classifier_version smallint NOT NULL CHECK (report_classifier_version=1),
    report_migration_version bigint NOT NULL CHECK (report_migration_version IN (17,18,19)),
    report_migration_name text NOT NULL CHECK (report_migration_name<>''),
    report_migration_checksum text NOT NULL CHECK (nmcp_is_sha256(report_migration_checksum)),
    plan_version smallint NOT NULL CHECK (plan_version=1),
    item_count bigint NOT NULL CHECK (item_count>0),
    first_report_ordinal bigint NOT NULL CHECK (first_report_ordinal>0),
    last_report_ordinal bigint NOT NULL CHECK (last_report_ordinal>=first_report_ordinal),
    plan_digest text NOT NULL CHECK (nmcp_is_sha256(plan_digest)),
    sealed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY(run_id,source_report_id)
        REFERENCES reconciliation_repair_runs(id,source_report_id) ON DELETE RESTRICT
);
CREATE TRIGGER reconciliation_repair_plan_seals_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_repair_plan_seals
    FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER reconciliation_repair_plan_seals_no_truncate
    BEFORE TRUNCATE ON reconciliation_repair_plan_seals
    FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

ALTER TABLE reconciliation_repair_events
    DROP CONSTRAINT reconciliation_repair_events_event_type_check;
ALTER TABLE reconciliation_repair_events
    ADD CONSTRAINT reconciliation_repair_events_event_type_check CHECK (event_type IN (
        'prepared','applying','revalidated','rename','source_directory_fsync',
        'destination_directory_fsync','completed','failed','stale','resolved',
        'uncertain','unknown'
    ));
ALTER TABLE reconciliation_repair_events
    DROP CONSTRAINT reconciliation_repair_events_error_code_check;
ALTER TABLE reconciliation_repair_events
    ADD CONSTRAINT reconciliation_repair_events_error_code_check CHECK (
        error_code IS NULL OR error_code IN (
            'permission','read_only','cross_device','no_space','quota','collision','wrong_type',
            'source_missing','source_changed','newly_referenced','live_owner','recent_owner',
            'unknown_owner','manifest_mismatch','quarantine_mismatch','durability',
            'outcome_uncertain','cancelled','internal','current_reference',
            'owner_set_changed','owner_lookup_failed','source_restored','namespace_changed'
        )
    );
ALTER TABLE reconciliation_repair_events
    DROP CONSTRAINT reconciliation_repair_events_check;
ALTER TABLE reconciliation_repair_events
    ADD CONSTRAINT reconciliation_repair_events_check CHECK (
        (event_type IN ('failed','stale','resolved','uncertain','unknown') AND error_code IS NOT NULL)
        OR (event_type NOT IN ('failed','stale','resolved','uncertain','unknown') AND error_code IS NULL)
    );

-- Canonical plan-time owner evidence. The text form is deliberately composed
-- only from stable scalar fields and ordered UUIDs so session timezone and row
-- order cannot change the seal input.
CREATE FUNCTION nmcp_repair_owner_snapshot(
    requested_source_type text,requested_subject_id nmcp_uuid_v4,
    requested_job_id uuid,requested_attempt_id uuid
) RETURNS text LANGUAGE sql STABLE AS $$
  SELECT COALESCE(pg_catalog.string_agg(
    a.id::text||'|'||a.kind||'|'||a.coverage||'|'||
    COALESCE(latest.id::text,'')||'|'||COALESCE(latest.sequence::text,'')||'|'||
    COALESCE(latest.event_type,'')||'|'||
    COALESCE(pg_catalog.date_part('epoch',latest.lease_expires_at)::numeric::text,'')||'|'||
    COALESCE(pg_catalog.date_part('epoch',latest.occurred_at)::numeric::text,''),
    ',' ORDER BY a.id),'')
  FROM storage_attempts AS a
  LEFT JOIN LATERAL (
    SELECT e.id,e.sequence,e.event_type,e.lease_expires_at,e.occurred_at
    FROM storage_attempt_events AS e WHERE e.attempt_id=a.id
    ORDER BY e.sequence DESC LIMIT 1
  ) AS latest ON true
  WHERE (requested_source_type='original_final' AND a.kind='upload'
          AND a.original_id=requested_subject_id)
     OR (requested_source_type='rendition_final' AND a.kind='transform'
          AND a.job_id=requested_job_id)
     OR (requested_source_type IN ('original_attempt_temp','rendition_attempt_temp')
          AND a.id=requested_attempt_id)
$$;

CREATE FUNCTION nmcp_read_repairable_findings(
    requested_report_id nmcp_uuid_v4,after_ordinal bigint,page_limit integer
) RETURNS TABLE(
    ordinal bigint,finding_id uuid,source_type text,subject_id uuid,media_id uuid,
    job_id uuid,job_target_id uuid,storage_attempt_id uuid,source_relative_key text,
    expected_size_bytes bigint,report_observed_sha256 text,
    observed_mtime timestamptz,observed_ctime timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF after_ordinal<0 OR page_limit<1 OR page_limit>1000 THEN
        RAISE EXCEPTION 'invalid repair finding page' USING ERRCODE='22023';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM reconciliation_check_reports AS report
        JOIN reconciliation_check_report_seals AS seal ON seal.report_id=report.id
        WHERE report.id=requested_report_id
          AND report.format_version=1 AND report.classifier_version=1
          AND report.migration_version IN (17,18,19)
          AND EXISTS (SELECT 1 FROM schema_migrations AS migration
               WHERE migration.version=report.migration_version
                 AND migration.name=report.migration_name
                 AND migration.checksum=report.migration_checksum
                 AND NOT migration.dirty)
          AND (SELECT count(*) FROM reconciliation_check_source_results AS source
               WHERE source.report_id=report.id AND source.result='complete')=3
          AND NOT EXISTS (SELECT 1 FROM reconciliation_check_source_results AS source
               WHERE source.report_id=report.id AND source.result='error')
    ) THEN
        RAISE EXCEPTION 'repair requires a sealed error-free supported report' USING ERRCODE='23514';
    END IF;
    RETURN QUERY
    SELECT f.ordinal,f.id::uuid,
      CASE f.subject_type WHEN 'original' THEN 'original_final'
        WHEN 'rendition' THEN 'rendition_final'
        WHEN 'original_attempt_temp' THEN 'original_attempt_temp'
        WHEN 'rendition_attempt_temp' THEN 'rendition_attempt_temp' END,
      f.subject_id::uuid,f.media_id::uuid,f.job_id::uuid,f.job_target_id::uuid,
      f.attempt_id::uuid,f.relative_key,f.observed_size_bytes,f.observed_sha256,
      f.observed_mtime,f.observed_ctime
    FROM reconciliation_check_findings AS f
    WHERE f.report_id=requested_report_id AND f.actionability='repairable'
      AND f.ordinal>after_ordinal
      AND f.subject_type IN ('original','rendition','original_attempt_temp','rendition_attempt_temp')
    ORDER BY f.ordinal LIMIT page_limit;
END;
$$;

CREATE FUNCTION nmcp_begin_repair_run_v2(
    requested_run_id nmcp_uuid_v4,requested_report_id nmcp_uuid_v4,
    requested_authorization_id nmcp_uuid_v4,requested_actor text
) RETURNS TABLE(
    run_id uuid,source_report_id uuid,authorization_id uuid,
    authorized_actor text,created boolean
) LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE existing reconciliation_repair_runs%ROWTYPE;
BEGIN
    IF requested_actor='' OR octet_length(requested_actor)>255 THEN
        RAISE EXCEPTION 'invalid repair actor' USING ERRCODE='22023';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(requested_run_id::text,0));
    SELECT * INTO existing FROM reconciliation_repair_runs WHERE id=requested_run_id FOR UPDATE;
    IF FOUND THEN
        IF existing.source_report_id=requested_report_id
           AND existing.authorization_id=requested_authorization_id
           AND existing.authorized_actor=requested_actor THEN
            RETURN QUERY SELECT existing.id::uuid,existing.source_report_id::uuid,
              existing.authorization_id::uuid,existing.authorized_actor,false;
            RETURN;
        END IF;
        RAISE EXCEPTION 'repair run replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_repair_runs AS conflicting
      WHERE conflicting.authorization_id=requested_authorization_id) THEN
        RAISE EXCEPTION 'repair authorization replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    PERFORM * FROM nmcp_read_repairable_findings(requested_report_id,0,1);
    IF NOT FOUND THEN
        RAISE EXCEPTION 'repair report has no repairable findings' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_repair_runs(id,source_report_id,authorization_id,authorized_actor)
      VALUES(requested_run_id,requested_report_id,requested_authorization_id,requested_actor);
    RETURN QUERY SELECT requested_run_id::uuid,requested_report_id::uuid,
      requested_authorization_id::uuid,requested_actor,true;
END;
$$;

CREATE FUNCTION nmcp_prepare_repair_manifest_item_v2(
    requested_item_id nmcp_uuid_v4,requested_run_id nmcp_uuid_v4,
    requested_finding_id nmcp_uuid_v4,requested_quarantine_id nmcp_uuid_v4
) RETURNS TABLE(
    item_id uuid,report_ordinal bigint,destination_relative_key text,created boolean
) LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE run_row reconciliation_repair_runs%ROWTYPE;
        finding reconciliation_check_findings%ROWTYPE;
        existing reconciliation_repair_manifest_items%ROWTYPE;
        derived_source_type text; derived_owner_snapshot text;
BEGIN
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(requested_run_id::text,0));
    SELECT * INTO STRICT run_row FROM reconciliation_repair_runs WHERE id=requested_run_id FOR UPDATE;
    IF EXISTS (SELECT 1 FROM reconciliation_repair_plan_seals WHERE run_id=requested_run_id)
       OR EXISTS (SELECT 1 FROM reconciliation_repair_results WHERE run_id=requested_run_id) THEN
        RAISE EXCEPTION 'sealed repair plan is immutable' USING ERRCODE='23514';
    END IF;
    SELECT * INTO existing FROM reconciliation_repair_manifest_items WHERE id=requested_item_id;
    IF FOUND THEN
        IF existing.run_id=requested_run_id AND existing.finding_id=requested_finding_id
           AND existing.quarantine_id=requested_quarantine_id THEN
            RETURN QUERY SELECT existing.id::uuid,existing.report_ordinal,
              existing.destination_relative_key,false;
            RETURN;
        END IF;
        RAISE EXCEPTION 'repair manifest replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    SELECT * INTO STRICT finding FROM reconciliation_check_findings
      WHERE report_id=run_row.source_report_id AND id=requested_finding_id;
    IF finding.actionability<>'repairable' THEN
        RAISE EXCEPTION 'finding is not repairable' USING ERRCODE='23514';
    END IF;
    derived_source_type:=CASE finding.subject_type
      WHEN 'original' THEN 'original_final'
      WHEN 'rendition' THEN 'rendition_final'
      WHEN 'original_attempt_temp' THEN 'original_attempt_temp'
      WHEN 'rendition_attempt_temp' THEN 'rendition_attempt_temp' END;
    IF derived_source_type IS NULL THEN
        RAISE EXCEPTION 'repair source type is unsupported' USING ERRCODE='23514';
    END IF;
    derived_owner_snapshot:=nmcp_repair_owner_snapshot(
      derived_source_type,finding.subject_id,finding.job_id,finding.attempt_id);
    INSERT INTO reconciliation_repair_manifest_items(
      id,run_id,source_report_id,finding_id,quarantine_id,media_id,job_id,job_target_id,
      storage_attempt_id,source_type,source_relative_key,expected_size_bytes,
      report_observed_sha256,destination_relative_key,planned_action,report_ordinal,
      owner_snapshot
    ) VALUES(
      requested_item_id,requested_run_id,run_row.source_report_id,requested_finding_id,
      requested_quarantine_id,finding.media_id,finding.job_id,finding.job_target_id,
      finding.attempt_id,derived_source_type,finding.relative_key,finding.observed_size_bytes,
      finding.observed_sha256,'.quarantine/'||requested_quarantine_id::text,
      'quarantine',finding.ordinal,derived_owner_snapshot
    );
    RETURN QUERY SELECT requested_item_id::uuid,finding.ordinal,
      ('.quarantine/'||requested_quarantine_id::text),true;
END;
$$;

CREATE FUNCTION nmcp_read_repair_plan_digest(requested_run_id nmcp_uuid_v4)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE canonical jsonb;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM reconciliation_repair_runs WHERE id=requested_run_id) THEN
        RAISE EXCEPTION 'repair run is absent' USING ERRCODE='23514';
    END IF;
    SELECT pg_catalog.jsonb_build_object(
      'version',1,
      'report_id',run.source_report_id::text,
      'items',COALESCE(pg_catalog.jsonb_agg(
        pg_catalog.jsonb_build_array(
          item.report_ordinal,item.finding_id::text,item.id::text,item.quarantine_id::text,
          item.source_type,item.source_relative_key,item.expected_size_bytes,
          item.report_observed_sha256,
          (extract(epoch FROM finding.observed_mtime)*1000000)::bigint,
          (extract(epoch FROM finding.observed_ctime)*1000000)::bigint,
          item.owner_snapshot,item.destination_relative_key
        ) ORDER BY item.report_ordinal),'[]'::jsonb)
    ) INTO STRICT canonical
    FROM reconciliation_repair_runs AS run
    LEFT JOIN reconciliation_repair_manifest_items AS item ON item.run_id=run.id
    LEFT JOIN reconciliation_check_findings AS finding
      ON finding.report_id=item.source_report_id AND finding.id=item.finding_id
    WHERE run.id=requested_run_id
    GROUP BY run.source_report_id;
    RETURN pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(canonical::text,'UTF8')),'hex');
END;
$$;

CREATE FUNCTION nmcp_seal_repair_plan(
    requested_plan_id nmcp_uuid_v4,requested_run_id nmcp_uuid_v4,
    requested_plan_version smallint,requested_item_count bigint,requested_plan_digest text
) RETURNS TABLE(plan_id uuid,item_count bigint,plan_digest text,created boolean)
LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE run_row reconciliation_repair_runs%ROWTYPE;
        report_row reconciliation_check_reports%ROWTYPE;
        existing reconciliation_repair_plan_seals%ROWTYPE;
        actual_count bigint; repairable_count bigint; first_ordinal bigint; last_ordinal bigint;
        canonical_digest text;
BEGIN
    IF requested_plan_version<>1 OR requested_item_count<1 OR NOT nmcp_is_sha256(requested_plan_digest) THEN
        RAISE EXCEPTION 'invalid repair plan seal' USING ERRCODE='22023';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(requested_run_id::text,0));
    SELECT * INTO STRICT run_row FROM reconciliation_repair_runs WHERE id=requested_run_id FOR UPDATE;
    SELECT * INTO STRICT report_row FROM reconciliation_check_reports
      WHERE id=run_row.source_report_id;
    SELECT * INTO existing FROM reconciliation_repair_plan_seals WHERE run_id=requested_run_id;
    IF FOUND THEN
        IF existing.id=requested_plan_id AND existing.plan_version=requested_plan_version
           AND existing.item_count=requested_item_count
           AND existing.plan_digest=requested_plan_digest THEN
            RETURN QUERY SELECT existing.id::uuid,existing.item_count,existing.plan_digest,false;
            RETURN;
        END IF;
        RAISE EXCEPTION 'repair plan seal replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    PERFORM * FROM nmcp_read_repairable_findings(run_row.source_report_id,0,1);
    SELECT count(*),min(report_ordinal),max(report_ordinal)
      INTO actual_count,first_ordinal,last_ordinal
      FROM reconciliation_repair_manifest_items WHERE run_id=requested_run_id;
    SELECT count(*) INTO repairable_count FROM reconciliation_check_findings
      WHERE report_id=run_row.source_report_id AND actionability='repairable';
    IF actual_count<>requested_item_count OR actual_count<>repairable_count
       OR EXISTS (
         SELECT 1 FROM reconciliation_check_findings AS f
         LEFT JOIN reconciliation_repair_manifest_items AS item
           ON item.run_id=requested_run_id AND item.finding_id=f.id
         WHERE f.report_id=run_row.source_report_id AND f.actionability='repairable'
           AND (item.id IS NULL OR item.report_ordinal IS DISTINCT FROM f.ordinal
              OR item.source_relative_key IS DISTINCT FROM f.relative_key
              OR item.expected_size_bytes IS DISTINCT FROM f.observed_size_bytes
              OR item.report_observed_sha256 IS DISTINCT FROM f.observed_sha256
              OR item.owner_snapshot IS NULL)
       ) THEN
        RAISE EXCEPTION 'repair plan is not the complete deterministic finding set' USING ERRCODE='23514';
    END IF;
    canonical_digest:=nmcp_read_repair_plan_digest(requested_run_id);
    IF requested_plan_digest IS DISTINCT FROM canonical_digest THEN
        RAISE EXCEPTION 'repair plan digest is not canonical' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_repair_plan_seals(
      id,run_id,source_report_id,report_format_version,report_classifier_version,
      report_migration_version,report_migration_name,report_migration_checksum,
      plan_version,item_count,first_report_ordinal,last_report_ordinal,plan_digest
    ) VALUES(
      requested_plan_id,requested_run_id,run_row.source_report_id,report_row.format_version,
      report_row.classifier_version,report_row.migration_version,report_row.migration_name,
      report_row.migration_checksum,requested_plan_version,requested_item_count,
      first_ordinal,last_ordinal,requested_plan_digest
    );
    RETURN QUERY SELECT requested_plan_id::uuid,requested_item_count,requested_plan_digest,true;
END;
$$;

CREATE FUNCTION nmcp_repair_authority_rows(
    requested_report_id nmcp_uuid_v4,requested_finding_id nmcp_uuid_v4,
    requested_destination text,requested_owner_snapshot text
) RETURNS TABLE(
    decision text,reason_code text,source_type text,subject_id uuid,media_id uuid,
    job_id uuid,job_target_id uuid,storage_attempt_id uuid,source_relative_key text,
    expected_size_bytes bigint,expected_sha256 text,destination_relative_key text,
    owner_attempt_id uuid,owner_kind text,owner_coverage text,owner_event_type text,
    owner_lease_expires_at timestamptz,owner_event_occurred_at timestamptz
) LANGUAGE plpgsql AS $$
DECLARE finding reconciliation_check_findings%ROWTYPE;
        report_row reconciliation_check_reports%ROWTYPE;
        derived_source_type text; derived_decision text:='apply'; derived_reason text;
        current_owner_snapshot text;
BEGIN
    SELECT * INTO STRICT report_row FROM reconciliation_check_reports
      WHERE id=requested_report_id;
    SELECT * INTO STRICT finding FROM reconciliation_check_findings
      WHERE report_id=requested_report_id AND id=requested_finding_id;
    IF finding.actionability<>'repairable' THEN
        RAISE EXCEPTION 'finding is not repairable' USING ERRCODE='23514';
    END IF;
    derived_source_type:=CASE finding.subject_type
      WHEN 'original' THEN 'original_final'
      WHEN 'rendition' THEN 'rendition_final'
      WHEN 'original_attempt_temp' THEN 'original_attempt_temp'
      WHEN 'rendition_attempt_temp' THEN 'rendition_attempt_temp' END;
    IF derived_source_type IS NULL OR requested_destination !~ '^\.quarantine/[0-9a-f-]{36}$' THEN
        RAISE EXCEPTION 'invalid repair authority request' USING ERRCODE='23514';
    END IF;

    -- Canonical entity lock order: Media, Jobs ordered by UUID, Targets/entities,
    -- then storage attempts ordered by UUID. Missing rows remain a validation
    -- result; lookup errors propagate and are never interpreted as absence.
    IF finding.media_id IS NOT NULL THEN
        PERFORM 1 FROM media WHERE id=finding.media_id FOR UPDATE;
    END IF;
    IF finding.job_id IS NOT NULL THEN
        PERFORM 1 FROM jobs AS locked_job WHERE locked_job.id=finding.job_id ORDER BY locked_job.id FOR UPDATE;
    END IF;
    IF finding.job_target_id IS NOT NULL THEN
        PERFORM 1 FROM job_targets WHERE id=finding.job_target_id ORDER BY id FOR UPDATE;
    END IF;
    IF derived_source_type='original_final' THEN
        PERFORM 1 FROM storage_attempts
          WHERE kind='upload' AND original_id=finding.subject_id
          ORDER BY id FOR UPDATE;
    ELSIF derived_source_type='rendition_final' THEN
        PERFORM 1 FROM storage_attempts
          WHERE storage_attempts.kind='transform' AND storage_attempts.job_id=finding.job_id
          ORDER BY storage_attempts.id FOR UPDATE;
    ELSE
        PERFORM 1 FROM storage_attempts WHERE id=finding.attempt_id ORDER BY id FOR UPDATE;
    END IF;

    current_owner_snapshot:=nmcp_repair_owner_snapshot(
      derived_source_type,finding.subject_id,finding.job_id,finding.attempt_id);
    IF requested_owner_snapshot IS NOT NULL
       AND current_owner_snapshot IS DISTINCT FROM requested_owner_snapshot THEN
        derived_decision:='stale'; derived_reason:='owner_set_changed';
    ELSIF (derived_source_type='original_final' AND EXISTS (SELECT 1 FROM originals
          WHERE relative_path=finding.relative_key OR id=finding.subject_id))
       OR (derived_source_type='rendition_final' AND EXISTS (SELECT 1 FROM renditions
          WHERE relative_path=finding.relative_key OR id=finding.subject_id)) THEN
        derived_decision:='stale'; derived_reason:='newly_referenced';
    ELSIF derived_source_type='rendition_final' AND (
        NOT EXISTS (SELECT 1 FROM jobs AS j JOIN job_targets AS t ON t.job_id=j.id
          WHERE j.id=finding.job_id AND t.id=finding.job_target_id
            AND j.media_id_snapshot=finding.media_id)
        OR EXISTS (SELECT 1 FROM jobs AS current_job WHERE current_job.id=finding.job_id
          AND current_job.status IN ('queued','running'))
    ) THEN
        derived_decision:='stale'; derived_reason:='live_owner';
    ELSIF derived_source_type IN ('original_attempt_temp','rendition_attempt_temp') AND NOT EXISTS (
        SELECT 1 FROM storage_attempts AS a
        JOIN LATERAL (
          SELECT e.event_type,e.occurred_at FROM storage_attempt_events AS e
          WHERE e.attempt_id=a.id ORDER BY e.sequence DESC LIMIT 1
        ) AS latest ON true
        WHERE a.id=finding.attempt_id AND a.coverage='native'
          AND latest.event_type IN ('aborted','released','expired')
          AND latest.occurred_at<=report_row.temp_cutoff_at
          AND ((derived_source_type='original_attempt_temp' AND a.kind='upload'
                AND a.temp_relative_key=finding.relative_key)
            OR (derived_source_type='rendition_attempt_temp' AND a.kind='transform'
                AND a.job_id=finding.job_id))
    ) THEN
        derived_decision:='stale'; derived_reason:='live_owner';
    ELSIF derived_source_type='original_final' AND EXISTS (
        SELECT 1 FROM storage_attempts AS a
        LEFT JOIN LATERAL (
          SELECT e.event_type,e.lease_expires_at,e.occurred_at
          FROM storage_attempt_events AS e WHERE e.attempt_id=a.id
          ORDER BY e.sequence DESC LIMIT 1
        ) AS latest ON true
        WHERE a.kind='upload' AND a.original_id=finding.subject_id
          AND (a.coverage<>'native' OR latest.event_type IS NULL
            OR latest.event_type NOT IN ('published','aborted'))
    ) THEN
        derived_decision:='stale'; derived_reason:='live_owner';
    ELSIF derived_source_type='rendition_final' AND EXISTS (
        SELECT 1 FROM storage_attempts AS a
        LEFT JOIN LATERAL (
          SELECT e.event_type,e.lease_expires_at,e.occurred_at
          FROM storage_attempt_events AS e WHERE e.attempt_id=a.id
          ORDER BY e.sequence DESC LIMIT 1
        ) AS latest ON true
        WHERE a.kind='transform' AND a.job_id=finding.job_id
          AND (a.coverage<>'native' OR latest.event_type IS NULL
            OR latest.event_type NOT IN ('published','released','expired'))
    ) THEN
        derived_decision:='stale'; derived_reason:='live_owner';
    END IF;

    RETURN QUERY
    WITH owners AS (
      SELECT a.id::uuid,a.kind,a.coverage,latest.event_type,
             latest.lease_expires_at,latest.occurred_at
      FROM storage_attempts AS a
      LEFT JOIN LATERAL (
        SELECT e.event_type,e.lease_expires_at,e.occurred_at
        FROM storage_attempt_events AS e WHERE e.attempt_id=a.id
        ORDER BY e.sequence DESC LIMIT 1
      ) AS latest ON true
      WHERE (derived_source_type='original_final' AND a.kind='upload' AND a.original_id=finding.subject_id)
         OR (derived_source_type='rendition_final' AND a.kind='transform' AND a.job_id=finding.job_id)
         OR (derived_source_type IN ('original_attempt_temp','rendition_attempt_temp') AND a.id=finding.attempt_id)
    )
    SELECT derived_decision,derived_reason,derived_source_type,finding.subject_id::uuid,
      finding.media_id::uuid,finding.job_id::uuid,finding.job_target_id::uuid,
      finding.attempt_id::uuid,finding.relative_key,finding.observed_size_bytes,
      finding.observed_sha256,requested_destination,
      owner.id,owner.kind,owner.coverage,owner.event_type,
      owner.lease_expires_at,owner.occurred_at
    FROM (SELECT 1) AS one
    LEFT JOIN owners AS owner ON true
    ORDER BY owner.id NULLS FIRST;
END;
$$;

-- Arguments are report ID, finding ID, and an unpersisted preview quarantine
-- UUID. The returned destination is illustrative only: dry-run reserves no ID,
-- inserts no row/event, and changes no sequence.
CREATE FUNCTION nmcp_preflight_repair_finding(
    requested_report_id nmcp_uuid_v4,requested_finding_id nmcp_uuid_v4,
    preview_quarantine_id nmcp_uuid_v4
) RETURNS TABLE(
    decision text,reason_code text,source_type text,subject_id uuid,media_id uuid,
    job_id uuid,job_target_id uuid,storage_attempt_id uuid,source_relative_key text,
    expected_size_bytes bigint,expected_sha256 text,destination_relative_key text,
    owner_attempt_id uuid,owner_kind text,owner_coverage text,owner_event_type text,
    owner_lease_expires_at timestamptz,owner_event_occurred_at timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE source_path text; destination text:='.quarantine/'||preview_quarantine_id::text;
BEGIN
    PERFORM * FROM nmcp_read_repairable_findings(requested_report_id,0,1000)
      WHERE finding_id=requested_finding_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair finding is not admitted' USING ERRCODE='23514'; END IF;
    SELECT relative_key INTO STRICT source_path FROM reconciliation_check_findings
      WHERE report_id=requested_report_id AND id=requested_finding_id;
    -- Dry-run follows the same global order except that it has no run advisory:
    -- maintenance, cross-run source path, entities/owners.
    PERFORM 1 FROM maintenance_state WHERE id=1 AND mode='maintenance' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair preflight requires maintenance mode' USING ERRCODE='55000'; END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('nmcp-repair-path-v1:'||source_path,0));
    PERFORM * FROM nmcp_repair_authority_rows(requested_report_id,requested_finding_id,destination,NULL);
    RETURN QUERY SELECT * FROM nmcp_repair_authority_rows(requested_report_id,requested_finding_id,destination,NULL);
END;
$$;

-- Arguments are the persisted run, manifest item, and active attempt UUIDs.
-- The destination is always the persisted plan quarantine UUID, never caller
-- input. The plan seal is mandatory before authority can be acquired.
CREATE FUNCTION nmcp_lock_repair_item_apply(
    requested_run_id nmcp_uuid_v4,requested_item_id nmcp_uuid_v4,
    requested_attempt_id nmcp_uuid_v4
) RETURNS TABLE(
    decision text,reason_code text,source_type text,subject_id uuid,media_id uuid,
    job_id uuid,job_target_id uuid,storage_attempt_id uuid,source_relative_key text,
    expected_size_bytes bigint,expected_sha256 text,destination_relative_key text,
    owner_attempt_id uuid,owner_kind text,owner_coverage text,owner_event_type text,
    owner_lease_expires_at timestamptz,owner_event_occurred_at timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE item reconciliation_repair_manifest_items%ROWTYPE;
        finding reconciliation_check_findings%ROWTYPE;
        terminal_outcome text;
BEGIN
    SELECT * INTO STRICT item FROM reconciliation_repair_manifest_items
      WHERE id=requested_item_id AND run_id=requested_run_id;
    IF NOT EXISTS (SELECT 1 FROM reconciliation_repair_plan_seals WHERE run_id=requested_run_id)
       OR NOT EXISTS (SELECT 1 FROM reconciliation_repair_attempts
          WHERE id=requested_attempt_id AND manifest_item_id=requested_item_id) THEN
        RAISE EXCEPTION 'repair apply requires an exact sealed plan attempt' USING ERRCODE='23514';
    END IF;
    -- Adopted global order: run advisory, maintenance, source path, entities,
    -- then journal rows. Never move the path lock before maintenance.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(requested_run_id::text,0));
    terminal_outcome:=nmcp_repair_effective_outcome(requested_item_id);
    IF terminal_outcome IS NOT NULL THEN
        SELECT * INTO STRICT finding FROM reconciliation_check_findings
          WHERE id=item.finding_id AND report_id=item.source_report_id;
        RETURN QUERY SELECT 'terminal'::text,terminal_outcome,item.source_type,
          finding.subject_id::uuid,item.media_id::uuid,item.job_id::uuid,
          item.job_target_id::uuid,item.storage_attempt_id::uuid,item.source_relative_key,
          item.expected_size_bytes,item.report_observed_sha256,item.destination_relative_key,
          NULL::uuid,NULL::text,NULL::text,NULL::text,NULL::timestamptz,NULL::timestamptz;
        RETURN;
    END IF;
    PERFORM 1 FROM maintenance_state WHERE id=1 AND mode='maintenance' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair apply requires maintenance mode' USING ERRCODE='55000'; END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('nmcp-repair-path-v1:'||item.source_relative_key,0));
    PERFORM * FROM nmcp_repair_authority_rows(item.source_report_id,item.finding_id,item.destination_relative_key,item.owner_snapshot);
    PERFORM 1 FROM reconciliation_repair_runs WHERE id=requested_run_id FOR UPDATE;
    PERFORM 1 FROM reconciliation_repair_plan_seals WHERE run_id=requested_run_id;
    PERFORM 1 FROM reconciliation_repair_manifest_items WHERE id=requested_item_id FOR UPDATE;
    PERFORM 1 FROM reconciliation_repair_attempts WHERE id=requested_attempt_id FOR UPDATE;
    RETURN QUERY SELECT * FROM nmcp_repair_authority_rows(
      item.source_report_id,item.finding_id,item.destination_relative_key,item.owner_snapshot);
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_begin_repair_attempt(
    repair_attempt_id nmcp_uuid_v4,repair_item_id nmcp_uuid_v4,
    prepared_event_id nmcp_uuid_v4
) RETURNS integer LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE item reconciliation_repair_manifest_items%ROWTYPE;
        existing_attempt reconciliation_repair_attempts%ROWTYPE;
        existing_event reconciliation_repair_events%ROWTYPE;
        next_attempt integer; latest_type text;
BEGIN
    SELECT * INTO STRICT item FROM reconciliation_repair_manifest_items WHERE id=repair_item_id;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(item.run_id::text,0));
    PERFORM 1 FROM maintenance_state WHERE id=1 AND mode='maintenance' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair attempt requires maintenance mode' USING ERRCODE='55000'; END IF;
    PERFORM 1 FROM reconciliation_repair_runs WHERE id=item.run_id FOR UPDATE;
    PERFORM 1 FROM reconciliation_repair_plan_seals WHERE run_id=item.run_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair attempt requires a sealed plan' USING ERRCODE='23514'; END IF;
    PERFORM 1 FROM reconciliation_repair_manifest_items WHERE id=repair_item_id FOR UPDATE;
    SELECT * INTO existing_attempt FROM reconciliation_repair_attempts WHERE id=repair_attempt_id;
    IF FOUND THEN
        SELECT * INTO existing_event FROM reconciliation_repair_events
          WHERE attempt_id=repair_attempt_id AND sequence=1;
        IF existing_attempt.manifest_item_id=repair_item_id
           AND existing_event.id=prepared_event_id
           AND existing_event.event_type='prepared' THEN
            RETURN existing_attempt.attempt_number;
        END IF;
        RAISE EXCEPTION 'repair attempt replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_repair_events WHERE id=prepared_event_id) THEN
        RAISE EXCEPTION 'prepared event replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM reconciliation_repair_attempts AS a
        JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
        WHERE a.manifest_item_id=repair_item_id
          AND e.event_type IN ('rename','source_directory_fsync','destination_directory_fsync',
                               'completed','uncertain','unknown','stale','resolved')
    ) THEN
        RAISE EXCEPTION 'repair item has terminal or physical-effect-possible history' USING ERRCODE='23514';
    END IF;
    SELECT latest.event_type INTO latest_type
      FROM reconciliation_repair_attempts AS a
      JOIN LATERAL (SELECT e.event_type FROM reconciliation_repair_events AS e
        WHERE e.attempt_id=a.id ORDER BY e.sequence DESC LIMIT 1) AS latest ON true
      WHERE a.manifest_item_id=repair_item_id
      ORDER BY a.attempt_number DESC LIMIT 1;
    IF latest_type IS NOT NULL AND latest_type<>'failed' THEN
        RAISE EXCEPTION 'previous repair attempt is still active' USING ERRCODE='23514';
    END IF;
    SELECT COALESCE(max(attempt_number),0)+1 INTO next_attempt
      FROM reconciliation_repair_attempts WHERE manifest_item_id=repair_item_id;
    INSERT INTO reconciliation_repair_attempts(id,manifest_item_id,attempt_number)
      VALUES(repair_attempt_id,repair_item_id,next_attempt);
    INSERT INTO reconciliation_repair_events(id,attempt_id,sequence,event_type,outcome_code)
      VALUES(prepared_event_id,repair_attempt_id,1,'prepared','prepared');
    RETURN next_attempt;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_repair_attempt_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- The SECURITY DEFINER wrapper owns ordering, replay and sealing checks.
    -- This trigger remains fail closed against direct owner writes.
    IF current_user<>'nmcp_repair_function_owner' THEN
        RAISE EXCEPTION 'repair attempts require the repair API' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_repair_event_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE item reconciliation_repair_manifest_items%ROWTYPE;
        expected_sequence integer; previous_type text; last_applying_sequence integer;
        applying_transaction xid8;
BEGIN
    SELECT m.* INTO STRICT item FROM reconciliation_repair_attempts AS a
      JOIN reconciliation_repair_manifest_items AS m ON m.id=a.manifest_item_id
      WHERE a.id=NEW.attempt_id;
    IF NEW.transaction_id IS DISTINCT FROM pg_catalog.pg_current_xact_id() THEN
        RAISE EXCEPTION 'repair event transaction identity is not current' USING ERRCODE='23514';
    END IF;
    SELECT COALESCE(max(sequence),0)+1 INTO expected_sequence
      FROM reconciliation_repair_events WHERE attempt_id=NEW.attempt_id;
    SELECT event_type INTO previous_type FROM reconciliation_repair_events
      WHERE attempt_id=NEW.attempt_id ORDER BY sequence DESC LIMIT 1;
    IF NEW.sequence<>expected_sequence THEN
        RAISE EXCEPTION 'repair event sequence is not contiguous' USING ERRCODE='23514';
    END IF;
    IF (NEW.sequence=1 AND NEW.event_type<>'prepared')
       OR (NEW.event_type='applying' AND previous_type NOT IN ('prepared','uncertain'))
       OR (NEW.event_type='revalidated' AND previous_type<>'applying')
       OR (NEW.event_type='rename' AND previous_type<>'revalidated')
       OR (NEW.event_type='source_directory_fsync' AND previous_type<>'rename')
       OR (NEW.event_type='destination_directory_fsync' AND previous_type<>'source_directory_fsync')
       OR (NEW.event_type='completed' AND previous_type<>'destination_directory_fsync')
       OR (NEW.event_type='stale' AND previous_type NOT IN ('applying','revalidated'))
       OR (NEW.event_type='resolved' AND previous_type NOT IN ('applying','revalidated'))
       OR (NEW.event_type='failed' AND previous_type NOT IN ('applying','revalidated'))
       OR (NEW.event_type='uncertain' AND previous_type NOT IN
             ('applying','revalidated','rename','source_directory_fsync','destination_directory_fsync'))
       OR (NEW.event_type='unknown' AND previous_type<>'applying')
       OR (NEW.sequence>1 AND NEW.event_type='prepared') THEN
        RAISE EXCEPTION 'invalid repair event transition: % to %',previous_type,NEW.event_type USING ERRCODE='23514';
    END IF;
    IF previous_type IN ('completed','failed','stale','resolved','unknown') THEN
        RAISE EXCEPTION 'repair attempt is terminal' USING ERRCODE='23514';
    END IF;
    SELECT max(sequence) INTO last_applying_sequence FROM reconciliation_repair_events
      WHERE attempt_id=NEW.attempt_id AND event_type='applying';
    IF NEW.event_type<>'prepared' AND NEW.event_type<>'applying' THEN
        SELECT transaction_id INTO applying_transaction FROM reconciliation_repair_events
          WHERE attempt_id=NEW.attempt_id AND sequence=last_applying_sequence;
        IF applying_transaction IS NULL OR NEW.transaction_id IS DISTINCT FROM applying_transaction THEN
            RAISE EXCEPTION 'applying through convergence boundary requires one transaction' USING ERRCODE='23514';
        END IF;
    END IF;
    IF NEW.event_type='unknown' AND NOT EXISTS (
        SELECT 1 FROM reconciliation_repair_events
        WHERE attempt_id=NEW.attempt_id AND event_type IN
          ('uncertain','rename','source_directory_fsync','destination_directory_fsync')
    ) THEN
        RAISE EXCEPTION 'final unknown requires prior physical-effect uncertainty' USING ERRCODE='23514';
    END IF;
    IF NEW.event_type IN ('failed','stale','resolved') AND EXISTS (
        SELECT 1 FROM reconciliation_repair_events
        WHERE attempt_id=NEW.attempt_id AND event_type IN
          ('rename','source_directory_fsync','destination_directory_fsync','uncertain')
    ) THEN
        RAISE EXCEPTION 'physical-effect-possible repair cannot become a definite non-quarantine outcome' USING ERRCODE='23514';
    END IF;
    IF NEW.event_type NOT IN ('failed','stale','resolved','uncertain','unknown')
       AND NEW.outcome_code IS DISTINCT FROM (CASE NEW.event_type
         WHEN 'prepared' THEN 'prepared' WHEN 'applying' THEN 'applying'
         WHEN 'revalidated' THEN 'matched' WHEN 'rename' THEN 'renamed'
         WHEN 'source_directory_fsync' THEN 'durable'
         WHEN 'destination_directory_fsync' THEN 'durable'
         WHEN 'completed' THEN 'quarantined' END) THEN
        RAISE EXCEPTION 'repair event outcome does not match transition' USING ERRCODE='23514';
    END IF;
    IF (NEW.event_type='failed' AND NEW.outcome_code<>'failed')
       OR (NEW.event_type='stale' AND NEW.outcome_code<>'stale')
       OR (NEW.event_type='resolved' AND NEW.outcome_code<>'resolved')
       OR (NEW.event_type IN ('uncertain','unknown') AND NEW.outcome_code<>'unknown') THEN
        RAISE EXCEPTION 'repair terminal outcome is inconsistent' USING ERRCODE='23514';
    END IF;
    IF NEW.event_type IN ('revalidated','completed')
       AND (NEW.observed_size_bytes IS DISTINCT FROM item.expected_size_bytes
         OR NEW.observed_sha256 IS DISTINCT FROM item.report_observed_sha256) THEN
        RAISE EXCEPTION 'repair observation does not match sealed manifest' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_require_terminal_repair_attempt()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE latest_type text; latest_xid xid8;
BEGIN
    IF NEW.event_type='applying' THEN
        SELECT event_type,transaction_id INTO latest_type,latest_xid
          FROM reconciliation_repair_events WHERE attempt_id=NEW.attempt_id
          ORDER BY sequence DESC LIMIT 1;
        IF latest_xid IS DISTINCT FROM NEW.transaction_id
           OR latest_type NOT IN ('completed','failed','stale','resolved','uncertain','unknown') THEN
            RAISE EXCEPTION 'applying repair segment must reach a convergence boundary in one transaction'
              USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_append_repair_event(
    repair_event_id nmcp_uuid_v4,repair_attempt_id nmcp_uuid_v4,repair_event_type text,
    repair_outcome_code text,repair_error_code text,repair_observed_size bigint,
    repair_observed_sha text
) RETURNS integer LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE existing reconciliation_repair_events%ROWTYPE; next_sequence integer;
        item_id nmcp_uuid_v4; run_id nmcp_uuid_v4;
BEGIN
    SELECT m.id,m.run_id INTO STRICT item_id,run_id
      FROM reconciliation_repair_attempts AS a
      JOIN reconciliation_repair_manifest_items AS m ON m.id=a.manifest_item_id
      WHERE a.id=repair_attempt_id;
    IF repair_event_type='applying' THEN
        PERFORM * FROM nmcp_lock_repair_item_apply(run_id,item_id,repair_attempt_id);
    ELSE
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(run_id::text,0));
    END IF;
    SELECT * INTO existing FROM reconciliation_repair_events WHERE id=repair_event_id;
    IF FOUND THEN
        IF existing.attempt_id=repair_attempt_id AND existing.event_type=repair_event_type
           AND existing.outcome_code=repair_outcome_code
           AND existing.error_code IS NOT DISTINCT FROM repair_error_code
           AND existing.observed_size_bytes IS NOT DISTINCT FROM repair_observed_size
           AND existing.observed_sha256 IS NOT DISTINCT FROM repair_observed_sha THEN
            RETURN existing.sequence;
        END IF;
        RAISE EXCEPTION 'repair event replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    PERFORM 1 FROM reconciliation_repair_attempts WHERE id=repair_attempt_id FOR UPDATE;
    SELECT COALESCE(max(sequence),0)+1 INTO next_sequence
      FROM reconciliation_repair_events WHERE attempt_id=repair_attempt_id;
    INSERT INTO reconciliation_repair_events(
      id,attempt_id,sequence,event_type,outcome_code,error_code,
      observed_size_bytes,observed_sha256
    ) VALUES(repair_event_id,repair_attempt_id,next_sequence,repair_event_type,
      repair_outcome_code,repair_error_code,repair_observed_size,repair_observed_sha);
    RETURN next_sequence;
END;
$$;

CREATE FUNCTION nmcp_repair_effective_outcome(requested_item_id nmcp_uuid_v4)
RETURNS text LANGUAGE sql STABLE AS $$
  WITH history AS (
    SELECT a.attempt_number,e.sequence,e.event_type,e.outcome_code,e.error_code
    FROM reconciliation_repair_attempts AS a
    JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
    WHERE a.manifest_item_id=requested_item_id
  ), latest AS (
    SELECT event_type,outcome_code,error_code FROM history
    ORDER BY attempt_number DESC,sequence DESC LIMIT 1
  )
  SELECT CASE
    WHEN EXISTS (SELECT 1 FROM history WHERE event_type='completed' AND outcome_code='quarantined')
      THEN 'quarantined'
    WHEN EXISTS (SELECT 1 FROM history WHERE event_type IN
      ('rename','source_directory_fsync','destination_directory_fsync','uncertain'))
      AND NOT EXISTS (SELECT 1 FROM history WHERE event_type='unknown') THEN NULL
    WHEN EXISTS (SELECT 1 FROM history WHERE event_type='unknown') THEN 'unknown'
    WHEN (SELECT event_type FROM latest) IN ('prepared','applying','revalidated','rename',
      'source_directory_fsync','destination_directory_fsync','uncertain') THEN NULL
    WHEN (SELECT event_type FROM latest)='stale' THEN 'stale'
    WHEN (SELECT event_type FROM latest)='resolved' THEN 'resolved'
    WHEN (SELECT event_type FROM latest)='failed' THEN 'failed'
    ELSE NULL END;
$$;

CREATE FUNCTION nmcp_read_repair_run_outcome(requested_run_id nmcp_uuid_v4)
RETURNS TABLE(
    run_id uuid,source_report_id uuid,authorization_id uuid,authorized_actor text,
    started_at timestamptz,plan_id uuid,plan_version smallint,plan_item_count bigint,
    plan_digest text,result text,quarantined_count bigint,stale_count bigint,
    resolved_count bigint,failed_count bigint,unknown_count bigint,completed_at timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM reconciliation_repair_runs WHERE id=requested_run_id) THEN
        RAISE EXCEPTION 'repair run is absent' USING ERRCODE='23514';
    END IF;
    RETURN QUERY SELECT r.id::uuid,r.source_report_id::uuid,r.authorization_id::uuid,
      r.authorized_actor,r.started_at,seal.id::uuid,seal.plan_version,seal.item_count,
      seal.plan_digest,result.result,result.quarantined_count,result.stale_count,
      result.resolved_count,result.failed_count,result.unknown_count,result.completed_at
    FROM reconciliation_repair_runs AS r
    LEFT JOIN reconciliation_repair_plan_seals AS seal ON seal.run_id=r.id
    LEFT JOIN reconciliation_repair_results AS result ON result.run_id=r.id
    WHERE r.id=requested_run_id;
END;
$$;

CREATE FUNCTION nmcp_read_repair_run_by_authorization(requested_authorization_id nmcp_uuid_v4)
RETURNS TABLE(
    run_id uuid,source_report_id uuid,authorization_id uuid,authorized_actor text,
    started_at timestamptz,plan_id uuid,plan_version smallint,plan_item_count bigint,
    plan_digest text,result text,quarantined_count bigint,stale_count bigint,
    resolved_count bigint,failed_count bigint,unknown_count bigint,completed_at timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE matched_run nmcp_uuid_v4;
BEGIN
    SELECT candidate.id INTO matched_run FROM reconciliation_repair_runs AS candidate
      WHERE candidate.authorization_id=requested_authorization_id;
    IF matched_run IS NULL THEN RETURN; END IF;
    RETURN QUERY SELECT * FROM nmcp_read_repair_run_outcome(matched_run);
END;
$$;

CREATE FUNCTION nmcp_read_repair_manifest(
    requested_run_id nmcp_uuid_v4,after_ordinal bigint,page_limit integer
) RETURNS TABLE(
    ordinal bigint,item_id uuid,finding_id uuid,quarantine_id uuid,source_type text,
    subject_id uuid,media_id uuid,job_id uuid,job_target_id uuid,storage_attempt_id uuid,
    source_relative_key text,expected_size_bytes bigint,expected_sha256 text,
    observed_mtime timestamptz,observed_ctime timestamptz,owner_snapshot text,
    destination_relative_key text,effective_outcome text,active_attempt_id uuid,
    latest_attempt_number integer
) LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF after_ordinal<0 OR page_limit<1 OR page_limit>1000 THEN
        RAISE EXCEPTION 'invalid repair manifest page' USING ERRCODE='22023';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM reconciliation_repair_runs WHERE id=requested_run_id) THEN
        RAISE EXCEPTION 'repair run is absent' USING ERRCODE='23514';
    END IF;
    RETURN QUERY
    SELECT item.report_ordinal,item.id::uuid,item.finding_id::uuid,item.quarantine_id::uuid,
      item.source_type,f.subject_id::uuid,item.media_id::uuid,item.job_id::uuid,
      item.job_target_id::uuid,item.storage_attempt_id::uuid,item.source_relative_key,
      item.expected_size_bytes,item.report_observed_sha256,f.observed_mtime,f.observed_ctime,
      item.owner_snapshot,item.destination_relative_key,
      nmcp_repair_effective_outcome(item.id),latest.id::uuid,latest.attempt_number
    FROM reconciliation_repair_manifest_items AS item
    JOIN reconciliation_check_findings AS f ON f.id=item.finding_id AND f.report_id=item.source_report_id
    LEFT JOIN LATERAL (
      SELECT a.id,a.attempt_number FROM reconciliation_repair_attempts AS a
      WHERE a.manifest_item_id=item.id ORDER BY a.attempt_number DESC LIMIT 1
    ) AS latest ON true
    WHERE item.run_id=requested_run_id AND item.report_ordinal>after_ordinal
    ORDER BY item.report_ordinal LIMIT page_limit;
END;
$$;

CREATE FUNCTION nmcp_read_repair_attempt_history(
    requested_item_id nmcp_uuid_v4,after_attempt_number integer,
    after_sequence integer,page_limit integer
) RETURNS TABLE(
    attempt_number integer,attempt_id uuid,started_at timestamptz,sequence integer,
    event_id uuid,event_type text,outcome_code text,error_code text,
    observed_size_bytes bigint,observed_sha256 text,transaction_id xid8,
    occurred_at timestamptz,physical_effect_possible boolean
) LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF after_attempt_number<0 OR after_sequence<0 OR page_limit<1 OR page_limit>1000 THEN
        RAISE EXCEPTION 'invalid repair history page' USING ERRCODE='22023';
    END IF;
    RETURN QUERY
    SELECT a.attempt_number,a.id::uuid,a.started_at,e.sequence,e.id::uuid,e.event_type,
      e.outcome_code,e.error_code,e.observed_size_bytes,e.observed_sha256,
      e.transaction_id,e.occurred_at,
      EXISTS (SELECT 1 FROM reconciliation_repair_events AS effect
        WHERE effect.attempt_id=a.id AND effect.event_type IN
          ('rename','source_directory_fsync','destination_directory_fsync','uncertain','completed'))
    FROM reconciliation_repair_attempts AS a
    JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
    WHERE a.manifest_item_id=requested_item_id
      AND (a.attempt_number>after_attempt_number
        OR (a.attempt_number=after_attempt_number AND e.sequence>after_sequence))
    ORDER BY a.attempt_number,e.sequence LIMIT page_limit;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_repair_result_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE sealed_count bigint; actual_manifest bigint; actual_quarantined bigint;
        actual_stale bigint; actual_resolved bigint; actual_failed bigint;
        actual_unknown bigint; incomplete bigint;
BEGIN
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(NEW.run_id::text,0));
    SELECT item_count INTO sealed_count FROM reconciliation_repair_plan_seals
      WHERE run_id=NEW.run_id;
    IF sealed_count IS NULL THEN
        RAISE EXCEPTION 'repair result requires a sealed plan' USING ERRCODE='23514';
    END IF;
    SELECT count(*),
      count(*) FILTER (WHERE outcome='quarantined'),
      count(*) FILTER (WHERE outcome='stale'),
      count(*) FILTER (WHERE outcome='resolved'),
      count(*) FILTER (WHERE outcome='failed'),
      count(*) FILTER (WHERE outcome='unknown'),
      count(*) FILTER (WHERE outcome IS NULL)
    INTO actual_manifest,actual_quarantined,actual_stale,actual_resolved,
      actual_failed,actual_unknown,incomplete
    FROM (
      SELECT nmcp_repair_effective_outcome(item.id) AS outcome
      FROM reconciliation_repair_manifest_items AS item WHERE item.run_id=NEW.run_id
    ) AS outcomes;
    IF actual_manifest<>sealed_count OR incomplete<>0
       OR NEW.manifest_count<>actual_manifest OR NEW.quarantined_count<>actual_quarantined
       OR NEW.stale_count<>actual_stale OR NEW.resolved_count<>actual_resolved
       OR NEW.failed_count<>actual_failed OR NEW.unknown_count<>actual_unknown
       OR NEW.result IS DISTINCT FROM (CASE WHEN actual_unknown>0 THEN 'unknown'
          WHEN actual_failed=0 THEN 'succeeded'
          WHEN actual_failed=actual_manifest THEN 'failed' ELSE 'partial' END) THEN
        RAISE EXCEPTION 'repair result is not derived from complete effective history' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_finish_repair_run(repair_run_id nmcp_uuid_v4)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE sealed_count bigint; actual_manifest bigint; actual_quarantined bigint;
        actual_stale bigint; actual_resolved bigint; actual_failed bigint;
        actual_unknown bigint; incomplete bigint; derived_result text;
        existing reconciliation_repair_results%ROWTYPE;
BEGIN
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(repair_run_id::text,0));
    PERFORM 1 FROM reconciliation_repair_runs WHERE id=repair_run_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'repair run is absent' USING ERRCODE='23514'; END IF;
    SELECT item_count INTO sealed_count FROM reconciliation_repair_plan_seals
      WHERE run_id=repair_run_id;
    IF sealed_count IS NULL THEN RAISE EXCEPTION 'repair run has no sealed plan' USING ERRCODE='23514'; END IF;
    SELECT count(*),
      count(*) FILTER (WHERE outcome='quarantined'),count(*) FILTER (WHERE outcome='stale'),
      count(*) FILTER (WHERE outcome='resolved'),count(*) FILTER (WHERE outcome='failed'),
      count(*) FILTER (WHERE outcome='unknown'),count(*) FILTER (WHERE outcome IS NULL)
    INTO actual_manifest,actual_quarantined,actual_stale,actual_resolved,
      actual_failed,actual_unknown,incomplete
    FROM (SELECT nmcp_repair_effective_outcome(item.id) AS outcome
      FROM reconciliation_repair_manifest_items AS item WHERE item.run_id=repair_run_id) AS outcomes;
    IF actual_manifest<>sealed_count OR incomplete<>0 THEN
        RAISE EXCEPTION 'repair run has incomplete or recoverable items' USING ERRCODE='23514';
    END IF;
    derived_result:=CASE WHEN actual_unknown>0 THEN 'unknown' WHEN actual_failed=0 THEN 'succeeded'
      WHEN actual_failed=actual_manifest THEN 'failed' ELSE 'partial' END;
    SELECT * INTO existing FROM reconciliation_repair_results WHERE run_id=repair_run_id;
    IF FOUND THEN
        IF existing.result=derived_result AND existing.manifest_count=actual_manifest
           AND existing.quarantined_count=actual_quarantined AND existing.stale_count=actual_stale
           AND existing.resolved_count=actual_resolved AND existing.failed_count=actual_failed
           AND existing.unknown_count=actual_unknown THEN RETURN; END IF;
        RAISE EXCEPTION 'repair result replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_repair_results(
      run_id,result,manifest_count,quarantined_count,stale_count,resolved_count,failed_count,unknown_count
    ) VALUES(repair_run_id,derived_result,actual_manifest,actual_quarantined,
      actual_stale,actual_resolved,actual_failed,actual_unknown);
END;
$$;

-- Preserve every completed or physical-effect-possible destination across all
-- attempts. The v18 reader remains an internal fixed-shape source for all
-- non-quarantine rows.
ALTER FUNCTION nmcp_read_check_database_references(timestamptz)
    RENAME TO nmcp_read_check_database_references_v18;
REVOKE ALL ON FUNCTION nmcp_read_check_database_references_v18(timestamptz)
    FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;

ALTER TABLE reconciliation_check_findings
    DROP CONSTRAINT reconciliation_check_findings_expected_state_check;
ALTER TABLE reconciliation_check_findings
    ADD CONSTRAINT reconciliation_check_findings_expected_state_check CHECK (
      expected_state IS NULL OR expected_state IN (
        'referenced','unreferenced','current','non_current','purged','cleanup_deleted',
        'quarantined','possible_quarantine'
      )
    );

CREATE FUNCTION nmcp_read_check_database_references(check_cutoff timestamptz)
RETURNS TABLE(
    row_kind text,subject_id uuid,media_id uuid,original_id uuid,job_id uuid,
    job_target_id uuid,relative_key text,expected_state text,size_bytes bigint,
    sha256 text,is_current boolean,provenance_valid boolean,current_valid boolean,
    due_at timestamptz
) LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    RETURN QUERY
    SELECT source.row_kind,source.subject_id,source.media_id,source.original_id,
      source.job_id,source.job_target_id,source.relative_key,source.expected_state,
      source.size_bytes,source.sha256,source.is_current,source.provenance_valid,
      source.current_valid,source.due_at
    FROM nmcp_read_check_database_references_v18(check_cutoff) AS source
    WHERE source.row_kind<>'quarantine'
    UNION ALL
    SELECT 'quarantine'::text,item.quarantine_id::uuid,item.media_id::uuid,NULL::uuid,
      item.job_id::uuid,item.job_target_id::uuid,item.destination_relative_key,
      CASE WHEN nmcp_repair_effective_outcome(item.id)='quarantined'
        THEN 'quarantined' ELSE 'possible_quarantine' END,
      COALESCE(latest.observed_size_bytes,item.expected_size_bytes),
      COALESCE(latest.observed_sha256,item.report_observed_sha256),
      NULL::boolean,true,true,COALESCE(latest.occurred_at,item.prepared_at)
    FROM reconciliation_repair_manifest_items AS item
    LEFT JOIN LATERAL (
      SELECT e.observed_size_bytes,e.observed_sha256,e.occurred_at
      FROM reconciliation_repair_attempts AS a
      JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
      WHERE a.manifest_item_id=item.id
      ORDER BY a.attempt_number DESC,e.sequence DESC LIMIT 1
    ) AS latest ON true
    WHERE EXISTS (SELECT 1 FROM reconciliation_repair_attempts AS a
      JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
      WHERE a.manifest_item_id=item.id AND e.event_type IN
        ('rename','source_directory_fsync','destination_directory_fsync','uncertain','unknown'))
    ORDER BY 1,7 NULLS FIRST,2;
END;
$$;

DO $$
DECLARE target_schema text:=current_schema(); function_signature text;
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
    FOREACH function_signature IN ARRAY ARRAY[
      'nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer)',
      'nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text)',
      'nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
      'nmcp_read_repair_plan_digest(nmcp_uuid_v4)',
      'nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text)',
      'nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
      'nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
      'nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)',
      'nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text)',
      'nmcp_read_repair_run_outcome(nmcp_uuid_v4)',
      'nmcp_read_repair_run_by_authorization(nmcp_uuid_v4)',
      'nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer)',
      'nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer)',
      'nmcp_finish_repair_run(nmcp_uuid_v4)'
    ] LOOP
      EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_repair_function_owner',target_schema,function_signature);
      EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    FOREACH function_signature IN ARRAY ARRAY[
      'nmcp_repair_owner_snapshot(text,nmcp_uuid_v4,uuid,uuid)',
      'nmcp_repair_authority_rows(nmcp_uuid_v4,nmcp_uuid_v4,text,text)',
      'nmcp_repair_effective_outcome(nmcp_uuid_v4)'
    ] LOOP
      EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_repair_function_owner',target_schema,function_signature);
      EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    FOREACH function_signature IN ARRAY ARRAY[
      'nmcp_guard_repair_attempt_insert()','nmcp_guard_repair_event_insert()',
      'nmcp_guard_repair_result_insert()','nmcp_require_terminal_repair_attempt()'
    ] LOOP
      EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_check_database_references(timestamptz) OWNER TO nmcp_check_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_check_database_references(timestamptz) SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_check_database_references_v18(timestamptz) SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
END;
$$;

REVOKE ALL ON reconciliation_repair_plan_seals
  FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
GRANT SELECT,INSERT ON reconciliation_repair_plan_seals TO nmcp_repair_function_owner;
GRANT SELECT(report_ordinal) ON reconciliation_repair_manifest_items TO nmcp_repair_function_owner;
GRANT SELECT(version,name,checksum,dirty) ON schema_migrations TO nmcp_repair_function_owner;
-- The checker definer joins only this narrow projection to correlate a
-- journaled physical effect with its exact quarantine destination. It does not
-- receive table-wide repair authority.
GRANT SELECT(id,quarantine_id,media_id,job_id,job_target_id,
  destination_relative_key,expected_size_bytes,report_observed_sha256,prepared_at)
  ON reconciliation_repair_manifest_items TO nmcp_check_function_owner;
GRANT SELECT(id,manifest_item_id,attempt_number)
  ON reconciliation_repair_attempts TO nmcp_check_function_owner;
GRANT SELECT(attempt_id,sequence,event_type,outcome_code,error_code,
  observed_size_bytes,observed_sha256,occurred_at)
  ON reconciliation_repair_events TO nmcp_check_function_owner;

REVOKE ALL ON FUNCTION
  nmcp_begin_repair_run(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text),
  nmcp_prepare_repair_manifest_item(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;

REVOKE ALL ON FUNCTION
  nmcp_repair_owner_snapshot(text,nmcp_uuid_v4,uuid,uuid),
  nmcp_repair_authority_rows(nmcp_uuid_v4,nmcp_uuid_v4,text,text),
  nmcp_repair_effective_outcome(nmcp_uuid_v4)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_repair_effective_outcome(nmcp_uuid_v4)
  TO nmcp_check_function_owner;

REVOKE ALL ON FUNCTION
  nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer),
  nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text),
  nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_read_repair_plan_digest(nmcp_uuid_v4),
  nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text),
  nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text),
  nmcp_read_repair_run_outcome(nmcp_uuid_v4),
  nmcp_read_repair_run_by_authorization(nmcp_uuid_v4),
  nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer),
  nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer),
  nmcp_finish_repair_run(nmcp_uuid_v4)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime;

GRANT EXECUTE ON FUNCTION
  nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer),
  nmcp_begin_repair_run_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text),
  nmcp_prepare_repair_manifest_item_v2(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_read_repair_plan_digest(nmcp_uuid_v4),
  nmcp_seal_repair_plan(nmcp_uuid_v4,nmcp_uuid_v4,smallint,bigint,text),
  nmcp_preflight_repair_finding(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_lock_repair_item_apply(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_begin_repair_attempt(nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4),
  nmcp_append_repair_event(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,bigint,text),
  nmcp_read_repair_run_outcome(nmcp_uuid_v4),
  nmcp_read_repair_run_by_authorization(nmcp_uuid_v4),
  nmcp_read_repair_manifest(nmcp_uuid_v4,bigint,integer),
  nmcp_read_repair_attempt_history(nmcp_uuid_v4,integer,integer,integer),
  nmcp_finish_repair_run(nmcp_uuid_v4)
TO nmcp_repair_runtime;

REVOKE ALL ON FUNCTION nmcp_read_check_database_references(timestamptz)
  FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_read_check_database_references(timestamptz)
  TO nmcp_check_runtime;
