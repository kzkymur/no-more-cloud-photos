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
    WITH rendition_base AS (
        SELECT r.id,r.media_id,j.original_id,j.id AS job_id,r.job_target_id,
               r.relative_path,r.size_bytes,r.sha256,r.is_current,r.purge_after,
               p.version AS profile_version,
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
    ), rendition_validity AS (
        SELECT b.*,
               COALESCE((count(*) FILTER (WHERE b.is_current) OVER (PARTITION BY b.media_id,r.profile_key)=1
                AND max(b.profile_version) FILTER (WHERE b.provenance_valid)
                    OVER (PARTITION BY b.media_id,r.profile_key)
                    <= max(b.profile_version) FILTER (WHERE b.is_current AND b.provenance_valid)
                    OVER (PARTITION BY b.media_id,r.profile_key)
                AND bool_and(NOT b.is_current OR b.purge_after IS NULL)
                    OVER (PARTITION BY b.media_id,r.profile_key)),false) AS current_valid
        FROM rendition_base AS b
        JOIN renditions AS r ON r.id=b.id
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
    ORDER BY 1,7 NULLS FIRST,2;
END;
$$;

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
      (SELECT finding_count FROM reconciliation_check_report_seals WHERE report_id=outcome_report_id),
      (SELECT fs_scan_ended_at FROM reconciliation_check_report_seals WHERE report_id=outcome_report_id);
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

GRANT SELECT ON change_events TO nmcp_check_function_owner;

REVOKE ALL ON FUNCTION nmcp_read_check_database_references(timestamptz),
    nmcp_read_check_attempt_owners(),nmcp_read_check_report_outcome(nmcp_uuid_v4)
FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_repair_runtime;
GRANT EXECUTE ON FUNCTION nmcp_read_check_database_references(timestamptz),
    nmcp_read_check_attempt_owners(),nmcp_read_check_report_outcome(nmcp_uuid_v4)
TO nmcp_check_runtime;
