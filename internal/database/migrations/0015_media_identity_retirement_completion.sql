-- A Media DELETE only reserves the identity as purging. It may commit solely as
-- the canonical finalizer transaction: Media absent, one null-payload physical
-- tombstone, and the purge Job terminal-successful.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='nmcp_purge_function_owner' AND NOT rolcanlogin)
       OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_purge_function_owner','MEMBER') THEN
        RAISE EXCEPTION 'purge function owner role topology is unsafe';
    END IF;
END;
$$;

LOCK TABLE media,change_events IN SHARE MODE;

ALTER TABLE media_purge_identity_guard
ADD COLUMN purge_job_id nmcp_uuid_v4 REFERENCES jobs(id) ON DELETE RESTRICT;

UPDATE media_purge_identity_guard AS g
SET purge_job_id=(
    SELECT j.id FROM jobs AS j
    WHERE j.type='purge' AND j.media_id_snapshot=g.media_id AND j.status='succeeded'
      AND (SELECT count(*) FROM jobs AS c
           WHERE c.type='purge' AND c.media_id_snapshot=g.media_id AND c.status='succeeded')=1
    LIMIT 1
)
WHERE g.state='purged';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM media_purge_identity_guard WHERE state='purging') THEN
        RAISE EXCEPTION 'cannot enforce identity retirement with stranded purging rows';
    END IF;
    IF EXISTS (
        SELECT 1 FROM media_purge_identity_guard AS g
        WHERE g.state='purged' AND (
            EXISTS (SELECT 1 FROM media WHERE id=g.media_id)
            OR (SELECT count(*) FROM change_events
                WHERE media_id=g.media_id AND event_type='media_purged' AND reason='physical_purge' AND payload IS NULL) <> 1
            OR EXISTS (SELECT 1 FROM change_events
                WHERE media_id=g.media_id AND event_type='media_purged' AND reason='physical_purge' AND payload IS NOT NULL)
            OR g.purge_job_id IS NULL
            OR NOT EXISTS (SELECT 1 FROM jobs
                WHERE id=g.purge_job_id AND type='purge' AND media_id_snapshot=g.media_id AND status='succeeded'
                  AND lease_token IS NULL AND lease_expires_at IS NULL AND finished_at IS NOT NULL)
        )
    ) THEN
        RAISE EXCEPTION 'cannot enforce identity retirement with noncanonical purged rows';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_media_without_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE identity_state text;
BEGIN
    IF TG_OP='DELETE' THEN
        SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=OLD.id FOR UPDATE;
        IF identity_state <> 'live' THEN
            RAISE EXCEPTION 'Media identity is not live: %',OLD.id USING ERRCODE='23514';
        END IF;
        UPDATE media_purge_identity_guard
        SET state='purging',purge_job_id=current_setting('nmcp.purge_job_id')::nmcp_uuid_v4
        WHERE media_id=OLD.id;
        RETURN OLD;
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Media identity is immutable: %',OLD.id USING ERRCODE='23514';
    END IF;
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'Media creation requires read committed isolation' USING ERRCODE='25001';
    END IF;
    INSERT INTO media_purge_identity_guard(media_id,state,purge_job_id) VALUES (NEW.id,'live',NULL)
    ON CONFLICT (media_id) DO NOTHING RETURNING state INTO identity_state;
    IF NOT FOUND THEN
        SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=NEW.id FOR UPDATE;
        RAISE EXCEPTION 'Media identity has already been reserved: %',NEW.id USING ERRCODE='23505';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION nmcp_require_completed_media_identity_retirement()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE current_state text; retiring_job_id nmcp_uuid_v4;
BEGIN
    SELECT state,purge_job_id INTO STRICT current_state,retiring_job_id
    FROM media_purge_identity_guard WHERE media_id=NEW.media_id;
    IF current_state='purging' THEN
        RAISE EXCEPTION 'Media identity retirement is incomplete: %',NEW.media_id USING ERRCODE='23514';
    END IF;
    IF current_state='purged' AND (
        EXISTS (SELECT 1 FROM media WHERE id=NEW.media_id)
        OR (SELECT count(*) FROM change_events
            WHERE media_id=NEW.media_id AND event_type='media_purged' AND reason='physical_purge' AND payload IS NULL) <> 1
        OR EXISTS (SELECT 1 FROM change_events
            WHERE media_id=NEW.media_id AND event_type='media_purged' AND reason='physical_purge' AND payload IS NOT NULL)
        OR retiring_job_id IS NULL
        OR NOT EXISTS (SELECT 1 FROM jobs
            WHERE id=retiring_job_id AND type='purge' AND media_id_snapshot=NEW.media_id AND status='succeeded'
              AND lease_token IS NULL AND lease_expires_at IS NULL AND finished_at IS NOT NULL)
    ) THEN
        RAISE EXCEPTION 'Media identity retirement lacks canonical finalizer state: %',NEW.media_id USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER media_identity_retirement_completed
AFTER INSERT OR UPDATE ON media_purge_identity_guard
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION nmcp_require_completed_media_identity_retirement();

GRANT SELECT ON TABLE change_events TO nmcp_purge_function_owner;
REVOKE ALL ON FUNCTION nmcp_require_completed_media_identity_retirement() FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_media_without_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_require_completed_media_identity_retirement() OWNER TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_require_completed_media_identity_retirement() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_purge_function_owner',target_schema);
END;
$$;
