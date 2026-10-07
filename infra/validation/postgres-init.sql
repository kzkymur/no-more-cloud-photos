\set ON_ERROR_STOP on

CREATE ROLE nmcp_migrator LOGIN PASSWORD 'nmcp-migrator-validation-only'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE nmcp_api LOGIN PASSWORD 'nmcp-api-validation-only'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE nmcp_worker LOGIN PASSWORD 'nmcp-worker-validation-only'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;

\set nmcp_migrator_role nmcp_migrator
\ir /opt/nmcp-bootstrap/runtime-roles.sql

GRANT nmcp_runtime TO nmcp_api;
GRANT nmcp_worker_runtime TO nmcp_worker;

REVOKE ALL ON DATABASE nmcp FROM PUBLIC;
GRANT CONNECT ON DATABASE nmcp TO nmcp_migrator,nmcp_api,nmcp_worker;
ALTER DATABASE nmcp OWNER TO nmcp_migrator;
ALTER SCHEMA public OWNER TO nmcp_migrator;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
