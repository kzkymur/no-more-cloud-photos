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
