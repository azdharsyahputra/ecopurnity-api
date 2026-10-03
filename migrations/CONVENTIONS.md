# Database conventions

PostgreSQL 17 is the system of record (primary + async streaming replica). ClickHouse holds analytics only, fed from the
`outbox` table. The API contract is `api/openapi.yaml`; the reference behaviour is the frontend mock in
`/Users/csadeveloper/kkn/ecp/ecopurnity/src/mocks/*.ts` and the rules in `src/domain/*.ts`. The schema must be able to
serve every endpoint in the spec and enforce what the database can enforce cheaply.

## Files

`migrations/postgres/NNNNN_<area>.sql`, goose format (`-- +goose Up` / `-- +goose Down`, wrap plpgsql bodies in
`-- +goose StatementBegin/End`). Applied in number order by `go run ./cmd/migrate up`. Every file must have a working
Down that drops exactly what its Up created (reverse order). Down then Up again must succeed.

| File | Area | Owner |
| --- | --- | --- |
| 00001_foundation.sql | helpers, users, sessions, orgs (identity row), org_members, parties, notifications, audit_log, outbox | done |
| 00002_identity_governance.sql | participant identity, KYC, account governance, roles activation, fraud | agent A |
| 00003_economy.sql | listings, opportunities, markets, auctions, bids, matching | agent B |
| 00004_trade.sql | trades, money ledger, finance, disputes, RFQ, conversations, contracts, collective settlement | agent C |
| 00005_org.sql | org workspace: profile, roles, approvals, inventory, procurement, pools, org auctions, suppliers | agent D |

A file may reference tables from lower-numbered files only. A reference that would point forward (e.g. economy → trade)
is added by the later file with `ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY` (and dropped in its Down).

## Table registry (names are fixed: other files reference them without reading your file)

- **00001 (exists):** `users`, `user_capabilities`, `sessions`, `auth_tokens`, `orgs`, `org_members`, `parties`,
  `notifications`, `notification_prefs`, `audit_log`, `outbox`. Domains `idr`, `qty`, `category_id`. Functions
  `set_updated_at()`, `next_code(prefix)`.
- **00002 A:** `identities` (1:1 users: bio, availability, preferences), `capacity_items`, `phone_verifications`,
  `kyc_submissions` (KTP + selfie), `verification_requests` (admin queue, business and personal) and
  `verification_documents`, `user_reports`, `suspension_appeals`, `mm_applications`, `fraud_alerts`,
  `fraud_alert_subjects`, `fraud_alert_notes`.
- **00003 B:** `listings` (supply + demand in one table, `kind`), `listing_events`, `listing_attachments`, `opportunities`,
  `opportunity_participants`, `opportunity_follows`, `markets`, `market_rule_versions`, `market_participants`,
  `market_settings`, `market_flags`, `market_reports`, `mm_pipeline` (opportunity stage per maker), `market_disputes`
  (MM-level disputes), `auctions` (economy rounds and personal buyer auctions), `auction_qualifications`, `bids`,
  `auction_awards` / `auction_award_lines`, `matches` (per-user smart-match state), `watchlist`.
- **00004 C:** `trades` (one row per trade, `buyer_party_id`, `supplier_party_id`), `trade_acceptances`,
  `trade_events` (status timeline), `trade_documents`, `invoices`, `shipments`, `qc_results`, `reviews`, `disputes`,
  `dispute_evidence`, `dispute_events`, `ledger_accounts`, `ledger_entries`, `bank_accounts`, `withdrawals`,
  `settlements` (collective round settlement header) and `settlement_lines`, `rfqs`, `rfq_invitations`, `quotes`,
  `quote_events`, `conversations`, `conversation_participants`, `messages`, `supply_contracts`, `contract_orders`.
- **00005 D:** `org_profiles`, `org_documents`, `org_roles` (custom roles + permission matrix), `org_approval_rules`,
  `org_settings`, `inventory_items`, `supply_schedules`, `procurement_requests`, `procurement_approvals`,
  `collective_pools`, `pool_members`, `pool_markets`, `org_auctions`, `org_auction_lots`, `org_auction_approvals`,
  `org_auction_offers`, `org_awards`, `org_award_lines`, `purchase_orders`, `suppliers` (directory), `org_suppliers`
  (relation + own rating), `org_purchase_history`.

If you need a table that is not listed, prefix it with your area (`trade_…`, `org_…`, `market_…`) so names never collide,
and mention it in your report.

## Column rules

- Primary keys `id uuid DEFAULT gen_random_uuid()`. Public codes (`TRX-…`, `AUC-…`, `MKT-…`, `RFQ-…`, `CTR-…`, `PRQ-…`,
  `DSP-…`, `SUP-…`/`DEM-…` for listings) are `code text NOT NULL UNIQUE DEFAULT next_code('TRX')`.
- Timestamps `timestamptz`; every mutable table has `created_at` and `updated_at` with the `set_updated_at()` trigger.
- Money: `idr` domain (non-negative bigint Rupiah). Signed amounts (ledger) use `bigint`. Rates (fees) `numeric(6,5)`.
- Quantities: `<name>_value qty NOT NULL, <name>_unit text NOT NULL` (or `quantity`/`unit` when unambiguous).
- Enumerations: `text` + `CHECK (col IN (...))`, values exactly as the OpenAPI enum. No Postgres ENUM types.
- Status machines are enforced in the application (rules in the frontend's `src/domain`); the database enforces the
  value set, the invariants that are cheap (`quantity > 0`, `buyer <> supplier`, one active X per Y via partial unique
  indexes) and referential integrity.
- Foreign keys: name the column `<thing>_id`; `ON DELETE CASCADE` only for rows that are meaningless without the parent
  (child lines, memberships); otherwise default (restrict). Who did something: `<verb>_by uuid REFERENCES users(id)`.
- Flexible but schema-less blobs (`jsonb`) only for genuinely open shapes (audit `changes`, rule snapshots, OCR fields,
  form snapshots). Anything the API filters, sorts or joins on is a real column.
- Index every FK used in a lookup, every list's filter + sort (`(owner, created_at DESC)`), and partial indexes for queues
  (`WHERE status = 'pending'`).
- Comment non-obvious columns with `--`; add `COMMENT ON TABLE` for every table (one sentence: what a row is).
- Party columns (`buyer_party_id`, `supplier_party_id`, ...) reference `parties(id)`; a user or org gets its party row on
  first use (upsert by `user_id` / `org_id`).
- Audit: no per-table audit columns beyond `created_by/updated_by` where the API exposes them; history goes to `audit_log`.
- The `market_id` column of `audit_log` gets its FK in 00003 (`ALTER TABLE audit_log ADD CONSTRAINT ... REFERENCES markets`).

## Testing your file

The local cluster runs from `docker compose` (`make up`). Primary: `postgres://ecopurnity:ecopurnity@localhost:5432/ecopurnity`.
Do NOT migrate the shared `ecopurnity` database. Create your own scratch database and apply the files in order:

```bash
docker exec ecopurnity-postgres-primary-1 psql -U ecopurnity -d postgres -c "CREATE DATABASE scratch_<area>"
for f in migrations/postgres/0000{1,2,3,4,5}_*.sql; do ...; done   # only files that exist and are <= yours
```

The simplest way: `POSTGRES_PRIMARY_URL=postgres://ecopurnity:ecopurnity@localhost:5432/scratch_<area>?sslmode=disable
go run ./cmd/migrate up`, then `down` back to just below your version and `up` again. Lower-numbered files written by
other agents may not exist yet while you work; if yours references their tables, create minimal stub tables in your
scratch database by hand (never in the migration file) so your file applies, and list the references in your report.
Drop your scratch database when done.
