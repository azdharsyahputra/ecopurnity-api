# Trade engine

`applyTradeAction(ctx, tx, tradeID, actor, in)` in `internal/server/trade_engine.go` runs every step of the trade
settlement flow. It is shared by:

- `POST /me/transactions/{id}/actions` and `POST /orgs/{orgId}/transactions/{tid}/actions` (every action but `pay`);
- the payment gateway settlement (`payments.go`: `pay`, once per settled payment);
- the external counterparty bot (`trade_clock.go`).

## Contract

- **The caller authorizes.** It has established that `actor` may act for `actor.Side` of this trade (personal: the
  session user's party is that side; org: the member's role permission; bot: the side's party is external). The engine
  never looks at sessions.
- It runs inside the caller's transaction `tx` and locks the trade row (`FOR UPDATE`) itself.
- It returns `*Error` for the API contract: `404 not_found` (no such trade), `409 invalid_transition` (the action is not
  open to that side in the current state; rules in `trade_rules.go`), `422 validation` (per-action fields). Any other
  error is internal and the caller rolls back.
- On success, in the same transaction: the trade, its child rows (acceptances, invoice, shipments, documents, QC,
  dispute and evidence, reviews), the ledger journals below, a `trade_events` row and a `trade.status` analytics fact
  when the status changed, an audit entry (reason = note), a notification to the users behind the other side, and a
  `trade.updated` frame on `user:{id}` for the users behind both sides (`fanoutTrade`: a user party's user, an org
  party's active members).

## Ledger postings

`B` = what the buyer pays, `S` = subtotal; fees are charged on the current subtotal. `finance.go` explains the signs.

| Step | Journal | Kind / label |
| --- | --- | --- |
| `pay`, escrow terms | `bank_clearing +B` / `escrow(buyer) −B` | escrow, "Bayar TRX · title" |
| `confirm_receipt` partial, escrow | first refund the short quantity incl. PPN: `escrow(buyer) +r` / `wallet_available(buyer) −r` | refund, "Refund TRX" |
| release `R` of the escrow | `escrow(buyer) +R` / `wallet_available(supplier) −S` / `ppn_payable(supplier) −(R−S)` | payout, "Pencairan TRX · title" |
| fees after a release | `wallet_available(supplier) +(pf+mf)` / `platform_revenue −pf` / `maker_commission(maker) −mf` (no maker party: the maker fee goes to `platform_revenue`) | fee, "Fee platform … TRX" |
| `pay`, net terms (after acceptance) | `bank_clearing +B` / `escrow(buyer) −B`, then release `R = B` as above | payment |

- `confirm_receipt` accepted or partial on escrow terms releases the escrow (after the partial refund).
- `cancel` never moves money: it is only open before an escrow payment (agreement / invoiced).
- QC rejected or a dispute keeps the escrow held until the admin decision (`settleDisputeResolution`), which refunds
  and/or releases with the same journals.
- Withdrawals: requesting moves the wallet (and collected PPN) to `payout_pending`; an admin marking it paid posts
  `payout_pending +A` / `bank_clearing −A`; a rejection reverses the request's journal.
