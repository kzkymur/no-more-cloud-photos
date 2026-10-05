-- Correct the pre-activation bundled profile geometry declaration. Still and
-- animation processors round aspect dimensions to nearest and preserve odd or
-- one-pixel axes; only video requires even round-down dimensions.

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    PERFORM pg_catalog.set_config(
        'search_path', pg_catalog.quote_ident(target_schema) || ', pg_catalog, pg_temp', true
    );
END;
$$;

CREATE FUNCTION nmcp_expected_bundled_profile(profile_key text, dimension_rule text)
RETURNS jsonb
LANGUAGE sql
STABLE
STRICT
AS $$
WITH definitions AS (
    SELECT capability.input_mime_type, capability.source_mode,
           profile_key = 'standard' AS is_standard,
           CASE WHEN profile_key = 'standard' THEN 1920 ELSE 640 END AS max_edge,
           CASE WHEN profile_key = 'standard' THEN 60 ELSE 50 END AS quality
    FROM profile_processor_capabilities AS capability
    WHERE capability.processor = 'nmcp-media' AND capability.parameters_schema_version = 1
), recipes AS (
    SELECT input_mime_type, jsonb_build_object(
        'source_mode', source_mode,
        'frame_policy', CASE WHEN source_mode = 'still' THEN 'first'
            WHEN source_mode = 'probe-animation' AND is_standard THEN 'all'
            WHEN source_mode = 'probe-animation' THEN 'first'
            WHEN source_mode = 'video' AND is_standard THEN 'all' ELSE 'first' END,
        'max_long_edge', max_edge, 'allow_upscale', false, 'crop', 'none',
        'orientation', 'apply',
        'color', CASE WHEN source_mode = 'video' AND is_standard
            THEN 'normalize-bt709-tone-map-hdr' ELSE 'normalize-srgb-tone-map-hdr' END,
        'metadata', 'strip-after-normalization-keep-color-tags', 'alpha', 'preserve',
        'audio', CASE WHEN source_mode IN ('still','probe-animation') THEN 'none'
            WHEN is_standard THEN 'aac-if-present' ELSE 'discard' END,
        'still_output', CASE WHEN source_mode = 'video' AND is_standard THEN 'null'::jsonb
            ELSE jsonb_build_object('format','avif','quality',quality,'bit_depth',8) END,
        'animation_output', CASE WHEN source_mode = 'probe-animation' AND is_standard
            THEN jsonb_build_object('format','animated-webp','quality',80,'bit_depth',8) ELSE 'null'::jsonb END,
        'video_output', CASE WHEN source_mode = 'video' AND is_standard THEN jsonb_build_object(
            'container','mp4','video_codec','av1','crf',32,'bit_depth',10,
            'chroma','4:2:0','audio_codec','aac','audio_bitrate_kbps',128) ELSE 'null'::jsonb END,
        'dimension_rule', CASE WHEN source_mode = 'video'
            THEN 'preserve-aspect-no-crop-no-upscale-even-round-down' ELSE dimension_rule END,
        'stream_selection', CASE WHEN source_mode = 'video' THEN 'primary-video' ELSE 'not-applicable' END,
        'animation_timing', CASE WHEN source_mode = 'probe-animation' AND is_standard THEN 'preserve'
            WHEN source_mode IN ('probe-animation','video') AND NOT is_standard THEN 'first-frame'
            ELSE 'not-applicable' END,
        'animation_loop', CASE WHEN source_mode = 'probe-animation' AND is_standard THEN 'preserve'
            WHEN source_mode IN ('probe-animation','video') AND NOT is_standard THEN 'discard'
            ELSE 'not-applicable' END
    ) AS recipe
    FROM definitions
)
SELECT jsonb_build_object(
    'evidence_status','provisional-unverified',
    'recipes',jsonb_object_agg(input_mime_type, recipe ORDER BY input_mime_type)
)
FROM recipes;
$$;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_expected_bundled_profile(text,text) SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;

DO $$
DECLARE bundled record;
BEGIN
    IF (SELECT count(*) FROM profiles) <> 2 THEN
        RAISE EXCEPTION 'profile dimension correction requires exactly the two untouched bundled drafts'
            USING ERRCODE = '23514';
    END IF;
    FOR bundled IN
        SELECT * FROM (VALUES
            ('60000000-0000-4000-8000-000000000001'::uuid, 'standard'::text),
            ('60000000-0000-4000-8000-000000000002'::uuid, 'thumbnail'::text)
        ) AS expected(id, key)
    LOOP
        IF NOT EXISTS (
            SELECT 1 FROM profiles AS profile
            WHERE profile.id = bundled.id AND profile.key = bundled.key AND profile.version = 1
              AND profile.status = 'draft' AND profile.activated_at IS NULL AND profile.retired_at IS NULL
              AND profile.processor = 'nmcp-media' AND profile.parameters_schema_version = 1
              AND profile.input_mime_types = ARRAY(
                  SELECT input_mime_type FROM profile_processor_capabilities
                  WHERE processor = 'nmcp-media' AND parameters_schema_version = 1 ORDER BY input_mime_type
              )
              AND profile.parameters = nmcp_expected_bundled_profile(
                  bundled.key, 'preserve-aspect-no-crop-no-upscale-even-round-down'
              )
        ) THEN
            RAISE EXCEPTION 'bundled profile %/1 is missing, active, or divergent', bundled.key
                USING ERRCODE = '23514';
        END IF;
        IF EXISTS (SELECT 1 FROM job_targets WHERE profile_id = bundled.id)
           OR EXISTS (SELECT 1 FROM admin_batches WHERE profile_id = bundled.id) THEN
            RAISE EXCEPTION 'bundled profile %/1 already has unexpected references', bundled.key
                USING ERRCODE = '23514';
        END IF;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION nmcp_valid_profile_recipe(recipe jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    source_mode text;
    frame_policy text;
    audio_policy text;
    has_still boolean;
    has_animation boolean;
    has_video boolean;
    expected_dimension_rule text;
BEGIN
    IF NOT nmcp_jsonb_exact_keys(recipe, ARRAY[
        'source_mode','frame_policy','max_long_edge','allow_upscale','crop',
        'orientation','color','metadata','alpha','audio','still_output',
        'animation_output','video_output','dimension_rule','stream_selection',
        'animation_timing','animation_loop'
    ]) THEN RETURN false; END IF;
    source_mode := recipe->>'source_mode';
    expected_dimension_rule := CASE WHEN source_mode = 'video'
        THEN 'preserve-aspect-no-crop-no-upscale-even-round-down'
        ELSE 'preserve-aspect-no-crop-no-upscale-round-nearest' END;
    IF nmcp_jsonb_integer_between(recipe->'max_long_edge', 1, 2147483647) IS NOT TRUE
       OR recipe->'allow_upscale' IS DISTINCT FROM 'false'::jsonb
       OR recipe->>'crop' IS DISTINCT FROM 'none'
       OR recipe->>'orientation' IS DISTINCT FROM 'apply'
       OR recipe->>'metadata' IS DISTINCT FROM 'strip-after-normalization-keep-color-tags'
       OR recipe->>'alpha' IS DISTINCT FROM 'preserve'
       OR recipe->>'dimension_rule' IS DISTINCT FROM expected_dimension_rule THEN RETURN false; END IF;
    frame_policy := recipe->>'frame_policy';
    audio_policy := recipe->>'audio';
    has_still := jsonb_typeof(recipe->'still_output') IS DISTINCT FROM 'null';
    has_animation := jsonb_typeof(recipe->'animation_output') IS DISTINCT FROM 'null';
    has_video := jsonb_typeof(recipe->'video_output') IS DISTINCT FROM 'null';
    IF has_still AND NOT nmcp_valid_still_output(recipe->'still_output') THEN RETURN false; END IF;
    IF has_animation AND NOT nmcp_valid_animation_output(recipe->'animation_output') THEN RETURN false; END IF;
    IF has_video AND NOT nmcp_valid_video_output(recipe->'video_output') THEN RETURN false; END IF;
    RETURN COALESCE((source_mode = 'still' AND frame_policy = 'first' AND audio_policy = 'none'
            AND recipe->>'color' = 'normalize-srgb-tone-map-hdr' AND recipe->>'stream_selection' = 'not-applicable'
            AND recipe->>'animation_timing' = 'not-applicable' AND recipe->>'animation_loop' = 'not-applicable'
            AND has_still AND NOT has_animation AND NOT has_video)
        OR (source_mode = 'probe-animation' AND audio_policy = 'none'
            AND recipe->>'color' = 'normalize-srgb-tone-map-hdr' AND recipe->>'stream_selection' = 'not-applicable'
            AND has_still AND NOT has_video
            AND ((frame_policy = 'all' AND has_animation AND recipe->>'animation_timing' = 'preserve'
                  AND recipe->>'animation_loop' = 'preserve')
                 OR (frame_policy = 'first' AND NOT has_animation AND recipe->>'animation_timing' = 'first-frame'
                     AND recipe->>'animation_loop' = 'discard')))
        OR (source_mode = 'video'
            AND ((frame_policy = 'all' AND audio_policy = 'aac-if-present'
                  AND recipe->>'color' = 'normalize-bt709-tone-map-hdr' AND recipe->>'stream_selection' = 'primary-video'
                  AND recipe->>'animation_timing' = 'not-applicable' AND recipe->>'animation_loop' = 'not-applicable'
                  AND has_video AND NOT has_still AND NOT has_animation)
                 OR (frame_policy = 'first' AND audio_policy = 'discard'
                     AND recipe->>'color' = 'normalize-srgb-tone-map-hdr' AND recipe->>'stream_selection' = 'primary-video'
                     AND recipe->>'animation_timing' = 'first-frame' AND recipe->>'animation_loop' = 'discard'
                     AND has_still AND NOT has_animation AND NOT has_video))), false);
END;
$$;

ALTER TABLE profiles DISABLE TRIGGER profiles_lifecycle;
UPDATE profiles AS profile
SET parameters = nmcp_expected_bundled_profile(
    profile.key, 'preserve-aspect-no-crop-no-upscale-round-nearest'
)
WHERE profile.id IN (
    '60000000-0000-4000-8000-000000000001'::uuid,
    '60000000-0000-4000-8000-000000000002'::uuid
);
ALTER TABLE profiles ENABLE TRIGGER profiles_lifecycle;

DO $$
DECLARE bundled record;
BEGIN
    FOR bundled IN SELECT id, key, parameters FROM profiles LOOP
        IF bundled.parameters IS DISTINCT FROM nmcp_expected_bundled_profile(
            bundled.key, 'preserve-aspect-no-crop-no-upscale-round-nearest'
        ) THEN
            RAISE EXCEPTION 'bundled profile %/1 correction did not converge', bundled.key
                USING ERRCODE = '23514';
        END IF;
    END LOOP;
END;
$$;

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_valid_profile_recipe(jsonb) SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema
    );
END;
$$;
DROP FUNCTION nmcp_expected_bundled_profile(text, text);
