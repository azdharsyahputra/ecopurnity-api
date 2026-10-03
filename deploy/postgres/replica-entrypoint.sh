#!/usr/bin/env bash
# Streaming replica: on first start clone the primary with pg_basebackup (-R writes standby.signal and primary_conninfo),
# then run postgres as a hot standby. Re-creating the volume re-clones.
set -euo pipefail
export PGDATA=${PGDATA:-/var/lib/postgresql/data}

if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "replica: waiting for primary"
  until pg_isready -h postgres-primary -U "$POSTGRES_USER" >/dev/null 2>&1; do sleep 1; done
  mkdir -p "$PGDATA" && chown -R postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"
  echo "replica: cloning primary"
  PGPASSWORD="$REPLICATION_PASSWORD" gosu postgres pg_basebackup \
    -h postgres-primary -U replicator -D "$PGDATA" -R -X stream -C -S replica_1 -P
fi
exec gosu postgres postgres -c hot_standby=on -c hot_standby_feedback=on
