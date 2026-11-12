\set ON_ERROR_STOP on

\ir /opt/nmcp-infra/ubuntu/postgresql-roles.sql

ALTER ROLE nmcp_migrator PASSWORD 'nmcp-migrator-validation-only';
ALTER ROLE nmcp_api PASSWORD 'nmcp-api-validation-only';
ALTER ROLE nmcp_worker PASSWORD 'nmcp-worker-validation-only';
ALTER ROLE nmcp_check PASSWORD 'nmcp-check-validation-only';
ALTER ROLE nmcp_repair PASSWORD 'nmcp-repair-validation-only';
