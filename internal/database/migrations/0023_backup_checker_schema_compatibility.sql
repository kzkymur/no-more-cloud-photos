-- Backup lifecycle and serialization additions do not change report/repair formats.
-- Keep an explicit compatibility boundary: future migrations require review.
-- Recognize their schema versions while retaining every sealed/source/checksum
-- admission condition and all existing maintenance/repair role authorization.
ALTER TABLE reconciliation_repair_plan_seals
    DROP CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check;
ALTER TABLE reconciliation_repair_plan_seals
    ADD CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check
    CHECK (report_migration_version IN (17,18,19,20,21,22,23));

CREATE OR REPLACE FUNCTION nmcp_read_repairable_findings(
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
          AND report.migration_version IN (17,18,19,20,21,22,23)
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

-- Preserve the original definer owner, closed search path and existing ACL.
DO $$
DECLARE target_schema text:=current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer) OWNER TO nmcp_repair_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer) SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
END;
$$;
