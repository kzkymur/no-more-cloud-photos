-- Exact durable membership closes UUID/high-water races: a later insert with a
-- smaller random UUID is not silently pulled into an already-started batch.
-- Effects, receipt, aggregate counters, checkpoint and audit share one commit.
CREATE TABLE admin_batch_items (
    batch_id nmcp_uuid_v4 NOT NULL REFERENCES admin_batches(id) ON DELETE RESTRICT,
    ordinal bigint NOT NULL CHECK (ordinal > 0),
    media_id nmcp_uuid_v4 NOT NULL,
    source_job_id nmcp_uuid_v4,
    result_job_id nmcp_uuid_v4,
    outcome text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending','succeeded','failed')),
    error_code text,
    completed_at timestamptz,
    PRIMARY KEY (batch_id, ordinal),
    CHECK ((outcome='pending' AND completed_at IS NULL AND error_code IS NULL)
        OR (outcome='succeeded' AND completed_at IS NOT NULL AND error_code IS NULL)
        OR (outcome='failed' AND completed_at IS NOT NULL AND error_code IS NOT NULL))
);
CREATE UNIQUE INDEX admin_batch_items_media_idx ON admin_batch_items(batch_id,media_id)
    WHERE source_job_id IS NULL;
CREATE UNIQUE INDEX admin_batch_items_job_idx ON admin_batch_items(batch_id,source_job_id)
    WHERE source_job_id IS NOT NULL;
CREATE INDEX admin_batch_items_pending_idx ON admin_batch_items(batch_id,ordinal) WHERE outcome='pending';

CREATE FUNCTION nmcp_admin_batch_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
       OR NEW.media_id IS DISTINCT FROM OLD.media_id OR NEW.source_job_id IS DISTINCT FROM OLD.source_job_id
       OR OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'batch membership and completed receipts are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER admin_batch_receipt_guard BEFORE UPDATE ON admin_batch_items
    FOR EACH ROW EXECUTE FUNCTION nmcp_admin_batch_receipt_guard();
CREATE TRIGGER admin_batch_items_no_delete BEFORE DELETE ON admin_batch_items
    FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER admin_batch_items_no_truncate BEFORE TRUNCATE ON admin_batch_items
    FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();
REVOKE ALL ON admin_batch_items FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
REVOKE ALL ON FUNCTION nmcp_admin_batch_receipt_guard() FROM PUBLIC;

-- Filesystem cleanup services own recoverable transactions. Administrative
-- authorization must survive before invoking them; a receipt plus result audit
-- can then be recovered from their exact native deletion journal after a crash.
CREATE TABLE admin_cleanup_runs (
    id nmcp_uuid_v4 PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('media','renditions')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz
);
CREATE UNIQUE INDEX admin_cleanup_one_running_kind_idx ON admin_cleanup_runs(kind) WHERE finished_at IS NULL;
CREATE TABLE admin_cleanup_items (
    run_id nmcp_uuid_v4 NOT NULL REFERENCES admin_cleanup_runs(id) ON DELETE RESTRICT,
    object_id nmcp_uuid_v4 NOT NULL,
    media_id nmcp_uuid_v4 NOT NULL,
    job_id nmcp_uuid_v4,
    outcome text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending','completed','skipped')),
    completed_at timestamptz,
    PRIMARY KEY(run_id,object_id),
    CHECK ((outcome='pending' AND completed_at IS NULL) OR (outcome<>'pending' AND completed_at IS NOT NULL))
);
CREATE FUNCTION nmcp_admin_cleanup_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.object_id IS DISTINCT FROM OLD.object_id
       OR NEW.media_id IS DISTINCT FROM OLD.media_id OR OLD.outcome <> 'pending'
       OR (OLD.job_id IS NOT NULL AND NEW.job_id IS DISTINCT FROM OLD.job_id) THEN
        RAISE EXCEPTION 'cleanup authorization and completed receipts are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER admin_cleanup_receipt_guard BEFORE UPDATE ON admin_cleanup_items
    FOR EACH ROW EXECUTE FUNCTION nmcp_admin_cleanup_receipt_guard();
CREATE TRIGGER admin_cleanup_items_no_delete BEFORE DELETE ON admin_cleanup_items
    FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER admin_cleanup_items_no_truncate BEFORE TRUNCATE ON admin_cleanup_items
    FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();
REVOKE ALL ON admin_cleanup_runs,admin_cleanup_items
    FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime,nmcp_check_runtime,nmcp_repair_runtime;
REVOKE ALL ON FUNCTION nmcp_admin_cleanup_receipt_guard() FROM PUBLIC;

-- This migration adds only administrative journals, not new check/repair
-- evidence formats. Preserve exact schema identity validation and explicitly
-- admit version 20 in both preview admission and immutable plan evidence.
ALTER TABLE reconciliation_repair_plan_seals
    DROP CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check;
ALTER TABLE reconciliation_repair_plan_seals
    ADD CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check
    CHECK (report_migration_version IN (17,18,19,20));

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
          AND report.migration_version IN (17,18,19,20)
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
