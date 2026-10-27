-- Immutable dump manifests and effective configuration snapshots must not be
-- repurposed as mutable cleanup journals. Lifecycle metadata lives separately.
ALTER TABLE backup_runs
    ADD COLUMN retention_deleting_at timestamptz,
    ADD COLUMN retention_deleted_at timestamptz,
    ADD COLUMN staging_cleanup_completed_at timestamptz,
 ADD COLUMN failure_audit_pending boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT backup_runs_retention_lifecycle_check CHECK (
        (retention_deleting_at IS NULL OR status = 'succeeded')
        AND (retention_deleted_at IS NULL OR
             (status = 'succeeded' AND retention_deleting_at IS NULL))
        AND (staging_cleanup_completed_at IS NULL OR status = 'failed')
 AND (NOT failure_audit_pending OR status='failed')
    );

-- SELECT FOR SHARE itself requires UPDATE privileges. Do not grant the control
-- login any ability to update maintenance_state. This closed, schema-qualified
-- capability borrows only the migration owner's short row-locking authority.
DO $migration$
DECLARE app_schema text := current_schema();
BEGIN
    EXECUTE format($definition$
        CREATE FUNCTION %1$I.nmcp_backup_normal_fence() RETURNS boolean
        LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
        AS $function$
        DECLARE current_mode text;
        BEGIN
            SELECT mode INTO current_mode FROM %1$I.maintenance_state
                WHERE id=1 FOR SHARE NOWAIT;
            RETURN COALESCE(current_mode='normal', false);
        END;
        $function$
    $definition$, app_schema);
    EXECUTE format('REVOKE ALL ON FUNCTION %I.nmcp_backup_normal_fence() FROM PUBLIC', app_schema);
END;
$migration$;
