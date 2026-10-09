-- The checker reads fixed-shape, ordered source snapshots without direct table
-- privileges. Callers must execute both readers in one read-only repeatable-read
-- transaction and use the same database cutoff for all derived candidates.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_check_runtime' AND NOT rolcanlogin AND NOT rolinherit
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='nmcp_check_function_owner' AND NOT rolcanlogin AND NOT rolinherit
    ) THEN
        RAISE EXCEPTION 'checker role topology is unsafe' USING ERRCODE='55000';
    END IF;
END;
$$;

CREATE FUNCTION nmcp_read_check_database_references(check_cutoff timestamptz)
RETURNS TABLE(
    row_kind text,subject_id uuid,media_id uuid,
    original_id uuid,job_id uuid,job_target_id uuid,
    relative_key text,expected_state text,size_bytes bigint,sha256 text,
    is_current boolean,provenance_valid boolean,current_valid boolean,due_at timestamptz
)
LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF pg_catalog.current_setting('transaction_isolation')<>'repeatable read'
       OR pg_catalog.current_setting('transaction_read_only')<>'on' THEN
        RAISE EXCEPTION 'checker source reads require a read-only repeatable-read transaction'
            USING ERRCODE='25001';
    END IF;
    IF check_cutoff IS NULL THEN
        RAISE EXCEPTION 'checker cutoff is required' USING ERRCODE='22004';
    END IF;

    RETURN QUERY
    WITH publication_authority AS (
        SELECT r.id AS rendition_id,max(e.position) AS publication_position
        FROM change_events AS e
        CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(
            COALESCE(e.payload->'current_renditions','[]'::jsonb)) AS published
        JOIN renditions AS r ON r.id::text=published->>'id'
          AND r.media_id=e.media_id AND r.created_at=e.occurred_at
          AND r.profile_key=published#>>'{profile,key}'
        WHERE e.event_type='media_upsert' AND e.reason='rendition_current'
        GROUP BY r.id
    ), rendition_base AS (
        SELECT r.id,r.media_id,j.original_id,j.id AS job_id,r.job_target_id,
               r.relative_path,r.size_bytes,r.sha256,r.is_current,r.purge_after,
               r.profile_key,p.version AS profile_version,pa.publication_position,
               (jt.id IS NOT NULL AND jt.status='succeeded'
                AND j.id IS NOT NULL AND j.type='transform'
                AND j.media_id_snapshot=r.media_id AND j.original_id IS NOT NULL
                AND o.id=j.original_id AND o.media_id=r.media_id
                AND p.id=jt.profile_id AND p.key=r.profile_key) AS provenance_valid
        FROM renditions AS r
        LEFT JOIN job_targets AS jt ON jt.id=r.job_target_id
        LEFT JOIN jobs AS j ON j.id=jt.job_id
        LEFT JOIN originals AS o ON o.id=j.original_id
        LEFT JOIN profiles AS p ON p.id=jt.profile_id
        LEFT JOIN publication_authority AS pa ON pa.rendition_id=r.id
    ), expected_current AS (
        SELECT DISTINCT ON (b.media_id,b.profile_key) b.media_id,b.profile_key,b.id
        FROM rendition_base AS b
        WHERE b.provenance_valid AND b.publication_position IS NOT NULL
        ORDER BY b.media_id,b.profile_key,b.profile_version DESC,b.publication_position DESC
    ), rendition_validity AS (
        SELECT b.*,
               COALESCE((count(*) FILTER (WHERE b.is_current) OVER (PARTITION BY b.media_id,b.profile_key)=1
                AND bool_and(NOT b.is_current OR (b.id=expected.id AND b.provenance_valid
                    AND b.publication_position IS NOT NULL AND b.purge_after IS NULL))
                    OVER (PARTITION BY b.media_id,b.profile_key)),false) AS current_valid
        FROM rendition_base AS b
        LEFT JOIN expected_current AS expected ON expected.media_id=b.media_id AND expected.profile_key=b.profile_key
    ), due_cleanup AS (
        SELECT r.id
        FROM renditions AS r
        JOIN media AS m ON m.id=r.media_id
        JOIN job_targets AS candidate_target ON candidate_target.id=r.job_target_id AND candidate_target.status='succeeded'
        JOIN jobs AS candidate_job ON candidate_job.id=candidate_target.job_id
          AND candidate_job.type='transform' AND candidate_job.media_id_snapshot=r.media_id
        JOIN profiles AS candidate_profile ON candidate_profile.id=candidate_target.profile_id AND candidate_profile.key=r.profile_key
        JOIN renditions AS current_rendition ON current_rendition.media_id=r.media_id
          AND current_rendition.profile_key=r.profile_key AND current_rendition.is_current
        JOIN job_targets AS current_target ON current_target.id=current_rendition.job_target_id AND current_target.status='succeeded'
        JOIN jobs AS current_job ON current_job.id=current_target.job_id
          AND current_job.type='transform' AND current_job.media_id_snapshot=r.media_id
        JOIN profiles AS current_profile ON current_profile.id=current_target.profile_id
          AND current_profile.key=r.profile_key AND current_profile.version>=candidate_profile.version
        WHERE NOT r.is_current AND r.purge_after IS NOT NULL AND r.purge_after<=check_cutoff
          AND NOT EXISTS (SELECT 1 FROM jobs AS purge_job
              WHERE purge_job.type='purge' AND purge_job.media_id_snapshot=r.media_id
                AND purge_job.started_at IS NOT NULL)
    ), repair_quarantine AS (
        SELECT m.quarantine_id,m.media_id,m.job_id,m.job_target_id,m.destination_relative_key,
               CASE WHEN latest.event_type='completed' THEN 'quarantined'::text ELSE NULL::text END AS expected_state,
               COALESCE(latest.observed_size_bytes,m.expected_size_bytes) AS expected_size_bytes,
               COALESCE(latest.observed_sha256,m.report_observed_sha256) AS expected_sha256,
               latest.occurred_at
        FROM reconciliation_repair_manifest_items AS m
        JOIN LATERAL (
            SELECT e.attempt_id,e.event_type,e.outcome_code,e.error_code,e.observed_size_bytes,e.observed_sha256,e.occurred_at
            FROM reconciliation_repair_attempts AS a
            JOIN reconciliation_repair_events AS e ON e.attempt_id=a.id
            WHERE a.manifest_item_id=m.id
            ORDER BY a.attempt_number DESC,e.sequence DESC LIMIT 1
        ) AS latest ON true
        WHERE (latest.event_type='completed' AND latest.outcome_code='quarantined')
           OR (latest.event_type='failed' AND latest.error_code='outcome_uncertain'
               AND EXISTS (SELECT 1 FROM reconciliation_repair_events AS renamed
                   WHERE renamed.attempt_id=latest.attempt_id AND renamed.event_type='rename'))
    )
    SELECT 'original'::text,o.id::uuid,o.media_id::uuid,o.id::uuid,NULL::uuid,NULL::uuid,
           o.relative_path,'referenced'::text,o.size_bytes,o.sha256,
           NULL::boolean,true,true,NULL::timestamptz
    FROM originals AS o
    UNION ALL
    SELECT 'transform_target',t.id::uuid,j.media_id_snapshot::uuid,j.original_id::uuid,j.id::uuid,t.id::uuid,
           NULL::text,NULL::text,NULL::bigint,NULL::text,NULL::boolean,
           (j.type='transform' AND j.original_id IS NOT NULL AND o.id=j.original_id
             AND o.media_id=j.media_id_snapshot),true,NULL::timestamptz
    FROM job_targets AS t
    JOIN jobs AS j ON j.id=t.job_id
    LEFT JOIN originals AS o ON o.id=j.original_id
    WHERE j.type='transform'
    UNION ALL
    SELECT 'rendition',r.id::uuid,r.media_id::uuid,r.original_id::uuid,r.job_id::uuid,r.job_target_id::uuid,
           r.relative_path,CASE WHEN r.is_current THEN 'current' ELSE 'non_current' END,
           r.size_bytes,r.sha256,r.is_current,r.provenance_valid,r.current_valid,r.purge_after
    FROM rendition_validity AS r
    UNION ALL
    SELECT 'expired_media',m.id::uuid,m.id::uuid,NULL::uuid,j.id::uuid,NULL::uuid,
           NULL::text,NULL::text,NULL::bigint,NULL::text,NULL::boolean,true,true,m.purge_after
    FROM media AS m
    LEFT JOIN LATERAL (
        SELECT candidate.id FROM jobs AS candidate
        WHERE candidate.type='purge' AND candidate.media_id_snapshot=m.id
          AND candidate.status IN ('queued','running','failed')
        ORDER BY candidate.created_at,candidate.id LIMIT 1
    ) AS j ON true
    WHERE m.deleted_at IS NOT NULL AND m.purge_after IS NOT NULL AND m.purge_after<=check_cutoff
    UNION ALL
    SELECT 'expired_rendition',r.id::uuid,r.media_id::uuid,j.original_id::uuid,j.id::uuid,r.job_target_id::uuid,
           r.relative_path,'non_current',r.size_bytes,r.sha256,r.is_current,true,true,r.purge_after
    FROM due_cleanup AS d
    JOIN renditions AS r ON r.id=d.id
    JOIN job_targets AS t ON t.id=r.job_target_id
    JOIN jobs AS j ON j.id=t.job_id
    UNION ALL
    SELECT CASE p.object_kind WHEN 'original' THEN 'purged_original' ELSE 'purged_rendition' END,
           p.object_id::uuid,p.media_id_snapshot::uuid,NULL::uuid,p.job_id::uuid,
           NULL::uuid,
           p.relative_path,'purged',p.size_bytes,NULL::text,NULL::boolean,true,true,p.completed_at
    FROM purge_file_progress AS p
    JOIN jobs AS j ON j.id=p.job_id AND j.type='purge' AND j.status='succeeded'
    JOIN media_purge_identity_guard AS g ON g.media_id=p.media_id_snapshot
      AND g.state='purged' AND g.purge_job_id=p.job_id
    WHERE p.disposition IN ('deleted','missing')
      AND NOT EXISTS (SELECT 1 FROM media AS m WHERE m.id=p.media_id_snapshot)
    UNION ALL
    SELECT 'cleanup_deleted',p.rendition_id::uuid,p.media_id_snapshot::uuid,j.original_id::uuid,j.id::uuid,p.job_target_id::uuid,
           p.relative_path,'cleanup_deleted',p.size_bytes,NULL::text,false,true,true,p.completed_at
    FROM rendition_cleanup_progress AS p
    JOIN job_targets AS t ON t.id=p.job_target_id
    JOIN jobs AS j ON j.id=t.job_id
    WHERE p.disposition IN ('deleted','missing')
      AND NOT EXISTS (SELECT 1 FROM renditions AS r WHERE r.id=p.rendition_id)
    UNION ALL
    SELECT 'quarantine',q.quarantine_id::uuid,q.media_id::uuid,NULL::uuid,q.job_id::uuid,q.job_target_id::uuid,
           q.destination_relative_key,q.expected_state,q.expected_size_bytes,q.expected_sha256,
           NULL::boolean,true,true,q.occurred_at
    FROM repair_quarantine AS q
    ORDER BY 1,7 NULLS FIRST,2;
END;
$$;

ALTER TABLE reconciliation_check_findings DROP CONSTRAINT reconciliation_check_findings_kind_check;
ALTER TABLE reconciliation_check_findings ADD CONSTRAINT reconciliation_check_findings_kind_check CHECK (kind IN (
    'aged_attempt_temp','recent_or_live_temp','final_orphan','referenced_missing',
    'size_mismatch','sha256_mismatch','invalid_current','invalid_provenance',
    'expired_media_candidate','expired_rendition_candidate','terminal_delete_residue',
    'unexpected_path','unexpected_type','unexpected_hardlink','symlink','unstable_observation',
    'quarantine_missing','quarantine_mismatch'
));

CREATE FUNCTION nmcp_read_check_report_outcome(outcome_report_id nmcp_uuid_v4)
RETURNS TABLE(
    database_result text,database_error text,attempt_result text,attempt_error text,
    storage_result text,storage_error text,finding_count bigint,sealed boolean,
    sealed_finding_count bigint,fs_scan_ended_at timestamptz
)
LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM reconciliation_check_reports WHERE id=outcome_report_id) THEN
        RAISE EXCEPTION 'check report is absent' USING ERRCODE='23514';
    END IF;
    RETURN QUERY SELECT
      (SELECT result FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='database_references'),
      (SELECT error_code FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='database_references'),
      (SELECT result FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='attempt_owners'),
      (SELECT error_code FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='attempt_owners'),
      (SELECT result FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='storage_scan'),
      (SELECT error_code FROM reconciliation_check_source_results WHERE report_id=outcome_report_id AND source='storage_scan'),
      (SELECT count(*) FROM reconciliation_check_findings WHERE report_id=outcome_report_id),
      EXISTS (SELECT 1 FROM reconciliation_check_report_seals WHERE report_id=outcome_report_id),
      (SELECT seal.finding_count FROM reconciliation_check_report_seals AS seal WHERE seal.report_id=outcome_report_id),
      (SELECT seal.fs_scan_ended_at FROM reconciliation_check_report_seals AS seal WHERE seal.report_id=outcome_report_id);
END;
$$;

CREATE FUNCTION nmcp_read_check_attempt_owners()
RETURNS TABLE(
    attempt_id uuid,attempt_kind text,coverage text,original_id uuid,
    job_id uuid,temp_relative_key text,event_type text,
    lease_expires_at timestamptz,event_occurred_at timestamptz
)
LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF pg_catalog.current_setting('transaction_isolation')<>'repeatable read'
       OR pg_catalog.current_setting('transaction_read_only')<>'on' THEN
        RAISE EXCEPTION 'checker source reads require a read-only repeatable-read transaction'
            USING ERRCODE='25001';
    END IF;
    RETURN QUERY
    SELECT a.id::uuid,a.kind,a.coverage,a.original_id::uuid,a.job_id::uuid,a.temp_relative_key,
           latest.event_type,latest.lease_expires_at,latest.occurred_at
    FROM storage_attempts AS a
    JOIN LATERAL (
        SELECT e.event_type,e.lease_expires_at,e.occurred_at
        FROM storage_attempt_events AS e WHERE e.attempt_id=a.id
        ORDER BY e.sequence DESC LIMIT 1
    ) AS latest ON true
    ORDER BY a.id;
END;
$$;

-- Exact replay is required because a source-result commit can succeed after
-- the caller loses the server response. Contradictory replay remains closed.
CREATE OR REPLACE FUNCTION nmcp_complete_check_source(
    source_report_id nmcp_uuid_v4,source_name text,source_error_code text
) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE requested_result text:=CASE WHEN source_error_code IS NULL THEN 'complete' ELSE 'error' END;
        existing_result text; existing_error text;
BEGIN
    PERFORM 1 FROM reconciliation_check_reports WHERE id=source_report_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'check report is absent' USING ERRCODE='23514'; END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_check_report_seals WHERE report_id=source_report_id) THEN
        RAISE EXCEPTION 'sealed check report is immutable' USING ERRCODE='23514';
    END IF;
    SELECT result,error_code INTO existing_result,existing_error
      FROM reconciliation_check_source_results
      WHERE report_id=source_report_id AND source=source_name;
    IF FOUND THEN
        IF existing_result=requested_result AND existing_error IS NOT DISTINCT FROM source_error_code THEN RETURN; END IF;
        RAISE EXCEPTION 'check source result replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_check_source_results(report_id,source,result,error_code)
      VALUES(source_report_id,source_name,requested_result,source_error_code);
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_begin_check_report(
    report_id nmcp_uuid_v4,report_scope text,report_classifier_version smallint,
    snapshot_started timestamptz,cutoff_at timestamptz,snapshot_ended timestamptz,scan_started timestamptz
) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE migration_row schema_migrations%ROWTYPE; existing reconciliation_check_reports%ROWTYPE;
BEGIN
    SELECT * INTO STRICT migration_row FROM schema_migrations WHERE version=(SELECT max(version) FROM schema_migrations);
    SELECT * INTO existing FROM reconciliation_check_reports WHERE id=report_id FOR UPDATE;
    IF FOUND THEN
        IF existing.format_version=1 AND existing.classifier_version=report_classifier_version
           AND existing.scope=report_scope AND existing.migration_version=migration_row.version
           AND existing.migration_name=migration_row.name AND existing.migration_checksum=migration_row.checksum
           AND existing.db_snapshot_started_at=snapshot_started AND existing.db_cutoff_at=cutoff_at
           AND existing.db_snapshot_ended_at=snapshot_ended AND existing.fs_scan_started_at=scan_started THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'check report replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    INSERT INTO reconciliation_check_reports(
        id,format_version,classifier_version,scope,migration_version,migration_name,migration_checksum,
        db_snapshot_started_at,db_cutoff_at,db_snapshot_ended_at,fs_scan_started_at,temp_cutoff_at
    ) VALUES(report_id,1,report_classifier_version,report_scope,migration_row.version,migration_row.name,migration_row.checksum,
        snapshot_started,cutoff_at,snapshot_ended,scan_started,cutoff_at-interval '48 hours');
END;
$$;

ALTER FUNCTION nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)
    RENAME TO nmcp_append_check_finding_once;
REVOKE ALL ON FUNCTION nmcp_append_check_finding_once(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)
    FROM PUBLIC,nmcp_check_runtime,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;

CREATE FUNCTION nmcp_append_check_finding(
    finding_id nmcp_uuid_v4,finding_report_id nmcp_uuid_v4,finding_kind text,finding_reason text,
    finding_subject_type text,finding_subject_id nmcp_uuid_v4,
    finding_media_id nmcp_uuid_v4,finding_job_id nmcp_uuid_v4,finding_target_id nmcp_uuid_v4,
    finding_attempt_id nmcp_uuid_v4,finding_manifest_item_id nmcp_uuid_v4,finding_relative_key text,
    finding_expected_state text,finding_expected_size bigint,finding_expected_sha text,
    finding_observed_type text,finding_observed_size bigint,finding_observed_sha text,
    finding_observed_mtime timestamptz,finding_observed_ctime timestamptz,finding_observed_at timestamptz
) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE existing reconciliation_check_findings%ROWTYPE;
BEGIN
    SELECT * INTO existing FROM reconciliation_check_findings WHERE id=finding_id;
    IF FOUND THEN
        IF existing.report_id=finding_report_id AND existing.kind=finding_kind
           AND existing.reason_code=finding_reason AND existing.subject_type=finding_subject_type
           AND existing.subject_id IS NOT DISTINCT FROM finding_subject_id
           AND existing.media_id IS NOT DISTINCT FROM finding_media_id
           AND existing.job_id IS NOT DISTINCT FROM finding_job_id
           AND existing.job_target_id IS NOT DISTINCT FROM finding_target_id
           AND existing.attempt_id IS NOT DISTINCT FROM finding_attempt_id
           AND existing.manifest_item_id IS NOT DISTINCT FROM finding_manifest_item_id
           AND existing.relative_key IS NOT DISTINCT FROM finding_relative_key
           AND existing.expected_state IS NOT DISTINCT FROM finding_expected_state
           AND existing.expected_size_bytes IS NOT DISTINCT FROM finding_expected_size
           AND existing.expected_sha256 IS NOT DISTINCT FROM finding_expected_sha
           AND existing.observed_type=finding_observed_type
           AND existing.observed_size_bytes IS NOT DISTINCT FROM finding_observed_size
           AND existing.observed_sha256 IS NOT DISTINCT FROM finding_observed_sha
           AND existing.observed_mtime IS NOT DISTINCT FROM finding_observed_mtime
           AND existing.observed_ctime IS NOT DISTINCT FROM finding_observed_ctime
           AND existing.observed_at=finding_observed_at THEN
            RETURN existing.ordinal;
        END IF;
        RAISE EXCEPTION 'check finding replay conflicts with durable evidence' USING ERRCODE='23514';
    END IF;
    RETURN nmcp_append_check_finding_once(finding_id,finding_report_id,finding_kind,finding_reason,
      finding_subject_type,finding_subject_id,finding_media_id,finding_job_id,finding_target_id,
      finding_attempt_id,finding_manifest_item_id,finding_relative_key,finding_expected_state,
      finding_expected_size,finding_expected_sha,finding_observed_type,finding_observed_size,
      finding_observed_sha,finding_observed_mtime,finding_observed_ctime,finding_observed_at);
END;
$$;

DO $$
DECLARE target_schema text:=current_schema(); signature text:='nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)';
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_check_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_check_function_owner',target_schema,signature);
    EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,signature,target_schema);
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_check_function_owner',target_schema);
END;
$$;
REVOKE ALL ON FUNCTION nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)
    FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_append_check_finding(nmcp_uuid_v4,nmcp_uuid_v4,text,text,text,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,nmcp_uuid_v4,text,text,bigint,text,text,bigint,text,timestamptz,timestamptz,timestamptz)
    TO nmcp_check_runtime;

DO $$
DECLARE target_schema text:=current_schema(); function_signature text;
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_check_function_owner',target_schema);
    FOREACH function_signature IN ARRAY ARRAY[
        'nmcp_read_check_database_references(timestamptz)',
        'nmcp_read_check_attempt_owners()',
        'nmcp_read_check_report_outcome(nmcp_uuid_v4)'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_check_function_owner',target_schema,function_signature);
        EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,function_signature,target_schema);
    END LOOP;
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_check_function_owner',target_schema);
END;
$$;

REVOKE ALL ON change_events,reconciliation_repair_manifest_items,reconciliation_repair_attempts,reconciliation_repair_events
    FROM nmcp_check_function_owner;
GRANT SELECT(position,event_type,reason,media_id,payload,occurred_at) ON change_events TO nmcp_check_function_owner;
GRANT SELECT(id,quarantine_id,media_id,job_id,job_target_id,destination_relative_key,expected_size_bytes,report_observed_sha256)
    ON reconciliation_repair_manifest_items TO nmcp_check_function_owner;
GRANT SELECT(id,manifest_item_id,attempt_number) ON reconciliation_repair_attempts TO nmcp_check_function_owner;
GRANT SELECT(attempt_id,sequence,event_type,outcome_code,error_code,observed_size_bytes,observed_sha256,occurred_at)
    ON reconciliation_repair_events TO nmcp_check_function_owner;

REVOKE ALL ON FUNCTION nmcp_read_check_database_references(timestamptz),
    nmcp_read_check_attempt_owners(),nmcp_read_check_report_outcome(nmcp_uuid_v4)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_read_check_database_references(timestamptz),
    nmcp_read_check_attempt_owners(),nmcp_read_check_report_outcome(nmcp_uuid_v4)
TO nmcp_check_runtime;
