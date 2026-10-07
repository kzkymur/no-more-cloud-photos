-- A Media has exactly one physical purge tombstone, produced only after its
-- guarded deletion in the finalizer transaction.

LOCK TABLE media,change_events IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM change_events
        WHERE event_type='media_purged' AND reason='physical_purge'
        GROUP BY media_id HAVING count(*)>1
    ) THEN
        RAISE EXCEPTION 'duplicate physical purge tombstone history'
            USING ERRCODE='23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM change_events AS e JOIN media AS m ON m.id=e.media_id
        WHERE e.event_type='media_purged' AND e.reason='physical_purge'
    ) THEN
        RAISE EXCEPTION 'physical purge tombstone exists for live Media'
            USING ERRCODE='23514';
    END IF;
END;
$$;

CREATE UNIQUE INDEX change_events_one_physical_purge_per_media
ON change_events (media_id)
WHERE event_type='media_purged' AND reason='physical_purge';

CREATE FUNCTION nmcp_guard_physical_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.event_type='media_purged' AND NEW.reason='physical_purge' THEN
        IF current_setting('transaction_isolation') <> 'read committed' THEN
            RAISE EXCEPTION 'physical purge tombstones require read committed isolation'
                USING ERRCODE='25001';
        END IF;
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(NEW.media_id::text,5849403793557313876));
        IF EXISTS (SELECT 1 FROM media WHERE id=NEW.media_id) THEN
            RAISE EXCEPTION 'physical purge tombstone requires absent Media'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER change_events_physical_purge_guard
BEFORE INSERT OR UPDATE ON change_events
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_physical_purge_tombstone();

CREATE FUNCTION nmcp_guard_media_without_purge_tombstone()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'Media creation requires read committed isolation'
            USING ERRCODE='25001';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(NEW.id::text,5849403793557313876));
    IF EXISTS (
        SELECT 1 FROM change_events
        WHERE media_id=NEW.id AND event_type='media_purged' AND reason='physical_purge'
    ) THEN
        RAISE EXCEPTION 'Media cannot coexist with a physical purge tombstone'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER media_physical_purge_guard
BEFORE INSERT OR UPDATE OF id ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_guard_media_without_purge_tombstone();

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_physical_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',
        target_schema,target_schema
    );
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_guard_media_without_purge_tombstone() SET search_path = %I, pg_catalog, pg_temp',
        target_schema,target_schema
    );
END;
$$;
