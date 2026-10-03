# Ecopurnity API

Backend for the Ecopurnity frontend. Go · PostgreSQL (primary + streaming replica) · ClickHouse.

The contract comes first: `api/openapi.yaml` describes all 163 endpoints the frontend calls. It was written from the
frontend's reference mock (`../ecopurnity/src/mocks`), so request/response shapes, status codes, error codes, role checks and
side effects match what the UI already depends on.

## Layout

```
api/openapi.yaml          generated, committed: the one document to read / feed to codegen
openapi/                  sources (edit these, not api/openapi.yaml)
  base.yaml                 info, tags, shared parameters/responses
  CONVENTIONS.md            how the sources are written
  paths/<area>.yaml         operations per area
  schemas/*.yaml            component schemas (core, domain-*, paths-*)
cmd/api/                  HTTP server (health endpoints only for now)
internal/config/          env configuration
internal/db/              Postgres primary/replica cluster (read/write routing)
internal/analytics/       ClickHouse client
deploy/                   postgres replication scripts, clickhouse init
scripts/                  openapi bundler, coverage check
```

## Run

```bash
cp .env.example .env
make up            # postgres primary :5432, replica :5433, clickhouse :9000/:8123
make run           # API on :8080  ->  GET /healthz, GET /readyz
make reset         # stop and wipe volumes
```

## OpenAPI workflow

```bash
npm install
npm run openapi:lint                                   # bundle + redocly lint
node scripts/check-coverage.mjs                        # every mock endpoint is in the spec, and vice versa
```

`check-coverage` reads the endpoint list from the frontend repo (`docs/api-contract.md`, regenerated there with
`npm run contract`). Run it whenever either side changes.

## Data architecture

| Store | Holds | Rules |
| --- | --- | --- |
| PostgreSQL primary | System of record: users, orgs, listings, auctions, bids, trades, ledger, audit | All writes. Reads that must see their own write. |
| PostgreSQL replica | Same data, asynchronous streaming replication | Read-only: lists, dashboards, public pages. `db.Cluster.Reader()` falls back to the primary when the replica is down or lags past `REPLICA_MAX_LAG_SECONDS`. |
| ClickHouse | Append-only events and aggregates: explorer stats, price history, activity feed, audit search, market-maker analytics | Never the system of record; rebuildable from Postgres through an outbox publisher. |

A request that writes and then returns the resource must read from the primary, because the replica applies WAL
asynchronously.

## Notes for the implementer

- Business rules are in the operation `description`s (state machines, fee/PPN split, limits, masking rules). The frontend
  domain code (`../ecopurnity/src/domain/*.ts`, with tests) is the executable version of the same rules and can be ported
  one-to-one.
- Places where the mock is sloppy (unvalidated inputs, 500s on bad enums, odd status codes) are marked `x-note` on the
  operation. Decide per case whether the real API should keep the mock's behaviour or be stricter; stricter is safe for
  the frontend unless the note says the UI relies on it.
- Realtime (WebSocket) is `api/asyncapi.yaml` (AsyncAPI 3.0), explained in `docs/realtime.md`.
