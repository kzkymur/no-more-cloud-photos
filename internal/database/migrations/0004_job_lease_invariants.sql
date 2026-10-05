-- Prevent durable queued jobs that can never satisfy the Worker claim budget,
-- and support bounded expired-lease scans without walking job history.

ALTER TABLE jobs
    ADD CONSTRAINT jobs_queued_attempt_budget_check
    CHECK (status <> 'queued' OR attempts < max_attempts)
    NOT VALID;

ALTER TABLE jobs
    VALIDATE CONSTRAINT jobs_queued_attempt_budget_check;

CREATE INDEX jobs_expired_lease_idx
    ON jobs (lease_expires_at ASC, id ASC)
    WHERE status = 'running';
