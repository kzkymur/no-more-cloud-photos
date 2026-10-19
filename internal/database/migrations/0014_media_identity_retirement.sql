-- Retire a Media identity inside the authorized delete transaction, before its
-- physical tombstone is written. A rollback restores live; a committed purge
-- advances purging to purged. Media creation requires a brand-new guard row.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='nmcp_purge_function_owner' AND NOT rolcanlogin)
       OR pg_catalog.pg_has_role('nmcp_runtime','nmcp_purge_function_owner','MEMBER')
       OR pg_catalog.pg_has_role('nmcp_worker_runtime','nmcp_purge_function_owner','MEMBER') THEN
        RAISE EXCEPTION 'purge function owner role topology is unsafe';
    END IF;
END;
$$;

-- Drain Media/event writers before ALTER TABLE takes its stronger guard-table
-- lock. Existing trigger readers remain compatible and can finish first.
LOCK TABLE media,change_events IN SHARE MODE;

ALTER TABLE media_purge_identity_guard DROP CONSTRAINT media_purge_identity_guard_state_check;
ALTER TABLE media_purge_identity_guard ADD CONSTRAINT media_purge_identity_guard_state_check
CHECK (state IN ('live','purging','purged'));

CREATE OR REPLACE FUNCTION nmcp_guard_media_without_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE identity_state text;
BEGIN
    IF TG_OP='DELETE' THEN
        SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=OLD.id FOR UPDATE;
        IF identity_state <> 'live' THEN
            RAISE EXCEPTION 'Media identity is not live: %',OLD.id USING ERRCODE='23514';
        END IF;
        UPDATE media_purge_identity_guard SET state='purging' WHERE media_id=OLD.id;
        RETURN OLD;
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Media identity is immutable: %',OLD.id USING ERRCODE='23514';
    END IF;
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'Media creation requires read committed isolation' USING ERRCODE='25001';
    END IF;
    INSERT INTO media_purge_identity_guard(media_id,state) VALUES (NEW.id,'live')
    ON CONFLICT (media_id) DO NOTHING RETURNING state INTO identity_state;
    IF NOT FOUND THEN
        SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=NEW.id FOR UPDATE;
        RAISE EXCEPTION 'Media identity has already been reserved: %',NEW.id USING ERRCODE='23505';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_physical_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE identity_state text;
BEGIN
    IF NEW.event_type='media_purged' AND NEW.reason='physical_purge' THEN
        IF current_setting('transaction_isolation') <> 'read committed' THEN
            RAISE EXCEPTION 'physical purge tombstones require read committed isolation' USING ERRCODE='25001';
        END IF;
        SELECT state INTO identity_state FROM media_purge_identity_guard WHERE media_id=NEW.media_id FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'physical purge tombstone requires reserved Media identity' USING ERRCODE='23514';
        END IF;
        IF identity_state='purged' THEN
            RAISE EXCEPTION 'physical purge tombstone already exists' USING ERRCODE='23505';
        END IF;
        IF identity_state <> 'purging' THEN
            RAISE EXCEPTION 'physical purge tombstone requires retiring Media identity' USING ERRCODE='23514';
        END IF;
        IF EXISTS (SELECT 1 FROM media WHERE id=NEW.media_id) THEN
            RAISE EXCEPTION 'physical purge tombstone requires absent Media' USING ERRCODE='23514';
        END IF;
        UPDATE media_purge_identity_guard SET state='purged' WHERE media_id=NEW.media_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER media_physical_purge_delete_guard
BEFORE DELETE ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_media_without_purge_tombstone();

GRANT SELECT,INSERT,UPDATE ON TABLE media_purge_identity_guard TO nmcp_purge_function_owner;
REVOKE ALL ON FUNCTION nmcp_guard_media_without_purge_tombstone() FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;
REVOKE ALL ON FUNCTION nmcp_guard_physical_purge_tombstone() FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_media_without_purge_tombstone() OWNER TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_physical_purge_tombstone() OWNER TO nmcp_purge_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_physical_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_media_without_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_purge_function_owner',target_schema);
END;
$$;
