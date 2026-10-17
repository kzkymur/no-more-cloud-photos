-- Serialize Media creation and physical tombstones through one durable identity
-- row. The visible-duplicate check keeps finalizer Media->guard lock order from
-- cycling with a same-ID INSERT; absent-ID races serialize on the guard row.

-- SHARE rejects concurrent writes while allowing the guard functions' source
-- reads to finish, avoiding a cutover relation-lock cycle with an in-flight
-- finalizer or direct tombstone insert.
LOCK TABLE media,change_events IN SHARE MODE;

CREATE TABLE media_purge_identity_guard (
    media_id nmcp_uuid_v4 PRIMARY KEY,
    state text NOT NULL CHECK (state IN ('live','purged'))
);

INSERT INTO media_purge_identity_guard (media_id,state)
SELECT id,'live' FROM media;
INSERT INTO media_purge_identity_guard (media_id,state)
SELECT media_id,'purged' FROM change_events
WHERE event_type='media_purged' AND reason='physical_purge';

REVOKE ALL ON TABLE media_purge_identity_guard FROM PUBLIC,nmcp_runtime,nmcp_worker_runtime;

CREATE OR REPLACE FUNCTION nmcp_guard_physical_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE identity_state text;
BEGIN
    IF NEW.event_type='media_purged' AND NEW.reason='physical_purge' THEN
        IF current_setting('transaction_isolation') <> 'read committed' THEN
            RAISE EXCEPTION 'physical purge tombstones require read committed isolation' USING ERRCODE='25001';
        END IF;
        INSERT INTO media_purge_identity_guard(media_id,state) VALUES (NEW.media_id,'purged')
        ON CONFLICT (media_id) DO NOTHING RETURNING state INTO identity_state;
        IF NOT FOUND THEN
            SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=NEW.media_id FOR UPDATE;
            IF identity_state='purged' THEN
                RAISE EXCEPTION 'physical purge tombstone already exists' USING ERRCODE='23505';
            END IF;
        END IF;
        IF EXISTS (SELECT 1 FROM media WHERE id=NEW.media_id) THEN
            RAISE EXCEPTION 'physical purge tombstone requires absent Media' USING ERRCODE='23514';
        END IF;
        UPDATE media_purge_identity_guard SET state='purged' WHERE media_id=NEW.media_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_guard_media_without_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE identity_state text;
BEGIN
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Media identity is immutable: %',OLD.id USING ERRCODE='23514';
    END IF;
    IF EXISTS (SELECT 1 FROM media WHERE id=NEW.id) THEN
        RAISE EXCEPTION 'Media identity already exists: %',NEW.id USING ERRCODE='23505';
    END IF;
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'Media creation requires read committed isolation' USING ERRCODE='25001';
    END IF;
    INSERT INTO media_purge_identity_guard(media_id,state) VALUES (NEW.id,'live')
    ON CONFLICT (media_id) DO NOTHING;
    SELECT state INTO STRICT identity_state FROM media_purge_identity_guard WHERE media_id=NEW.id FOR UPDATE;
    IF identity_state='purged' THEN
        RAISE EXCEPTION 'Media cannot coexist with a physical purge tombstone' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_physical_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_guard_media_without_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',target_schema,target_schema);
END;
$$;
