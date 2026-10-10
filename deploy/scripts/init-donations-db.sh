#!/usr/bin/env bash
# Creates, once, the donations database and its two roles in the service's
# Postgres: donations_worker owns the database (the worker creates its
# tables), donations_api may only read them (the public API). Safe to run
# again; passwords come from .env (setup.sh makes them).
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a
: "${DONATIONS_WORKER_DBPASSWORD:?run scripts/setup.sh}"
: "${DONATIONS_API_DBPASSWORD:?run scripts/setup.sh}"

# psql variables keep the passwords out of the SQL text and off any
# command line (they go through the environment of the exec)
docker compose exec -T \
	-e PGPASSWORD_WORKER="$DONATIONS_WORKER_DBPASSWORD" \
	-e PGPASSWORD_API="$DONATIONS_API_DBPASSWORD" \
	postgres sh -c 'psql -v ON_ERROR_STOP=1 -U boltz -d postgres \
		-v worker_pw="$PGPASSWORD_WORKER" -v api_pw="$PGPASSWORD_API"' <<'SQL'
SELECT 'CREATE ROLE donations_worker LOGIN'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_worker') \gexec
SELECT 'CREATE ROLE donations_api LOGIN'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_api') \gexec
ALTER ROLE donations_worker PASSWORD :'worker_pw';
ALTER ROLE donations_api PASSWORD :'api_pw';
SELECT 'CREATE DATABASE donations OWNER donations_worker'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'donations') \gexec
REVOKE ALL ON DATABASE donations FROM PUBLIC;
GRANT CONNECT ON DATABASE donations TO donations_worker, donations_api;
\connect donations
GRANT USAGE ON SCHEMA public TO donations_api;
ALTER DEFAULT PRIVILEGES FOR ROLE donations_worker IN SCHEMA public
    GRANT SELECT ON TABLES TO donations_api;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO donations_api;
SQL
echo "donations database ready"
