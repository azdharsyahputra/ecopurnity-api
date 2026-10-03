#!/usr/bin/env bash
# Applies migrations/clickhouse/*.sql in order to the local ClickHouse. Every statement is IF NOT EXISTS, so re-running is safe.
set -euo pipefail
for f in migrations/clickhouse/*.sql; do
  docker exec -i ecopurnity-clickhouse-1 clickhouse-client --multiquery < "$f"
  echo "OK   $f"
done
