-- #21 release gate: profile certification requirements now match the Worker's
-- executable routes (internal/transformexecutor). Old migration bytes are
-- unchanged; this forward migration replaces the function body in place, which
-- keeps its owner and ACL, and re-pins its closed search path.
CREATE OR REPLACE FUNCTION nmcp_profile_recipe_certified(
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
    -- Require a still-avif certification only where the Worker can produce
    -- one: still sources, first-frame recipes, and image/webp under all-frames
    -- (a static WebP is rerouted to the still processor). Any other
    -- probe-animation source under all-frames (GIF) always produces animated
    -- WebP, so its still_output is never executed and never certified.
    IF jsonb_typeof(recipe->'still_output') IS DISTINCT FROM 'null'
       AND NOT (recipe_source_mode = 'probe-animation'
                AND recipe->>'frame_policy' = 'all'
                AND mime_value <> 'image/webp') THEN
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

DO $$
DECLARE target_schema text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.nmcp_profile_recipe_certified(text,integer,text,text,jsonb) SET search_path = %I, pg_catalog, pg_temp',
        target_schema, target_schema);
END;
$$;

-- Report/repair formats are unchanged by this migration. Keep the explicit
-- compatibility boundary (resource 56 forward guard): recognize version 24 and
-- retain every sealed/source/checksum admission condition; 25+ stays refused.
ALTER TABLE reconciliation_repair_plan_seals
    DROP CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check;
ALTER TABLE reconciliation_repair_plan_seals
    ADD CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check
    CHECK (report_migration_version IN (17,18,19,20,21,22,23,24));

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
          AND report.migration_version IN (17,18,19,20,21,22,23,24)
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

-- Preserve the original definer owner, closed search path and existing ACL.
DO $$
DECLARE target_schema text:=current_schema();
BEGIN
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer) OWNER TO nmcp_repair_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_read_repairable_findings(nmcp_uuid_v4,bigint,integer) SET search_path=%I,pg_catalog,pg_temp',target_schema,target_schema);
END;
$$;
