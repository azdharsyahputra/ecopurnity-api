#!/usr/bin/env bash
# Runs once on the primary's first start (docker-entrypoint-initdb.d): creates the replication role and lets it connect.
set -euo pipefail
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '${REPLICATION_PASSWORD}';
SQL
echo "host replication replicator all scram-sha-256" >> "$PGDATA/pg_hba.conf"
