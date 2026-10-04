-- Tighten upload MIME invariants without rewriting the immutable initial
-- schema. Existing valid rows are preserved; incompatible rows stop upgrade.

-- Take the strongest lock needed by the later DDL up front, in parent-before-child
-- order. ACCESS EXCLUSIVE conflicts with legacy INSERT/UPDATE/DELETE writers, and
-- the transactional migrator holds both locks through preflight and installation.
LOCK TABLE media, originals IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    invalid_media record;
BEGIN
    SELECT m.id, m.media_type, o.mime_type AS original_mime_type
    INTO invalid_media
    FROM media AS m
    LEFT JOIN originals AS o ON o.media_id = m.id
    WHERE NOT nmcp_is_mime_type(m.media_type)
       OR (o.media_id IS NOT NULL AND o.mime_type IS DISTINCT FROM m.media_type)
    ORDER BY m.id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'existing Media/Original MIME values are incompatible: media %, media type %, original type %',
            invalid_media.id, invalid_media.media_type, invalid_media.original_mime_type
            USING ERRCODE = '23514';
    END IF;
END;
$$;

ALTER TABLE media
ADD CONSTRAINT media_media_type_mime_check
CHECK (nmcp_is_mime_type(media_type)) NOT VALID;
ALTER TABLE media VALIDATE CONSTRAINT media_media_type_mime_check;

CREATE FUNCTION nmcp_validate_original_media_mime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_mime_type text;
BEGIN
    SELECT media_type INTO expected_mime_type
    FROM media
    WHERE id = NEW.media_id
    FOR UPDATE;
    IF FOUND AND NEW.mime_type IS DISTINCT FROM expected_mime_type THEN
        RAISE EXCEPTION 'original MIME type % does not match owning Media MIME type %',
            NEW.mime_type, expected_mime_type
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER originals_media_mime_validate
BEFORE INSERT ON originals
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_original_media_mime();

CREATE FUNCTION nmcp_validate_media_original_mime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.media_type IS DISTINCT FROM OLD.media_type
       AND EXISTS (
           SELECT 1 FROM originals
           WHERE media_id = NEW.id AND mime_type IS DISTINCT FROM NEW.media_type
       ) THEN
        RAISE EXCEPTION 'Media MIME type does not match its immutable Original'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER media_original_mime_validate
BEFORE UPDATE OF media_type ON media
FOR EACH ROW EXECUTE FUNCTION nmcp_validate_media_original_mime();

DO $$
DECLARE
    target_schema text := current_schema();
    function_name text;
BEGIN
    FOREACH function_name IN ARRAY ARRAY[
        'nmcp_validate_original_media_mime()',
        'nmcp_validate_media_original_mime()'
    ] LOOP
        EXECUTE format(
            'ALTER FUNCTION %I.%s SET search_path = %I, pg_catalog, pg_temp',
            target_schema, function_name, target_schema
        );
    END LOOP;
END;
$$;
