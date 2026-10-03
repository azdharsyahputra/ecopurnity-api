# Implementing an endpoint

Every operation in `api/openapi.yaml` is generated into `internal/api` and answers `501 not_implemented` until a method
with the same name is defined on `*server.Server`. This is how the existing ones are written; follow it.

## Sources of truth

1. `api/openapi.yaml` (sources in `openapi/`): request/response shapes, status codes, error codes, rules in the
   operation `description`. `x-note` marks where the frontend mock is sloppy: implement the stricter behaviour and update
   the note/description (the spec must describe the real API).
2. The frontend mock in `/Users/csadeveloper/kkn/ecp/ecopurnity/src/mocks/*.ts` (read-only): behaviour details and the
   **Indonesian user-facing copy** (error messages, notification titles, history notes). Reuse the copy word for word.
   Business rules with tests live in the frontend's `src/domain/*.ts`; port them (and their tests) to Go when needed.
3. `migrations/postgres/*.sql` + `docs/database.md` (tables, constraints, rules the app must follow). New tables or
   columns: a new numbered goose migration (never edit an applied one), following `migrations/CONVENTIONS.md`.

## Handler shape

- One file per area in `internal/server/` (e.g. `transactions.go`, `transactions_test.go`). Methods on `*Server`.
- Auth: `sess, err := requireUser(ctx)` (401 / 403 suspended) or `requireActive(ctx)` (also 403 `account_restricted`).
  `viewerID(ctx)` for optional sessions on public endpoints. Admin / market maker / org access: add a small helper in
  your area file (check `user_capabilities`, `market_operators`, `org_members` + `org_roles.permissions`).
- Errors: return `*Error{Status, Code, Message, Fields}` (written as `{error:{code,message,fields}}`). Field-level
  validation → 422 `validation` with `Fields`. `validationFields(map)` helper exists. Not found → 404 `not_found`.
- Writes: `s.inTx(ctx, func(tx pgx.Tx) error {...})` on the primary. Lock the row you change (`FOR UPDATE`) before
  checking its state. Reads that must see the caller's own write use `s.DB.Primary()`; other reads `s.DB.Reader()`.
- Side effects inside the same transaction:
  - `writeAudit(ctx, tx, audit{...})` for every governance / market maker / org action (spec `x-audit: true`).
  - `notify(ctx, tx, userID, notification{Type, Title, Body, Href})` (stores it and queues the realtime frame).
  - `emitFrame(ctx, tx, channel, type, seq, payload)` for realtime frames (`api/asyncapi.yaml`); `emitAuction` /
    `emitAuctionState` for auction channels; `emitBidStatus` for a bidder's own status.
  - `emit(ctx, tx, topic, aggregateID, payload)` for analytics facts (topics and payloads in `migrations/clickhouse/README.md`).
  - `emitActivity(ctx, tx, type, title, amountIdr, marketID)` for the public activity feed (`ActivityType`; topic
    `activity` + a `public:activity` frame). `detection.go` emits `opportunity_detected`.
- Shared building blocks: `userParty` (party of a user, created on first use), `createTrade` (`trades.go`),
  `createOrg` (`onboarding.go`), `loadUser`, `loadMarkets`, `loadOpportunities`, `loadAuctions` / `loadAuction` /
  `auctionDetail` / `myBid` (auction read model with visibility masking), `s.commitGuard` (KYC limit), `claimUpload`
  (verified uploads), `pageParams`, `likeEscape`, `rupiah`, `ptr`, `deref*`.
- Responses: return the generated typed response objects. Two generator gaps need a hand-written response type
  implementing the operation's `Visit…Response(w)`: a nullable body (`null`) and a union + extra fields (see
  `listingDetailResponse`; `internal/api/gen_test.go` fails if a new one appears unhandled).
- Lists that are empty must serialise as `[]`, not `null`.
- Money: integer Rupiah (`int64` in Go), quantities `float64`. Times UTC `time.Time`.

## Spec changes

Edit `openapi/` (paths/*.yaml, schemas/*.yaml), then:

```bash
npm run openapi:lint            # bundle + lint
node scripts/check-coverage.mjs # spec vs frontend mock endpoints (must stay equal)
make gen                        # regenerate internal/api (commit the result)
```

## Tests

Integration tests against a real Postgres (a throwaway database per `go test` run, see `testdb_test.go`):
`e := newEnv(t)`, `c, email := e.signedIn("Nama")` (registered + session), `e.bidder("Nama")` (also email-verified),
`e.call(c, method, path, body)` → `resp{Status, Body, Header}` with `.code() .field() .message()`, `e.exec / e.scalar`
for direct SQL, `e.seedMarket(...)`, `e.frames(channel)` (queued realtime frames), `e.lastMail(to)`, `e.sms()`,
`e.server` (call jobs directly, e.g. `AuctionTick`). Tests share one database per run: make names/units unique per
test (no global counts). Server logs show up in the failing test's output.

Before committing: `gofmt -l .` (empty), `go vet ./...`, `go test ./... -count=1`, `make check-gen`.
