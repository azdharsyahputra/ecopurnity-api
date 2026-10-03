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
cmd/api/                  HTTP server
internal/api/             GENERATED from the spec: models, router, typed handler interface, 501 stubs (make gen)
internal/server/          routing under /api/v1, request validation, error contract, operation implementations
internal/config/          env configuration
internal/db/              Postgres primary/replica cluster (read/write routing)
internal/analytics/       ClickHouse client
deploy/                   postgres replication scripts, clickhouse init
scripts/                  openapi bundler, coverage check
```

## Run

```bash
cp .env.example .env
make up            # postgres :5432 (replica :5433), clickhouse :9000/:8123, mailpit :1025 (UI :8025), seaweedfs S3 :8333
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

## Server code generation

`make gen` bundles the spec, writes a 3.0.3 copy for the generator (`api/openapi.codegen.yaml`, not committed; 3.1-only
constructs such as `const` and `type: [x, null]` are rewritten), runs `oapi-codegen` (net/http router + strict typed
handlers + models + embedded spec) and regenerates `internal/api/unimplemented.gen.go`.

- Every operation is routed and its request validated against the spec before a handler runs. Validation failures
  answer `422 {error: {code: "validation", fields: {...}}}`; unknown endpoints `404 not_found`.
- `server.Server` embeds `api.Unimplemented`, so an operation answers `501 not_implemented` until a method with its name
  (e.g. `func (s *Server) Login(ctx, api.LoginRequestObject) (api.LoginResponseObject, error)`) is defined on `*Server`.
  Return the typed response objects for documented outcomes, or a `*server.Error` for the error contract.
- `go test ./internal/server` walks all 163 operations and fails if any is not routed.
- `make check-gen` fails when the committed generated code is stale.

## Email

Transactional email (verification code, password reset link) goes through SMTP when `SMTP_HOST` is set, otherwise it is
written to the log. Locally `.env` points at Mailpit: open http://localhost:8025 to read what the API sent.

For a real mailbox put the provider's settings in `.env` (never commit it): `SMTP_HOST`, `SMTP_PORT` (587 STARTTLS or 465
TLS), `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM="Ecopurnity <no-reply@your-domain>"`. The sending domain needs SPF/DKIM
records at the provider or mail lands in spam.

Templates live in `internal/mail/templates/` (one shared layout + one file per email with subject, preheader, HTML and
plain-text parts): `verify_code`, `reset_password`, `notification`. `make mail-preview` sends a sample of each through
the configured SMTP (Mailpit locally) so you can check them. Provider examples are in `.env.example`.

Email verification is a 6-digit code: 10 minutes, 5 attempts, one resend per minute; only an HMAC of the code (keyed by
`APP_SECRET`) is stored.

## File uploads (Cloudflare R2)

Uploads never pass through the API: `POST /uploads` returns a presigned PUT URL, the browser sends the file straight to
the bucket, and the endpoint that uses the file verifies it (owner, purpose, size, sniffed content type) before
attaching it. The bucket is private; files are read back with short-lived presigned GET URLs.

Locally `docker compose` runs SeaweedFS (S3-compatible) and the API creates the bucket and its CORS rule at startup.

For R2, in `.env` (never commit it):

```
S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
S3_REGION=auto
S3_BUCKET=<bucket>
S3_ACCESS_KEY_ID=<R2 API token access key>
S3_SECRET_ACCESS_KEY=<R2 API token secret>
S3_PATH_STYLE=false
S3_CREATE_BUCKET=false
S3_CORS_ORIGINS=https://<your frontend origin>
```

The R2 API token needs **Object Read & Write** on the bucket. Such a token cannot change bucket settings, so add the CORS
rule once in the dashboard (R2 → bucket → Settings → CORS policy), otherwise browsers can't PUT:

```json
[{ "AllowedOrigins": ["https://<your frontend origin>"], "AllowedMethods": ["PUT", "GET", "HEAD"],
   "AllowedHeaders": ["content-type"], "ExposeHeaders": ["etag"], "MaxAgeSeconds": 3600 }]
```

Keep the bucket private (no public r2.dev access); KTP and selfie photos are personal data.
