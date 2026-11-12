-- #21 release gate (review B2): no process running as the nmcp uid may hold
-- the owner (migrator) credential. `nmcp-admin check`, `repair` and
-- `maintenance resume` run as uid nmcp with only the nmcp_check /
-- nmcp_repair capability logins, so the few owner-table steps they need are
-- these narrow SECURITY DEFINER functions, each executable only by its
-- capability role. Bodies reproduce the exact SQL the CLI ran before.

-- F1: readiness gate (migration history) for both capabilities. Read-only.
CREATE FUNCTION nmcp_admin_read_migration_history()
RETURNS TABLE(version bigint,name text,checksum text,transactional boolean,idempotent boolean,dirty boolean)
LANGUAGE sql STABLE SECURITY DEFINER AS $$
    SELECT m.version,m.name,m.checksum,m.transactional,m.idempotent,m.dirty
    FROM schema_migrations AS m ORDER BY m.version
$$;

-- F2: maintenance status for repair (dry-run admission and result display).
CREATE FUNCTION nmcp_admin_read_maintenance_state()
RETURNS TABLE(mode text,reason text,owner text,entered_at timestamptz,successful_check_report jsonb)
LANGUAGE sql STABLE SECURITY DEFINER AS $$
    SELECT s.mode,s.reason,s.owner,s.entered_at,s.successful_check_report FROM maintenance_state AS s WHERE s.id=1
$$;

-- F3: enter maintenance for a repair, audited in the same transaction. A
-- concurrent holder of the maintenance row is reported as 'locked' (with a
-- failed audit row) instead of raising, so the refusal is still audited.
CREATE FUNCTION nmcp_admin_repair_enter_maintenance(
    report nmcp_uuid_v4,audit_id nmcp_uuid_v4,actor text,host text,started timestamptz
) RETURNS text LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE current_mode text;
BEGIN
    IF report IS NULL OR actor IS NULL OR actor='' OR host IS NULL OR host='' OR started IS NULL THEN
        RAISE EXCEPTION 'invalid repair actor' USING ERRCODE='22023';
    END IF;
    -- Review K1: only a sealed, error-free full report (the repair admission
    -- rule) may disable the service; anything else changes nothing.
    IF NOT EXISTS(SELECT 1 FROM reconciliation_check_reports AS r
        JOIN reconciliation_check_report_seals AS seal ON seal.report_id=r.id
        WHERE r.id=report
          AND (SELECT count(*) FROM reconciliation_check_source_results AS src WHERE src.report_id=r.id AND src.result='complete')=3) THEN
        RAISE EXCEPTION 'repair requires a sealed error-free report' USING ERRCODE='23514';
    END IF;
    BEGIN
        SELECT s.mode INTO current_mode FROM maintenance_state AS s WHERE s.id=1 FOR UPDATE NOWAIT;
    EXCEPTION WHEN lock_not_available THEN
        INSERT INTO admin_audit(id,command,actor,host,sanitized_arguments,outcome,affected_ids,started_at,finished_at)
        VALUES(audit_id,'repair_enter',actor,host,jsonb_build_object('report_id',report::text),'failed',ARRAY[report],started,clock_timestamp());
        RETURN 'locked';
    END;
    IF current_mode<>'maintenance' THEN
        UPDATE maintenance_state SET mode='maintenance',reason='administrative repair',owner=actor||'@'||host,
            entered_at=clock_timestamp(),successful_check_report=NULL,updated_at=clock_timestamp() WHERE id=1;
    END IF;
    INSERT INTO admin_audit(id,command,actor,host,sanitized_arguments,outcome,affected_ids,started_at,finished_at)
    VALUES(audit_id,'repair_enter',actor,host,jsonb_build_object('report_id',report::text),'succeeded',ARRAY[report],started,clock_timestamp());
    RETURN CASE WHEN current_mode='maintenance' THEN 'already' ELSE 'entered' END;
END;
$$;

-- F4: the repair result audit row.
CREATE FUNCTION nmcp_admin_record_repair_result(
    report nmcp_uuid_v4,run nmcp_uuid_v4,result_outcome text,audit_id nmcp_uuid_v4,actor text,host text,started timestamptz
) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    IF result_outcome NOT IN ('succeeded','partial') OR actor IS NULL OR actor='' OR host IS NULL OR host='' OR started IS NULL THEN
        RAISE EXCEPTION 'invalid repair result audit' USING ERRCODE='22023';
    END IF;
    -- Review K1: the audited run must be a run of this report, already
    -- executed (its native result row exists) and with an outcome that is
    -- consistent with the claimed one.
    IF NOT EXISTS(SELECT 1 FROM reconciliation_repair_runs AS rr
        JOIN reconciliation_repair_results AS res ON res.run_id=rr.id
        WHERE rr.id=run AND rr.source_report_id=report
          AND (result_outcome='partial' OR res.result='succeeded')) THEN
        RAISE EXCEPTION 'repair result audit does not match an executed run of this report' USING ERRCODE='23514';
    END IF;
    INSERT INTO admin_audit(id,command,actor,host,sanitized_arguments,outcome,affected_ids,started_at,finished_at)
    VALUES(audit_id,'repair_result',actor,host,jsonb_build_object('report_id',report::text),result_outcome,ARRAY[run],started,clock_timestamp());
END;
$$;

-- F5: the resume fence. The row lock belongs to the CALLER's transaction and
-- is held across the fresh check run by the separate check login, exactly as
-- the previous single-transaction resume held it.
CREATE FUNCTION nmcp_admin_lock_maintenance_for_resume()
RETURNS TABLE(mode text,reason text,owner text)
LANGUAGE sql VOLATILE SECURITY DEFINER AS $$
    SELECT s.mode,s.reason,s.owner FROM maintenance_state AS s WHERE s.id=1 FOR UPDATE NOWAIT
$$;

-- F6: resume to normal mode on a fresh, sealed, complete, finding-free full
-- report with no unfinished repair run (the unchanged resume predicate),
-- audited in the same transaction. Returns false when the report does not
-- authorize resume; raises only on invalid input or a non-maintenance state.
CREATE FUNCTION nmcp_admin_resume_maintenance(
    report nmcp_uuid_v4,check_result jsonb,audit_id nmcp_uuid_v4,actor text,host text,started timestamptz
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE current_mode text; current_reason text; current_entered timestamptz; authorized boolean;
BEGIN
    IF check_result IS NULL OR jsonb_typeof(check_result)<>'object' OR actor IS NULL OR actor='' OR host IS NULL OR host='' OR started IS NULL THEN
        RAISE EXCEPTION 'invalid resume request' USING ERRCODE='22023';
    END IF;
    SELECT s.mode,s.reason,s.entered_at INTO current_mode,current_reason,current_entered
    FROM maintenance_state AS s WHERE s.id=1 FOR UPDATE NOWAIT;
    IF current_mode<>'maintenance' THEN
        RAISE EXCEPTION 'resume requires maintenance mode' USING ERRCODE='55000';
    END IF;
    -- Review K1: this function is the database-side resume gate. A
    -- post-restore maintenance is resumed only by the root operator procedure
    -- (its external ledger is not visible here).
    IF current_reason='operator restore' THEN
        RAISE EXCEPTION 'restore maintenance requires the operator resume' USING ERRCODE='55000';
    END IF;
    -- The report must be fresh (created after maintenance was entered) and be
    -- exactly the one the caller's check result describes.
    IF check_result->>'ReportID' IS DISTINCT FROM report::text THEN
        RETURN false;
    END IF;
    SELECT EXISTS(SELECT 1 FROM reconciliation_check_reports r
        JOIN reconciliation_check_report_seals seal ON seal.report_id=r.id
        WHERE r.id=report AND r.scope='all' AND seal.finding_count=0
        AND current_entered IS NOT NULL AND r.created_at>current_entered
        AND (SELECT count(*) FROM reconciliation_check_source_results src WHERE src.report_id=r.id AND src.result='complete')=3)
        AND NOT EXISTS(SELECT 1 FROM reconciliation_repair_runs run WHERE NOT EXISTS
        (SELECT 1 FROM reconciliation_repair_results result WHERE result.run_id=run.id))
    INTO authorized;
    IF NOT authorized THEN
        RETURN false;
    END IF;
    UPDATE maintenance_state SET mode='normal',reason=NULL,owner=NULL,entered_at=NULL,
        successful_check_report=check_result,updated_at=clock_timestamp() WHERE id=1;
    INSERT INTO admin_audit(id,command,actor,host,sanitized_arguments,outcome,affected_ids,started_at,finished_at)
    VALUES(audit_id,'maintenance_resume',actor,host,jsonb_build_object('report_id',report::text),'succeeded',ARRAY[report],started,clock_timestamp());
    RETURN true;
END;
$$;

-- Owners hold exactly what the bodies read and write.
GRANT SELECT ON schema_migrations TO nmcp_check_function_owner,nmcp_repair_function_owner;
GRANT SELECT,UPDATE ON maintenance_state TO nmcp_repair_function_owner;
GRANT INSERT ON admin_audit TO nmcp_repair_function_owner;
GRANT SELECT ON reconciliation_check_reports,reconciliation_check_report_seals,reconciliation_check_source_results,
    reconciliation_repair_runs,reconciliation_repair_results TO nmcp_repair_function_owner;

DO $$
DECLARE target_schema text:=current_schema(); signature text;
BEGIN
    -- ALTER ... OWNER requires the new owner to hold CREATE on the schema;
    -- grant it only for the transfer, exactly as 0018/0019 do.
    EXECUTE format('GRANT USAGE,CREATE ON SCHEMA %I TO nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
    EXECUTE format('ALTER FUNCTION %I.nmcp_admin_read_migration_history() OWNER TO nmcp_check_function_owner',target_schema);
    FOREACH signature IN ARRAY ARRAY[
        'nmcp_admin_read_maintenance_state()',
        'nmcp_admin_repair_enter_maintenance(nmcp_uuid_v4,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_record_repair_result(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_lock_maintenance_for_resume()',
        'nmcp_admin_resume_maintenance(nmcp_uuid_v4,jsonb,nmcp_uuid_v4,text,text,timestamptz)'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s OWNER TO nmcp_repair_function_owner',target_schema,signature);
    END LOOP;
    EXECUTE format('REVOKE CREATE ON SCHEMA %I FROM nmcp_check_function_owner,nmcp_repair_function_owner',target_schema);
    FOREACH signature IN ARRAY ARRAY[
        'nmcp_admin_read_migration_history()',
        'nmcp_admin_read_maintenance_state()',
        'nmcp_admin_repair_enter_maintenance(nmcp_uuid_v4,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_record_repair_result(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_lock_maintenance_for_resume()',
        'nmcp_admin_resume_maintenance(nmcp_uuid_v4,jsonb,nmcp_uuid_v4,text,text,timestamptz)'
    ] LOOP
        EXECUTE format('ALTER FUNCTION %I.%s SET search_path=%I,pg_catalog,pg_temp',target_schema,signature,target_schema);
        EXECUTE format('REVOKE ALL ON FUNCTION %I.%s FROM PUBLIC',target_schema,signature);
    END LOOP;
    EXECUTE format('GRANT EXECUTE ON FUNCTION %I.nmcp_admin_read_migration_history() TO nmcp_check_runtime,nmcp_repair_runtime',target_schema);
    FOREACH signature IN ARRAY ARRAY[
        'nmcp_admin_read_maintenance_state()',
        'nmcp_admin_repair_enter_maintenance(nmcp_uuid_v4,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_record_repair_result(nmcp_uuid_v4,nmcp_uuid_v4,text,nmcp_uuid_v4,text,text,timestamptz)',
        'nmcp_admin_lock_maintenance_for_resume()',
        'nmcp_admin_resume_maintenance(nmcp_uuid_v4,jsonb,nmcp_uuid_v4,text,text,timestamptz)'
    ] LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.%s TO nmcp_repair_runtime',target_schema,signature);
    END LOOP;
END;
$$;

-- Report/repair formats are unchanged by this migration. Keep the explicit
-- compatibility boundary (resource 56 forward guard): recognize version 25 and
-- retain every sealed/source/checksum admission condition; 26+ stays refused.
ALTER TABLE reconciliation_repair_plan_seals
    DROP CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check;
ALTER TABLE reconciliation_repair_plan_seals
    ADD CONSTRAINT reconciliation_repair_plan_seals_report_migration_version_check
    CHECK (report_migration_version IN (17,18,19,20,21,22,23,24,25));

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
          AND report.migration_version IN (17,18,19,20,21,22,23,24,25)
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
