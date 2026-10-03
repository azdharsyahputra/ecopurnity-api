# Database design

PostgreSQL 17 (primary + async streaming replica) is the system of record. ClickHouse holds analytics only and is
rebuildable from Postgres. Migrations: `migrations/postgres` (goose, `make migrate`), `migrations/clickhouse`.
Every table with one line on what a row is: [database-tables.md](database-tables.md) (generated: `scripts/db-doc.sh`).
Authoring rules: [migrations/CONVENTIONS.md](../migrations/CONVENTIONS.md).

## Migrations

| File | Area | Main tables |
| --- | --- | --- |
| 00001_foundation | shared | users, sessions, orgs, org_members, parties, notifications, audit_log, outbox |
| 00002_identity_governance | participant identity, KYC, governance | identities, capacity_items, phone_verifications (unused: KYC is email + KTP only), kyc_submissions, verification_requests, suspension_appeals, mm_applications, fraud_alerts |
| 00003_economy | public economy | listings, opportunities, markets (+ rule versions, participants, operators), auctions, bids, auction_awards, matches |
| 00004_trade | settlement, money, chat | trades (+ acceptances, events, invoices, shipments, qc, reviews), disputes, ledger, withdrawals, settlements, rfqs/quotes, conversations/messages, supply_contracts |
| 00005_org | business workspace | org_profiles, org_roles, approval rules, inventory, procurement_requests, collective_pools, org_auctions, purchase_orders, suppliers |
| 00014_org_uploads | business workspace | uploads purpose `org_document`; org_documents.object_key (verification files are uploads) |
| 00020_payments | payments | payments (gateway attempts per invoice: Midtrans order id, instructions, status; one pending per invoice) |
| 00021_bid_capacity | auctions | bids.capacity (quantity a supplier can deliver with a reverse/sealed bid; NULL = whole lot; owner and bidder only) |
| 00022_org_approval_on_behalf | business workspace | procurement_approvals / org_auction_approvals.on_behalf (owner signed for a required role with no active member) |
| 00023_org_purchase_history_outbox | business workspace | trigger: org_purchase_history rows queue trade.status (+ auction.closed) facts for ClickHouse org_purchase_monthly |

## Key decisions

**Parties.** Every side of a trade, quote, contract, bid, listing or dispute is a `parties` row: a user, an org, or an
external party not on the platform. A user or org gets its party on first use. This lets the same tables serve
personal trades, org trades, and counterparties that are not (yet) users.

**One row per trade.** The frontend mock keeps a buyer copy and a supplier copy of each trade and mirrors them. The
database has one `trades` row with `buyer_party_id` and `supplier_party_id`; per-side state (agreement acceptance,
review, dispute evidence) lives in child tables keyed by side. "My transactions" is a query over both party columns.

**Money is a double-entry ledger.** `ledger_entries` are append-only; a deferred constraint trigger rejects any
journal that does not sum to zero at COMMIT. Balances (available, escrow, receivable) are sums over entries. Invoices
freeze their breakdown at issue (PPN 11% paid by the buyer; platform 1% and maker fee deducted from the supplier;
CHECKs tie the numbers together). PPN is booked to the supplier's `ppn_payable` (the supplier remits it); confirm with
finance/tax before launch.

**Status machines live in the application.** The database enforces the value set (`text` + `CHECK`), cheap invariants
(quantities > 0, buyer ≠ supplier, one open dispute per trade, one pending application per user, decision fields set
exactly when a status is decided) and referential integrity. Transitions follow the frontend's `src/domain` rules,
which have tests and can be ported directly.

**Realtime ordering.** `bids.seq` and `messages.seq` are per-channel counters taken from `auctions.last_seq` and
`conversations.message_seq` inside the insert trigger. The counter update locks the parent row, so seq is gapless
and in commit order: a websocket client that saw seq N resumes with `seq > N` and never misses a row. Contract:
[realtime.md](realtime.md), `api/asyncapi.yaml`.

**Audit and outbox.** `audit_log` is append-only (trigger) and written in the same transaction as the change. Domain
events go to `outbox` in the same transaction; a publisher sends them to ClickHouse and the realtime hub.

**Sensitive data.** NIK and bank account numbers are encrypted by the application (bytea) with a keyed hash for
uniqueness/lookup; only the last 4 digits of an account are stored in clear. Session and one-time tokens are stored
as SHA-256 hashes. Passwords: `password_hash` (argon2id in the app).

**Org permissions.** `org_roles.permissions` is a `text[]` of `module.action`; built-in roles get a row per org
(inserted by `POST /orgs`), so `org_members.role` has a real FK and roles stay editable per org.

## Rules the application must follow

- Writes and read-your-writes go to the primary (`db.Cluster.Primary()`); everything else to `Reader()`.
- Setting `users.status = 'suspended'` sets `users.suspended_at` in the same update (appeals are one per suspension).
- Chat send: catch unique violation 23505 on `(author_user_id, client_msg_id)` and return the existing message;
  never `ON CONFLICT DO NOTHING` (the seq trigger would have burned a number).
- Withdrawals lock the party's `ledger_accounts` row `FOR UPDATE` before checking the available balance.
- Identity PUT upserts `capacity_items` (deleting them cascades to the user's smart-match state).
- `POST /orgs` inserts the five built-in `org_roles` rows in the same transaction.
- Do not put a NIK into any jsonb column (forms, OCR fields); the admin view decrypts it from `kyc_submissions`.
