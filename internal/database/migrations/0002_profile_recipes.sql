-- Candidate media processor registry and strict profile recipe schema.  All
-- bundled capabilities remain uncertified until later codec fixture evidence
-- and an explicit activation operation establish deployable support.

DO $$
DECLARE
    target_schema text := current_schema();
BEGIN
    PERFORM pg_catalog.set_config(
        'search_path',
        pg_catalog.quote_ident(target_schema) || ', pg_catalog, pg_temp',
        true
    );
END;
$$;

CREATE FUNCTION nmcp_candidate_source_mode(value text)
RETURNS text
LANGUAGE sql
IMMUTABLE
STRICT
RETURN CASE value
    WHEN 'image/gif' THEN 'probe-animation'
    WHEN 'image/webp' THEN 'probe-animation'
    WHEN 'video/mp4' THEN 'video'
    WHEN 'video/quicktime' THEN 'video'
    WHEN 'image/bmp' THEN 'still'
    WHEN 'image/dng' THEN 'still'
    WHEN 'image/heic' THEN 'still'
    WHEN 'image/heif' THEN 'still'
    WHEN 'image/jpeg' THEN 'still'
    WHEN 'image/png' THEN 'still'
    WHEN 'image/x-canon-cr2' THEN 'still'
    WHEN 'image/x-canon-cr3' THEN 'still'
    WHEN 'image/x-fuji-raf' THEN 'still'
    WHEN 'image/x-nikon-nef' THEN 'still'
    WHEN 'image/x-olympus-orf' THEN 'still'
    WHEN 'image/x-panasonic-rw2' THEN 'still'
    WHEN 'image/x-sony-arw' THEN 'still'
    ELSE NULL
END;

CREATE TABLE profile_processor_capabilities (
    processor text NOT NULL CHECK (processor ~ '^[a-z][a-z0-9_-]{0,63}$'),
    parameters_schema_version integer NOT NULL CHECK (parameters_schema_version >= 1),
    input_mime_type text NOT NULL CHECK (nmcp_is_mime_type(input_mime_type)),
    source_mode text NOT NULL CHECK (source_mode IN ('still', 'probe-animation', 'video')),
    evidence text NOT NULL CHECK (evidence <> ''),
    PRIMARY KEY (processor, parameters_schema_version, input_mime_type, source_mode),
    CONSTRAINT profile_processor_capabilities_closed_registry_check CHECK (
        processor = 'nmcp-media'
        AND parameters_schema_version = 1
        AND nmcp_candidate_source_mode(input_mime_type) IS NOT NULL
        AND source_mode = nmcp_candidate_source_mode(input_mime_type)
    )
);
CREATE TRIGGER profile_processor_capabilities_no_update
BEFORE UPDATE ON profile_processor_capabilities
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER profile_processor_capabilities_no_delete
BEFORE DELETE ON profile_processor_capabilities
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER profile_processor_capabilities_no_truncate
BEFORE TRUNCATE ON profile_processor_capabilities
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

INSERT INTO profile_processor_capabilities (
    processor, parameters_schema_version, input_mime_type, source_mode, evidence
) VALUES
    ('nmcp-media', 1, 'image/bmp', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/dng', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/gif', 'probe-animation', 'candidate; content probe and codec fixture evidence pending #7/#12'),
    ('nmcp-media', 1, 'image/heic', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/heif', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/jpeg', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/png', 'still', 'candidate; detector and codec fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/webp', 'probe-animation', 'candidate; content probe and codec fixture evidence pending #7/#11/#12'),
    ('nmcp-media', 1, 'image/x-canon-cr2', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-canon-cr3', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-fuji-raf', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-nikon-nef', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-olympus-orf', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-panasonic-rw2', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'image/x-sony-arw', 'still', 'candidate alias; detector and RAW fixture evidence pending #7/#11'),
    ('nmcp-media', 1, 'video/mp4', 'video', 'candidate; stream probe and codec fixture evidence pending #7/#13'),
    ('nmcp-media', 1, 'video/quicktime', 'video', 'candidate; stream probe and codec fixture evidence pending #7/#13');

CREATE FUNCTION nmcp_output_kind_allowed(source_mode text, output_kind text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN (source_mode = 'still' AND output_kind = 'still-avif')
    OR (source_mode = 'probe-animation' AND output_kind IN ('still-avif','animation-webp'))
    OR (source_mode = 'video' AND output_kind IN ('still-avif','video-av1'));

CREATE TABLE profile_processor_certifications (
    id nmcp_uuid_v4 PRIMARY KEY,
    processor text NOT NULL,
    parameters_schema_version integer NOT NULL,
    input_mime_type text NOT NULL,
    source_mode text NOT NULL,
    output_kind text NOT NULL CHECK (output_kind IN ('still-avif','animation-webp','video-av1')),
    max_long_edge integer NOT NULL CHECK (max_long_edge > 0),
    minimum_setting integer NOT NULL,
    maximum_setting integer NOT NULL CHECK (maximum_setting >= minimum_setting),
    evidence text NOT NULL CHECK (
        btrim(evidence, E' \t\n\r\f\x0B') <> ''
        AND btrim(evidence, E' \t\n\r\f\x0B') <> 'provisional-unverified'
    ),
    certified_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    FOREIGN KEY (processor, parameters_schema_version, input_mime_type, source_mode)
        REFERENCES profile_processor_capabilities (
            processor, parameters_schema_version, input_mime_type, source_mode
        ) ON DELETE RESTRICT,
    CONSTRAINT profile_processor_certifications_option_check CHECK (
        nmcp_output_kind_allowed(source_mode, output_kind)
        AND ((output_kind IN ('still-avif','animation-webp')
              AND minimum_setting >= 1 AND maximum_setting <= 100)
             OR (output_kind = 'video-av1'
                 AND minimum_setting >= 0 AND maximum_setting <= 63))
    )
);
CREATE INDEX profile_processor_certifications_lookup_idx ON profile_processor_certifications (
    processor, parameters_schema_version, input_mime_type, source_mode, output_kind,
    max_long_edge, minimum_setting, maximum_setting
);
CREATE FUNCTION nmcp_profile_certification_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.certified_at := statement_timestamp();
    RETURN NEW;
END;
$$;
CREATE TRIGGER profile_processor_certifications_insert
BEFORE INSERT ON profile_processor_certifications
FOR EACH ROW EXECUTE FUNCTION nmcp_profile_certification_insert();
CREATE TRIGGER profile_processor_certifications_no_update
BEFORE UPDATE ON profile_processor_certifications
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER profile_processor_certifications_no_delete
BEFORE DELETE ON profile_processor_certifications
FOR EACH ROW EXECUTE FUNCTION nmcp_reject_mutation();
CREATE TRIGGER profile_processor_certifications_no_truncate
BEFORE TRUNCATE ON profile_processor_certifications
FOR EACH STATEMENT EXECUTE FUNCTION nmcp_reject_mutation();

CREATE FUNCTION nmcp_jsonb_exact_keys(value jsonb, expected text[])
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
BEGIN
    IF jsonb_typeof(value) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
    RETURN (SELECT count(*) FROM jsonb_object_keys(value)) = cardinality(expected)
       AND NOT EXISTS (
           SELECT 1 FROM jsonb_object_keys(value) AS actual(key)
           WHERE NOT (actual.key = ANY(expected))
       )
       AND NOT EXISTS (
           SELECT 1 FROM unnest(expected) AS required(key)
           WHERE NOT (value ? required.key)
       );
END;
$$;

CREATE FUNCTION nmcp_jsonb_integer_between(value jsonb, minimum numeric, maximum numeric)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    encoded text;
BEGIN
    IF jsonb_typeof(value) IS DISTINCT FROM 'number' THEN RETURN false; END IF;
    encoded := value #>> '{}';
    IF encoded !~ '^-?[0-9]+$' THEN RETURN false; END IF;
    RETURN encoded::numeric BETWEEN minimum AND maximum;
END;
$$;

CREATE FUNCTION nmcp_valid_still_output(value jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN COALESCE(nmcp_jsonb_exact_keys(value, ARRAY['format','quality','bit_depth']), false)
   AND value->>'format' IS NOT DISTINCT FROM 'avif'
   AND COALESCE(nmcp_jsonb_integer_between(value->'quality', 1, 100), false)
   AND COALESCE(nmcp_jsonb_integer_between(value->'bit_depth', 8, 8), false);

CREATE FUNCTION nmcp_valid_animation_output(value jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN COALESCE(nmcp_jsonb_exact_keys(value, ARRAY['format','quality','bit_depth']), false)
   AND value->>'format' IS NOT DISTINCT FROM 'animated-webp'
   AND COALESCE(nmcp_jsonb_integer_between(value->'quality', 1, 100), false)
   AND COALESCE(nmcp_jsonb_integer_between(value->'bit_depth', 8, 8), false);

CREATE FUNCTION nmcp_valid_video_output(value jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
RETURN COALESCE(nmcp_jsonb_exact_keys(value, ARRAY[
       'container','video_codec','crf','bit_depth','chroma','audio_codec','audio_bitrate_kbps'
   ]), false)
   AND value->>'container' IS NOT DISTINCT FROM 'mp4'
   AND value->>'video_codec' IS NOT DISTINCT FROM 'av1'
   AND COALESCE(nmcp_jsonb_integer_between(value->'crf', 0, 63), false)
   AND COALESCE(nmcp_jsonb_integer_between(value->'bit_depth', 10, 10), false)
   AND value->>'chroma' IS NOT DISTINCT FROM '4:2:0'
   AND value->>'audio_codec' IS NOT DISTINCT FROM 'aac'
   AND COALESCE(nmcp_jsonb_integer_between(value->'audio_bitrate_kbps', 128, 128), false);

CREATE FUNCTION nmcp_valid_profile_recipe(recipe jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    source_mode text;
    frame_policy text;
    audio_policy text;
    color_policy text := 'normalize-srgb-tone-map-hdr';
    stream_selection text := 'not-applicable';
    animation_timing text := 'not-applicable';
    animation_loop text := 'not-applicable';
    has_still boolean;
    has_animation boolean;
    has_video boolean;
BEGIN
    IF NOT nmcp_jsonb_exact_keys(recipe, ARRAY[
        'source_mode','frame_policy','max_long_edge','allow_upscale','crop',
        'orientation','color','metadata','alpha','audio','still_output',
        'animation_output','video_output','dimension_rule','stream_selection',
        'animation_timing','animation_loop'
    ]) THEN
        RETURN false;
    END IF;
    IF nmcp_jsonb_integer_between(recipe->'max_long_edge', 1, 2147483647) IS NOT TRUE
       OR recipe->'allow_upscale' IS DISTINCT FROM 'false'::jsonb
       OR recipe->>'crop' IS DISTINCT FROM 'none'
       OR recipe->>'orientation' IS DISTINCT FROM 'apply'
       OR recipe->>'metadata' IS DISTINCT FROM 'strip-after-normalization-keep-color-tags'
       OR recipe->>'alpha' IS DISTINCT FROM 'preserve'
       OR recipe->>'dimension_rule' IS DISTINCT FROM 'preserve-aspect-no-crop-no-upscale-even-round-down' THEN
        RETURN false;
    END IF;

    source_mode := recipe->>'source_mode';
    frame_policy := recipe->>'frame_policy';
    audio_policy := recipe->>'audio';
    has_still := jsonb_typeof(recipe->'still_output') IS DISTINCT FROM 'null';
    has_animation := jsonb_typeof(recipe->'animation_output') IS DISTINCT FROM 'null';
    has_video := jsonb_typeof(recipe->'video_output') IS DISTINCT FROM 'null';

    IF has_still AND NOT nmcp_valid_still_output(recipe->'still_output') THEN RETURN false; END IF;
    IF has_animation AND NOT nmcp_valid_animation_output(recipe->'animation_output') THEN RETURN false; END IF;
    IF has_video AND NOT nmcp_valid_video_output(recipe->'video_output') THEN RETURN false; END IF;

    RETURN COALESCE((source_mode = 'still'
            AND frame_policy = 'first' AND audio_policy = 'none'
            AND recipe->>'color' = 'normalize-srgb-tone-map-hdr'
            AND recipe->>'stream_selection' = 'not-applicable'
            AND recipe->>'animation_timing' = 'not-applicable'
            AND recipe->>'animation_loop' = 'not-applicable'
            AND has_still AND NOT has_animation AND NOT has_video)
        OR (source_mode = 'probe-animation' AND audio_policy = 'none'
            AND recipe->>'color' = 'normalize-srgb-tone-map-hdr'
            AND recipe->>'stream_selection' = 'not-applicable'
            AND has_still AND NOT has_video
            AND ((frame_policy = 'all' AND has_animation
                  AND recipe->>'animation_timing' = 'preserve'
                  AND recipe->>'animation_loop' = 'preserve')
                 OR (frame_policy = 'first' AND NOT has_animation
                     AND recipe->>'animation_timing' = 'first-frame'
                     AND recipe->>'animation_loop' = 'discard')))
        OR (source_mode = 'video'
            AND ((frame_policy = 'all' AND audio_policy = 'aac-if-present'
                  AND recipe->>'color' = 'normalize-bt709-tone-map-hdr'
                  AND recipe->>'stream_selection' = 'primary-video'
                  AND recipe->>'animation_timing' = 'not-applicable'
                  AND recipe->>'animation_loop' = 'not-applicable'
                  AND has_video AND NOT has_still AND NOT has_animation)
                 OR (frame_policy = 'first' AND audio_policy = 'discard'
                     AND recipe->>'color' = 'normalize-srgb-tone-map-hdr'
                     AND recipe->>'stream_selection' = 'primary-video'
                     AND recipe->>'animation_timing' = 'first-frame'
                     AND recipe->>'animation_loop' = 'discard'
                     AND has_still AND NOT has_animation AND NOT has_video))), false);
END;
$$;

CREATE FUNCTION nmcp_profile_recipe_certified(
    profile_processor text, schema_version integer, mime_value text,
    recipe_source_mode text, recipe jsonb
)
RETURNS boolean
LANGUAGE plpgsql
STABLE
STRICT
AS $$
DECLARE
    max_edge integer := (recipe->>'max_long_edge')::integer;
    setting integer;
BEGIN
    IF jsonb_typeof(recipe->'still_output') IS DISTINCT FROM 'null' THEN
        setting := (recipe->'still_output'->>'quality')::integer;
        IF NOT EXISTS (
            SELECT 1 FROM profile_processor_certifications AS certification
            WHERE certification.processor = profile_processor
              AND certification.parameters_schema_version = schema_version
              AND certification.input_mime_type = mime_value
              AND certification.source_mode = recipe_source_mode
              AND certification.output_kind = 'still-avif'
              AND certification.max_long_edge >= max_edge
              AND setting BETWEEN certification.minimum_setting AND certification.maximum_setting
        ) THEN RETURN false; END IF;
    END IF;
    IF jsonb_typeof(recipe->'animation_output') IS DISTINCT FROM 'null' THEN
        setting := (recipe->'animation_output'->>'quality')::integer;
        IF NOT EXISTS (
            SELECT 1 FROM profile_processor_certifications AS certification
            WHERE certification.processor = profile_processor
              AND certification.parameters_schema_version = schema_version
              AND certification.input_mime_type = mime_value
              AND certification.source_mode = recipe_source_mode
              AND certification.output_kind = 'animation-webp'
              AND certification.max_long_edge >= max_edge
              AND setting BETWEEN certification.minimum_setting AND certification.maximum_setting
        ) THEN RETURN false; END IF;
    END IF;
    IF jsonb_typeof(recipe->'video_output') IS DISTINCT FROM 'null' THEN
        setting := (recipe->'video_output'->>'crf')::integer;
        IF NOT EXISTS (
            SELECT 1 FROM profile_processor_certifications AS certification
            WHERE certification.processor = profile_processor
              AND certification.parameters_schema_version = schema_version
              AND certification.input_mime_type = mime_value
              AND certification.source_mode = recipe_source_mode
              AND certification.output_kind = 'video-av1'
              AND certification.max_long_edge >= max_edge
              AND setting BETWEEN certification.minimum_setting AND certification.maximum_setting
        ) THEN RETURN false; END IF;
    END IF;
    RETURN true;
END;
$$;

CREATE FUNCTION nmcp_profile_definition_valid(
    mime_values text[], profile_processor text, schema_version integer,
    profile_parameters jsonb, require_certified boolean
)
RETURNS boolean
LANGUAGE plpgsql
STABLE
STRICT
AS $$
DECLARE
    mime_value text;
    recipe jsonb;
BEGIN
    IF profile_processor IS DISTINCT FROM 'nmcp-media' OR schema_version IS DISTINCT FROM 1
       OR array_ndims(mime_values) IS DISTINCT FROM 1
       OR array_lower(mime_values, 1) IS DISTINCT FROM 1
       OR nmcp_jsonb_exact_keys(profile_parameters, ARRAY['evidence_status','recipes']) IS NOT TRUE
       OR profile_parameters->>'evidence_status' IS DISTINCT FROM 'provisional-unverified' THEN
        RETURN false;
    END IF;
    IF jsonb_typeof(profile_parameters->'recipes') IS DISTINCT FROM 'object' THEN RETURN false; END IF;
    IF (SELECT count(*) FROM jsonb_object_keys(profile_parameters->'recipes')) IS DISTINCT FROM cardinality(mime_values) THEN RETURN false; END IF;

    FOREACH mime_value IN ARRAY mime_values LOOP
        IF (profile_parameters->'recipes' ? mime_value) IS NOT TRUE THEN RETURN false; END IF;
        recipe := profile_parameters->'recipes'->mime_value;
        IF nmcp_valid_profile_recipe(recipe) IS NOT TRUE THEN RETURN false; END IF;
        IF NOT EXISTS (
            SELECT 1 FROM profile_processor_capabilities AS capability
            WHERE capability.processor = profile_processor
              AND capability.parameters_schema_version = schema_version
              AND capability.input_mime_type = mime_value
              AND capability.source_mode = recipe->>'source_mode'
        ) THEN
            RETURN false;
        END IF;
        IF require_certified AND nmcp_profile_recipe_certified(
            profile_processor, schema_version, mime_value, recipe->>'source_mode', recipe
        ) IS NOT TRUE THEN RETURN false; END IF;
    END LOOP;
    RETURN true;
END;
$$;

CREATE FUNCTION nmcp_profile_definition_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT nmcp_profile_definition_valid(
        NEW.input_mime_types, NEW.processor, NEW.parameters_schema_version,
        NEW.parameters, NEW.status = 'active'
    ) THEN
        RAISE EXCEPTION 'invalid or uncertified profile definition'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER profiles_definition_validate
BEFORE INSERT OR UPDATE ON profiles
FOR EACH ROW EXECUTE FUNCTION nmcp_profile_definition_trigger();

DO $$
DECLARE
    invalid_profile record;
BEGIN
    SELECT id, key, version, status INTO invalid_profile
    FROM profiles
    WHERE nmcp_profile_definition_valid(
        input_mime_types, processor, parameters_schema_version,
        parameters, status = 'active'
    ) IS NOT TRUE
    ORDER BY key, version
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'existing profile %/% (%) is incompatible with profile schema v1',
            invalid_profile.key, invalid_profile.version, invalid_profile.status
            USING ERRCODE = '23514';
    END IF;
END;
$$;

CREATE FUNCTION nmcp_bundled_profile_recipe(profile_key text, source_mode text)
RETURNS jsonb
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    is_standard boolean := profile_key = 'standard';
    quality integer := CASE WHEN profile_key = 'standard' THEN 60 ELSE 50 END;
    max_edge integer := CASE WHEN profile_key = 'standard' THEN 1920 ELSE 640 END;
    frame_policy text;
    audio_policy text;
    color_policy text := 'normalize-srgb-tone-map-hdr';
    stream_selection text := 'not-applicable';
    animation_timing text := 'not-applicable';
    animation_loop text := 'not-applicable';
    still_output jsonb := jsonb_build_object('format','avif','quality',quality,'bit_depth',8);
    animation_output jsonb := 'null'::jsonb;
    video_output jsonb := 'null'::jsonb;
BEGIN
    IF source_mode = 'still' THEN
        frame_policy := 'first'; audio_policy := 'none';
    ELSIF source_mode = 'probe-animation' THEN
        frame_policy := CASE WHEN is_standard THEN 'all' ELSE 'first' END;
        audio_policy := 'none';
        IF is_standard THEN
            animation_timing := 'preserve'; animation_loop := 'preserve';
            animation_output := jsonb_build_object('format','animated-webp','quality',80,'bit_depth',8);
        ELSE
            animation_timing := 'first-frame'; animation_loop := 'discard';
        END IF;
    ELSIF source_mode = 'video' AND is_standard THEN
        frame_policy := 'all'; audio_policy := 'aac-if-present'; still_output := 'null'::jsonb;
        color_policy := 'normalize-bt709-tone-map-hdr'; stream_selection := 'primary-video';
        video_output := jsonb_build_object(
            'container','mp4','video_codec','av1','crf',32,'bit_depth',10,
            'chroma','4:2:0','audio_codec','aac','audio_bitrate_kbps',128
        );
    ELSIF source_mode = 'video' THEN
        frame_policy := 'first'; audio_policy := 'discard';
        stream_selection := 'primary-video'; animation_timing := 'first-frame'; animation_loop := 'discard';
    ELSE
        RAISE EXCEPTION 'unknown bundled source mode: %', source_mode;
    END IF;
    RETURN jsonb_build_object(
        'source_mode',source_mode,'frame_policy',frame_policy,'max_long_edge',max_edge,
        'allow_upscale',false,'crop','none','orientation','apply',
        'color',color_policy,'metadata','strip-after-normalization-keep-color-tags',
        'alpha','preserve','audio',audio_policy,'still_output',still_output,
        'animation_output',animation_output,'video_output',video_output,
        'dimension_rule','preserve-aspect-no-crop-no-upscale-even-round-down',
        'stream_selection',stream_selection,'animation_timing',animation_timing,
        'animation_loop',animation_loop
    );
END;
$$;

WITH bundled(id, key) AS (
    VALUES
        ('60000000-0000-4000-8000-000000000001'::uuid, 'standard'::text),
        ('60000000-0000-4000-8000-000000000002'::uuid, 'thumbnail'::text)
), definitions AS (
    SELECT bundled.id, bundled.key,
           array_agg(capability.input_mime_type ORDER BY capability.input_mime_type) AS mime_types,
           jsonb_object_agg(
               capability.input_mime_type,
               nmcp_bundled_profile_recipe(bundled.key, capability.source_mode)
               ORDER BY capability.input_mime_type
           ) AS recipes
    FROM bundled CROSS JOIN profile_processor_capabilities AS capability
    WHERE capability.processor = 'nmcp-media' AND capability.parameters_schema_version = 1
    GROUP BY bundled.id, bundled.key
)
INSERT INTO profiles (
    id, key, version, status, input_mime_types, processor,
    parameters_schema_version, parameters
)
SELECT id, key, 1, 'draft', mime_types, 'nmcp-media', 1,
       jsonb_build_object('evidence_status','provisional-unverified','recipes',recipes)
FROM definitions;

DROP FUNCTION nmcp_bundled_profile_recipe(text, text);

-- Pin every new function to the migration target schema and trusted catalogs.
DO $$
DECLARE
    target_schema text := current_schema();
    function_name text;
BEGIN
    FOREACH function_name IN ARRAY ARRAY[
        'nmcp_candidate_source_mode(text)',
        'nmcp_output_kind_allowed(text,text)',
        'nmcp_profile_certification_insert()',
        'nmcp_jsonb_exact_keys(jsonb,text[])',
        'nmcp_jsonb_integer_between(jsonb,numeric,numeric)',
        'nmcp_valid_still_output(jsonb)',
        'nmcp_valid_animation_output(jsonb)',
        'nmcp_valid_video_output(jsonb)',
        'nmcp_valid_profile_recipe(jsonb)',
        'nmcp_profile_recipe_certified(text,integer,text,text,jsonb)',
        'nmcp_profile_definition_valid(text[],text,integer,jsonb,boolean)',
        'nmcp_profile_definition_trigger()'
    ] LOOP
        EXECUTE format(
            'ALTER FUNCTION %I.%s SET search_path = %I, pg_catalog, pg_temp',
            target_schema, function_name, target_schema
        );
    END LOOP;
END;
$$;
