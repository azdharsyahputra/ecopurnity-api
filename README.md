# Ecopurnity API

Backend of **Ecopurnity**, an economic opportunity engine: it finds markets that don't exist yet from supply, demand and
networks, lets market makers form them, and runs the whole deal (real-time auctions, RFQs, escrow payments, shipments,
QC, disputes, payouts) for individuals, SMEs and organisations.

Live API: <https://api.ecopurnity.my.id> · App: <https://ecopurnity.my.id> · Frontend repo: `ecopurnity-fe`

The contract comes first: `api/openapi.yaml` (OpenAPI 3.1) describes all 172 endpoints the frontend calls, and
`api/asyncapi.yaml` the realtime channels. Request/response shapes, status codes, error codes, role checks and side
effects match what the UI depends on; CI fails if the spec, the generated code or the frontend's endpoint list drift.

## What sets it apart

- **Opportunity engine.** A background job clusters open listings by item, category, region and unit and detects market
  gaps, supply gaps, collective demand and capacity matches; opportunities are scored per user and flow into the market
  makers' pipeline.
- **Markets with transparent rules.** Market makers form markets from opportunities (or from SME collective pools) with
  versioned rules (eligibility, visibility, steps, quantities, award method) that apply from the next round.
- **Real-time auctions done right.** Reverse, forward, sealed and Dutch auctions; masked bidder identities; anti-sniping
  extensions; per-auction withdraw rules; bid capacity and Smart Allocation (split awards); a gapless per-auction
  sequence so clients replay missed bids after a reconnect. The auction clock is safe to run on every instance.
- **Collective procurement.** SMEs pool demand into one market round; the result is split pro-rata (largest remainder)
  into per-member purchase orders.
- **Money you can audit.** A double-entry ledger with a balanced-journal check at commit; escrow, PPN and fees per the
  invoice; Midtrans Core API payments (VA, QRIS, e-wallets) settled only on a verified notification; partial QC refunds;
  dispute rulings that move the money; payouts approved by an admin with the transfer reference on record.
- **Trust and compliance.** Tiered KYC (email → KTP) with commitment limits; NIK and bank numbers encrypted at rest
  (AES-GCM, keys derived from one secret); every view of decrypted personal data is audited; private file storage with
  short-lived presigned links.
- **Organisations.** Roles and permissions, approval rules (an owner can sign for a role with no members), procurement,
  multi-lot auctions, supplier scorecards and spend analytics.
- **Built to scale reads.** PostgreSQL primary + streaming replica with lag-aware read routing, ClickHouse for
  analytics fed by a transactional outbox, LISTEN/NOTIFY fan-out for WebSockets across instances.

## Tech stack

| Concern | Technology |
| --- | --- |
| Language / HTTP | **Go 1.26**, `net/http` with code generated from the spec (**oapi-codegen** strict server), request validation against the spec |
| Contract | **OpenAPI 3.1** (REST) and **AsyncAPI 3.0** (WebSocket), bundled from `openapi/` and linted with Redocly |
| System of record | **PostgreSQL** primary + asynchronous streaming replica (**pgx v5**), migrations with **goose** (embedded) |
| Analytics | **ClickHouse** (events + materialized views) fed through a transactional outbox |
| Realtime | WebSocket (**coder/websocket**) with Postgres LISTEN/NOTIFY fan-out and outbox replay |
| Payments | **Midtrans Core API** (official Go SDK), webhook + status reconciliation |
| Files | S3-compatible object storage (**Cloudflare R2** in production, SeaweedFS locally), presigned uploads |
| Email | SMTP with HTML templates (Mailpit locally) |
| Security | argon2id passwords, opaque session tokens (hashed), HKDF-derived keys, AES-GCM for personal data |
| Delivery | Docker, **GitHub Actions** CI/CD (tests on Postgres 15 + ClickHouse + S3, auto-deploy from `main`) |

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
internal/auth/            passwords, session tokens, email codes
internal/secure/          key derivation and encryption of personal data
internal/payments/        Midtrans Core API gateway (+ fake gateway for dev/tests)
internal/storage/         S3-compatible storage (presigned URLs)
internal/mail/            SMTP sender and email templates
migrations/               postgres (goose) and clickhouse SQL
deploy/                   local docker-compose helpers (postgres replication, clickhouse init, seaweedfs)
scripts/                  openapi bundler, coverage check
```

## Run

```bash
cp .env.example .env
make up            # postgres :5432 (replica :5433), clickhouse :9000/:8123, mailpit :1025 (UI :8025), seaweedfs S3 :8333
make run           # API on :8080  ->  GET /healthz, GET /readyz
make reset         # stop and wipe volumes
make seed-admin EMAIL=you@example.id   # grant the admin capability to an account you registered
make test          # integration tests (need `make up`; each run uses a throwaway database)
```

## CI/CD

GitHub Actions (`.github/workflows/ci-cd.yml`) on every push and pull request: gofmt, `go vet`, generated code up to date
(`make check-gen`), OpenAPI lint, and the full test suite against PostgreSQL 15, ClickHouse and S3 (SeaweedFS). A push to
`main` that passes is deployed automatically (build, Postgres + ClickHouse migrations, restart).

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

## Contributing

How to implement or change an endpoint (handler shape, errors, transactions, audit, notifications, realtime frames,
tests) is in `docs/contributing.md`. Database rules are in `docs/database.md`, realtime in `docs/realtime.md`. Business
rules also live in the frontend's `src/domain/*.ts` (with tests); ports here keep the same behaviour.

## Server code generation

`make gen` bundles the spec, writes a 3.0.3 copy for the generator (`api/openapi.codegen.yaml`, not committed; 3.1-only
constructs such as `const` and `type: [x, null]` are rewritten), runs `oapi-codegen` (net/http router + strict typed
handlers + models + embedded spec) and regenerates `internal/api/unimplemented.gen.go`.

- Every operation is routed and its request validated against the spec before a handler runs. Validation failures
  answer `422 {error: {code: "validation", fields: {...}}}`; unknown endpoints `404 not_found`.
- `server.Server` embeds `api.Unimplemented`, so an operation answers `501 not_implemented` until a method with its name
  (e.g. `func (s *Server) Login(ctx, api.LoginRequestObject) (api.LoginResponseObject, error)`) is defined on `*Server`.
  Return the typed response objects for documented outcomes, or a `*server.Error` for the error contract.
- `go test ./internal/server` walks all 172 operations and fails if any is not routed.
- `make check-gen` fails when the committed generated code is stale.

## Realtime (WebSocket)

`GET /api/v1/ws`: contract `api/asyncapi.yaml`, guide `docs/realtime.md`. Every instance keeps a `LISTEN ecp_rt`
connection; one instance at a time (advisory lock) publishes the outbox to that feed and to ClickHouse. Try it with
[websocat](https://github.com/vi/websocat) after `make migrate && make run`, one JSON frame per line:

```bash
websocat ws://localhost:8080/api/v1/ws                    # anonymous: public:* and auction:{id}
{"type":"subscribe","id":"1","channel":"public:activity"}
{"type":"subscribe","id":"2","channel":"auction:<auction id>","sinceSeq":0}
{"type":"ping","id":"3"}

websocat -H 'Cookie: ecp_session=<cookie value>' ws://localhost:8080/api/v1/ws   # signed in: also user:{your id}
```

`GET /readyz` shows `realtime: {feed, publisher, sockets}` for the instance.

## Simulated counterparties (demo only)

`SIMULATE_COUNTERPARTIES=true` (the `.env.example` default) starts background ticks that play parties without a platform
account: fictional suppliers quote on RFQs, answer counters and reply in chat (`internal/server/counterparties.go`), and
external trade/contract counterparties accept, invoice, ship, confirm and review (`internal/server/trade_clock.go`), so
one tester can walk every flow alone. **Leave it unset (false) in production.**

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

## Trades, money and payouts

The F6 settlement flow (agreement, invoice, payment, staged shipments, QC, disputes, reviews) is one engine,
`applyTradeAction` in `internal/server/trade_engine.go`; its header lists who may call it and the ledger journal each
action posts. Money is a double-entry ledger (`ledger_entries`, balanced per journal at commit); `/me/finance` is derived
from it (`internal/server/finance.go`). `RunTradeClock` places due standing-contract orders.

Sellers withdraw their available balance to a registered bank account; an admin transfers it by hand and records the
transfer reference (or rejects it, which returns the money to the wallet). Admin → Pencairan, `internal/server/payouts.go`.

## Verification (KYC)

Email only, no SMS/WhatsApp. Level 0 *Email* (verified email, Rp 10 jt per commitment) and level 1 *KTP* (KTP + selfie
reviewed by an admin, Rp 2 M per commitment). Every commitment (bid, Dutch accept, buyer auction, accepted quote, direct
order) is checked against the limit (`commitGuard`, `403 kyc_limit`). The NIK is stored encrypted with an HMAC for
uniqueness; admins see it decrypted only on the review page, and each view is audited.

## Payments (Midtrans Core API)

Buyers pay invoices in our own UI with Midtrans **Core API** charges (`internal/payments`, used by
`internal/server/payments.go`): virtual account (BCA, BNI, BRI, Permata, CIMB), Mandiri bill (`echannel`), QRIS, GoPay
and ShopeePay. A charge stays payable 24 h (transfers) or 15 min (QRIS, e-wallets). The money moves only when Midtrans
reports settlement: then the trade engine's `pay` step runs once (same ledger journals as before); users cannot send
`pay` themselves. Expired, cancelled, denied or failed payments close and notify the buyer; the trade waits for a new
attempt. Midtrans fees (MDR) are absorbed by the platform for now.

- **Keys**: sign in at <https://dashboard.sandbox.midtrans.com> → Settings → Access Keys and put the sandbox Server
  Key and Client Key in `.env` (`MIDTRANS_SERVER_KEY`, `MIDTRANS_CLIENT_KEY`, `MIDTRANS_ENV=sandbox`). Production keys
  come from <https://dashboard.midtrans.com> with `MIDTRANS_ENV=production`. Never commit them.
- **No key**: an empty `MIDTRANS_SERVER_KEY` runs a fake gateway (startup warning): plausible VA/QR data, and every
  payment settles by itself after 10 s. Dev and tests only.
- **Notification URL**: in the dashboard (Settings → Payment → Notification URL) set
  `{public API URL}/api/v1/payments/midtrans/notification`. Notifications are verified (`signature_key`) and then
  confirmed with a status call; the body is never trusted. Without a public URL (local dev) a reconciler polls Midtrans
  for pending payments every 30 s, so sandbox payments (Midtrans simulator) still complete.
- **Smoke test** (sandbox, opt-in): `set -a; . ./.env; set +a; MIDTRANS_LIVE_TEST=1 go test ./internal/payments -run Live -v`
  charges a BCA VA and a QRIS, checks they are pending and cancels them.
- **Not supported**: credit cards (by decision). Refunds through Midtrans are manual (a payment that settles after the
  invoice was already paid is logged for a refund in the dashboard); MDR fees are not in the ledger yet.

## File uploads (Cloudflare R2)

Uploads never pass through the API: `POST /uploads` returns a presigned PUT URL, the browser sends the file straight to
the bucket, and the endpoint that uses the file verifies it (owner, purpose, size, sniffed content type) before
attaching it. The bucket is private; files are read back with short-lived presigned GET URLs.

| Purpose | Files | Used by |
| --- | --- | --- |
| `kyc_ktp`, `kyc_selfie` | JPEG/PNG/WebP, 8 MB | KTP verification (admins only, presigned on the review page) |
| `org_document` | PDF/JPEG/PNG/WebP, 10 MB | business verification documents (NIB, NPWP, deed) |
| `listing_attachment` | PDF/JPEG/PNG/WebP, 10 MB, max 8 per listing | listing photos and documents (public for public listings) |
| `trade_proof` | PDF/JPEG/PNG/WebP, 10 MB | delivery proofs (the trade's two sides only) |
| `dispute_evidence` | PDF/JPEG/PNG/WebP, 10 MB | dispute evidence (both sides and admins on the case) |

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
