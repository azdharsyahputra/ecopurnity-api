package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Trade action engine (PRD F6 settlement flow), shared by POST /me/transactions/{id}/actions and
// POST /orgs/{orgId}/transactions/{tid}/actions (every action but `pay`), the payment gateway settlement (payments.go:
// `pay`, once per settled payment) and the external counterparty bot (trade_clock.go).
//
// Contract of applyTradeAction(ctx, tx, tradeID, actor, in):
//   - The CALLER authorizes: it has established that `actor` may act for `actor.Side` of this trade (personal: the
//     session user's party is that side; org: the member's role permission; bot: the side's party is external).
//     The engine never looks at sessions.
//   - It runs inside the caller's transaction `tx` and locks the trade row (FOR UPDATE) itself.
//   - It returns *Error for the API contract: 404 not_found (no such trade), 409 invalid_transition (action not open
//     to that side in the current state, rules in trade_rules.go), 422 validation (per-action fields). Any other error
//     is internal; the caller rolls back.
//   - On success, in the same transaction: the trade, its child rows (acceptances, invoice, shipments, documents, QC,
//     dispute + evidence, reviews), the ledger journals below, a trade_events row and a `trade.status` analytics fact
//     when the status changed, an audit entry (reason = note), a notification to the users behind the other side, and a
//     `trade.updated` frame on `user:{id}` for the users behind both sides (fanoutTrade: a user party's user, an org
//     party's active members).
//
// Ledger postings (finance.go explains signs; B = buyer pays, S = subtotal, fees on the current subtotal):
//   - pay, escrow terms:  bank_clearing +B / escrow(buyer) −B                        [escrow]  "Bayar TRX · title"
//   - confirm_receipt accepted/partial, escrow: release (below); partial first refunds the short quantity incl. PPN:
//     escrow(buyer) +r / wallet_available(buyer) −r                                    [refund]  "Refund TRX"
//   - release R of the escrow:  escrow(buyer) +R / wallet_available(supplier) −S / ppn_payable(supplier) −(R−S)
//                                                                                      [payout]  "Pencairan TRX · title"
//     then fees: wallet_available(supplier) +(pf+mf) / platform_revenue −pf / maker_commission(maker) −mf
//                                                                                      [fee]     "Fee platform … TRX"
//     (no maker party → the maker fee goes to platform_revenue)
//   - pay, net terms (after acceptance): bank_clearing +B / escrow(buyer) −B [payment], then release R = B as above.
//   - cancel never moves money: it is only open before an escrow payment (agreement/invoiced).
//   - QC rejected / dispute: the escrow stays held until the admin decision (settleDisputeResolution): refund and/or
//     release with the same journals.

type tradeActor struct {
	Side   string  // buyer | supplier
	UserID *string // nil for the external-party bot
	Name   string  // shown in audit, evidence, reviews, notifications
	OrgID  *string // the org acting through a member (audit org_id: the org's activity on the trade)
}

type tradeRow struct {
	ID, Code, Title           string
	Party                     map[string]string  // side -> party id
	User                      map[string]*string // side -> user id (nil: org or external party)
	PartyName                 map[string]string
	Quantity                  float64
	Unit                      string
	UnitPrice, Total          int64
	Terms, Status             string
	MakerRate, PlatformRate   float64
	MarketID, MakerParty      *string
	PaymentStatus             string // invoice status, unpaid without an invoice
	Scheduled                 float64
	OpenShipments             int
	AcceptedBuyer, AcceptedSu bool
	ReviewedBuyer, ReviewedSu bool
}

func (t tradeRow) state() tradeState {
	signed := t.Status != "agreement"
	return tradeState{Status: t.Status, Terms: t.Terms,
		Agreement:      map[string]bool{"buyer": signed || t.AcceptedBuyer, "supplier": signed || t.AcceptedSu},
		UnscheduledQty: math.Max(0, t.Quantity-t.Scheduled), OpenShipments: t.OpenShipments,
		Reviewed: map[string]bool{"buyer": t.ReviewedBuyer, "supplier": t.ReviewedSu}}
}

func (t tradeRow) label() string { return t.Code + " · " + t.Title }

var errTradeNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Transaksi tidak ditemukan"}

// lockTrade loads a trade with everything the rules need and locks the trade row.
func lockTrade(ctx context.Context, q dbtx, id string) (tradeRow, error) {
	t := tradeRow{Party: map[string]string{}, User: map[string]*string{}, PartyName: map[string]string{}}
	if !isUUID(id) {
		return t, errTradeNotFound
	}
	var bp, sp, bn, sn string
	var bu, su *string
	err := q.QueryRow(ctx, `
		SELECT t.id::text, t.code, t.title, t.buyer_party_id::text, t.supplier_party_id::text, bp.user_id::text, sp.user_id::text, bp.name, sp.name,
		       t.quantity::float8, t.unit, t.unit_price_idr, t.total_idr, t.terms, t.status, t.maker_fee_rate::float8, t.platform_fee_rate::float8,
		       t.market_id::text, m.maker_party_id::text,
		       coalesce((SELECT status FROM invoices WHERE trade_id = t.id), 'unpaid'),
		       coalesce((SELECT sum(quantity)::float8 FROM shipments WHERE trade_id = t.id), 0),
		       (SELECT count(*) FROM shipments WHERE trade_id = t.id AND status <> 'delivered'),
		       EXISTS (SELECT 1 FROM trade_acceptances WHERE trade_id = t.id AND side = 'buyer'),
		       EXISTS (SELECT 1 FROM trade_acceptances WHERE trade_id = t.id AND side = 'supplier'),
		       EXISTS (SELECT 1 FROM reviews WHERE trade_id = t.id AND side = 'buyer'),
		       EXISTS (SELECT 1 FROM reviews WHERE trade_id = t.id AND side = 'supplier')
		FROM trades t JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
		LEFT JOIN markets m ON m.id = t.market_id
		WHERE t.id = $1 FOR UPDATE OF t`, id).Scan(&t.ID, &t.Code, &t.Title, &bp, &sp, &bu, &su, &bn, &sn,
		&t.Quantity, &t.Unit, &t.UnitPrice, &t.Total, &t.Terms, &t.Status, &t.MakerRate, &t.PlatformRate, &t.MarketID, &t.MakerParty,
		&t.PaymentStatus, &t.Scheduled, &t.OpenShipments, &t.AcceptedBuyer, &t.AcceptedSu, &t.ReviewedBuyer, &t.ReviewedSu)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errTradeNotFound
	}
	t.Party["buyer"], t.Party["supplier"] = bp, sp
	t.User["buyer"], t.User["supplier"] = bu, su
	t.PartyName["buyer"], t.PartyName["supplier"] = bn, sn
	return t, err
}

func fieldErr(field, msg string) error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: msg, Fields: map[string]string{field: msg}}
}

var errInvalidTransition = &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Aksi ini tidak tersedia untuk status sekarang"}

// applyTradeAction applies one action for `actor` (see the contract at the top of this file).
func applyTradeAction(ctx context.Context, tx pgx.Tx, tradeID string, actor tradeActor, in api.TradeActionInput) error {
	t, err := lockTrade(ctx, tx, tradeID)
	if err != nil {
		return err
	}
	s := t.state()
	side, action := actor.Side, string(in.Action)
	if !slices.Contains(tradeActions(s, side), action) {
		return errInvalidTransition
	}
	note := tradeActionLabel[action]
	inputNote := strings.TrimSpace(deref(in.Note))
	file := strings.TrimSpace(deref(in.File))
	codeTail := strings.TrimPrefix(t.Code, "TRX-")
	doc := func(kind, name string) (string, error) {
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO trade_documents (trade_id, kind, name, uploaded_by) VALUES ($1, $2, $3, $4) RETURNING id`,
			t.ID, kind, name, actor.UserID).Scan(&id)
		return id, err
	}
	allDelivered := false
	qc := ""
	eventNote := "" // the timeline note when it differs from note (pay: the payment reference)

	switch action {
	case "accept_agreement":
		if _, err := tx.Exec(ctx, `INSERT INTO trade_acceptances (trade_id, side, accepted_by) VALUES ($1, $2, $3)`, t.ID, side, actor.UserID); err != nil {
			return err
		}
		if _, err := doc("agreement", fmt.Sprintf("Agreement-%s-%s.pdf", codeTail, side)); err != nil {
			return err
		}
		note = "Agreement disetujui"

	case "issue_invoice":
		due := time.Now().Add(3 * day)
		if t.Terms != "escrow" {
			due = time.Now().Add(time.Duration(termsDays[t.Terms]+5) * day)
		}
		m := breakdown(t.Total, t.PlatformRate, t.MakerRate)
		number := "INV-" + codeTail
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoices (trade_id, number, due_at, subtotal_idr, ppn_idr, buyer_pays_idr, platform_fee_idr, maker_fee_idr, supplier_receives_idr)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t.ID, number, due, m.Subtotal, m.VAT, m.BuyerPays, m.PlatformFee, m.MakerFee, m.SupplierReceives); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE trades SET due_at = $2 WHERE id = $1`, t.ID, due); err != nil {
			return err
		}
		if _, err := doc("invoice", number+".pdf"); err != nil {
			return err
		}

	case "pay":
		// Reached from a settled gateway payment (payments.go; note = its reference) or the demo bot; users cannot
		// send `pay` (the handlers answer 409 payment_required).
		b := breakdown(t.Total, t.PlatformRate, t.MakerRate).BuyerPays
		kind, status := "escrow", "escrow"
		note = "Dana masuk escrow"
		if t.Terms != "escrow" {
			kind, status, note = "payment", "released", "Pembayaran diterima supplier"
		}
		if inputNote != "" {
			eventNote = note + " · " + inputNote
		}
		acc, err := accounts(ctx, tx, "bank_clearing", "escrow:"+t.Party["buyer"])
		if err != nil {
			return err
		}
		if err := post(ctx, tx, journal{Label: "Bayar " + t.label(), TradeID: &t.ID, CreatedBy: actor.UserID, Lines: []ledgerLine{
			{acc["bank_clearing"], b, kind}, {acc["escrow:"+t.Party["buyer"]], -b, kind}}}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invoices SET status = $2, paid_at = now() WHERE trade_id = $1`, t.ID, status); err != nil {
			return err
		}
		if t.Terms != "escrow" {
			if err := releaseEscrow(ctx, tx, t, b, t.Total, actor.UserID); err != nil {
				return err
			}
		}

	case "ship":
		sh := in.Shipment
		unscheduled := s.UnscheduledQty
		if sh == nil || !(sh.Quantity > 0) {
			return fieldErr("quantity", "Isi kuantitas pengiriman")
		}
		if sh.Quantity > unscheduled+1e-9 {
			return fieldErr("quantity", fmt.Sprintf("Maksimal %s %s", qtyLabel(unscheduled), t.Unit))
		}
		drop := strings.TrimSpace(sh.DropPoint)
		if drop == "" {
			return fieldErr("dropPoint", "Isi titik tujuan")
		}
		carrier := nonEmpty(strings.TrimSpace(deref(sh.Carrier)), carrierDflt)
		at := time.Now()
		if sh.ScheduledAt != nil {
			at = *sh.ScheduledAt
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shipments (trade_id, quantity, drop_point, carrier, scheduled_at, status) VALUES ($1, $2, $3, $4, $5, 'in_transit')`,
			t.ID, sh.Quantity, drop, carrier, at); err != nil {
			return err
		}
		note = fmt.Sprintf("Kirim %s %s ke %s", qtyLabel(sh.Quantity), t.Unit, drop)

	case "upload_proof":
		var shipID, drop string
		var qty float64
		err := tx.QueryRow(ctx, `
			SELECT id::text, quantity::float8, drop_point FROM shipments WHERE trade_id = $1 AND status <> 'delivered'
			ORDER BY (id::text = $2) DESC, scheduled_at, created_at LIMIT 1`, t.ID, deref(in.ShipmentId)).Scan(&shipID, &qty, &drop)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusConflict, Code: "invalid_transition", Message: "Tidak ada pengiriman yang sedang berjalan"}
		} else if err != nil {
			return err
		}
		if file == "" {
			return fieldErr("file", "Pilih file bukti pengiriman")
		}
		docID, err := doc("proof", file)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE shipments SET status = 'delivered', delivered_at = now(), proof_document_id = $2 WHERE id = $1`, shipID, docID); err != nil {
			return err
		}
		allDelivered = s.OpenShipments == 1 && s.UnscheduledQty <= 1e-9
		note = fmt.Sprintf("Terkirim %s %s ke %s", qtyLabel(qty), t.Unit, drop)

	case "confirm_receipt":
		qc = "accepted"
		var qcNote string
		var acceptedIn *float64
		if in.Qc != nil {
			qc, qcNote, acceptedIn = string(in.Qc.Outcome), strings.TrimSpace(deref(in.Qc.Note)), in.Qc.AcceptedQty
		}
		accepted := t.Quantity
		switch qc {
		case "rejected":
			accepted = 0
		case "partial":
			accepted = math.Min(t.Quantity, math.Max(0, derefF(acceptedIn)))
			if !(accepted > 0 && accepted < t.Quantity) {
				return fieldErr("acceptedQty", "Antara 1 dan "+qtyLabel(t.Quantity-1))
			}
		}
		if qc != "accepted" && qcNote == "" {
			return fieldErr("note", "Jelaskan temuan QC")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO qc_results (trade_id, outcome, accepted_quantity, note, checked_by) VALUES ($1, $2, $3, nullif($4, ''), $5)`,
			t.ID, qc, accepted, qcNote, actor.UserID); err != nil {
			return err
		}
		switch qc {
		case "rejected":
			if err := openDispute(ctx, tx, t, actor, "QC menolak barang: "+qcNote, qcNote, ""); err != nil {
				return err
			}
			note = "Barang ditolak saat QC, dispute dibuka"
		case "partial":
			var refund int64
			if t.PaymentStatus == "escrow" {
				refund = partialRefund(t.UnitPrice, t.Quantity, accepted)
			}
			if _, err := tx.Exec(ctx, `UPDATE trades SET quantity = $2::numeric, total_idr = round($2::numeric * unit_price_idr)::bigint WHERE id = $1`, t.ID, accepted); err != nil {
				return err
			}
			t.Quantity, t.Total = accepted, round(accepted*float64(t.UnitPrice))
			note = fmt.Sprintf("Diterima sebagian (%s %s)", qtyLabel(accepted), t.Unit)
			if refund > 0 {
				if err := refundEscrow(ctx, tx, t, refund, actor.UserID); err != nil {
					return err
				}
				note += ", refund " + rupiah(refund)
			}
		default:
			note = "Barang diterima"
		}
		if qc != "rejected" {
			if t.Terms == "escrow" {
				held, err := escrowHeld(ctx, tx, t.ID)
				if err != nil {
					return err
				}
				if err := releaseEscrow(ctx, tx, t, held, t.Total, actor.UserID); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE trades SET due_at = now() + $2::interval WHERE id = $1`, t.ID, fmt.Sprintf("%d days", termsDays[t.Terms])); err != nil {
				return err
			}
		}

	case "cancel":
		// Open only in agreement/invoiced, i.e. before any escrow payment: nothing to refund.

	case "dispute":
		if inputNote == "" {
			return fieldErr("note", "Jelaskan alasan dispute")
		}
		if err := openDispute(ctx, tx, t, actor, inputNote, inputNote, file); err != nil {
			return err
		}

	case "add_evidence":
		if inputNote == "" {
			return fieldErr("note", "Tulis keterangan bukti")
		}
		var disputeID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM disputes WHERE trade_id = $1 AND status <> 'resolved' FOR UPDATE`, t.ID).Scan(&disputeID); errors.Is(err, pgx.ErrNoRows) {
			return errInvalidTransition
		} else if err != nil {
			return err
		}
		if err := addEvidence(ctx, tx, disputeID, actor, inputNote, file); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE disputes SET status = 'evidence' WHERE id = $1 AND status = 'open'`, disputeID); err != nil {
			return err
		}
		note = "Bukti dispute ditambahkan"
		if _, err := tx.Exec(ctx, `INSERT INTO dispute_events (dispute_id, actor_user_id, actor_label, label) VALUES ($1, $2, $3, $4)`,
			disputeID, actor.UserID, actor.Name, note); err != nil {
			return err
		}

	case "review":
		r := in.Review
		if r == nil || r.Rating < 1 || r.Rating > 5 {
			return fieldErr("rating", "Beri rating 1–5")
		}
		for f, v := range map[string]float64{"quality": r.Quality, "timeliness": r.Timeliness, "communication": r.Communication} {
			if v < 1 || v > 5 {
				return fieldErr(f, "Beri nilai 1–5")
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO reviews (trade_id, side, rating, quality, timeliness, communication, text, reviewed_by)
			VALUES ($1, $2, $3, round($4::numeric, 1), round($5::numeric, 1), round($6::numeric, 1), $7, $8)`,
			t.ID, side, r.Rating, r.Quality, r.Timeliness, r.Communication, strings.TrimSpace(r.Text), actor.UserID); err != nil {
			return err
		}
		note = fmt.Sprintf("Ulasan %d/5", r.Rating)
	}

	before := t.Status
	after := nextStatus(s, action, allDelivered, qc)
	if _, err := tx.Exec(ctx, `UPDATE trades SET status = $2 WHERE id = $1`, t.ID, after); err != nil {
		return err
	}
	var changes []change
	if after != before {
		changes = []change{{Field: "Status", Before: &before, After: after}}
		if _, err := tx.Exec(ctx, `INSERT INTO trade_events (trade_id, status, note, actor_user_id) VALUES ($1, $2, $3, $4)`, t.ID, after, nonEmpty(eventNote, note), actor.UserID); err != nil {
			return err
		}
		if err := tradeStatusChanged(ctx, tx, t.ID, after); err != nil {
			return err
		}
	}
	var reason *string
	if inputNote != "" {
		reason = &inputNote
	}
	if err := writeAudit(ctx, tx, audit{ActorUserID: actor.UserID, ActorLabel: actor.Name, Action: note, EntityType: "transaction",
		EntityID: t.ID, EntityLabel: t.label(), OrgID: actor.OrgID, MarketID: t.MarketID, Reason: reason, Changes: changes}); err != nil {
		return err
	}
	typ := "transaction_update"
	switch action {
	case "pay":
		typ = "payment"
	case "ship", "upload_proof":
		typ = "delivery"
	}
	return fanoutTrade(ctx, tx, t.ID, otherSide(side), notification{Type: typ, Title: t.Code + ": " + note, Body: actor.Name + " · " + t.Title,
		Href: "/app/transactions/" + t.ID})
}

// emitTradeUpdated sends `trade.updated` to everyone behind both parties of the trade (no notification).
func emitTradeUpdated(ctx context.Context, q dbtx, tradeID string) error {
	return fanoutTrade(ctx, q, tradeID, "", notification{})
}

// fanoutTrade sends `trade.updated` to the users behind both parties (a user party's user, an org party's active
// members) and notification n to the ones behind side `notifySide` ("" = nobody).
func fanoutTrade(ctx context.Context, q dbtx, tradeID, notifySide string, n notification) error {
	type party struct{ side, user, org string }
	rows, err := q.Query(ctx, `
		SELECT 'buyer', coalesce(p.user_id::text, ''), coalesce(p.org_id::text, '') FROM trades t JOIN parties p ON p.id = t.buyer_party_id WHERE t.id = $1
		UNION ALL
		SELECT 'supplier', coalesce(p.user_id::text, ''), coalesce(p.org_id::text, '') FROM trades t JOIN parties p ON p.id = t.supplier_party_id WHERE t.id = $1`, tradeID)
	if err != nil {
		return err
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (party, error) {
		var p party
		return p, r.Scan(&p.side, &p.user, &p.org)
	})
	if err != nil {
		return err
	}
	for _, p := range ps {
		m := notification{}
		if p.side == notifySide {
			m = n
		}
		switch {
		case p.user != "":
			err = notifyTradeUser(ctx, q, p.user, tradeID, m)
		case p.org != "":
			err = orgTradeFanout(ctx, q, p.org, tradeID, m)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// orgTradeFanout notifies (when n has a title; linked to the org's transaction page) and sends `trade.updated` to every
// active member of an org whose role may see transactions (owner, or transactions.view).
func orgTradeFanout(ctx context.Context, q dbtx, orgID, tradeID string, n notification) error {
	if n.Title != "" {
		n.Href = "/org/" + orgID + "/transactions/" + tradeID
	}
	rows, err := q.Query(ctx, `
		SELECT m.user_id::text FROM org_members m JOIN org_roles r ON r.org_id = m.org_id AND r.key = m.role
		WHERE m.org_id = $1 AND m.status = 'active' AND m.user_id IS NOT NULL AND (m.role = 'owner' OR 'transactions.view' = ANY(r.permissions))`, orgID)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := notifyTradeUser(ctx, q, u, tradeID, n); err != nil {
			return err
		}
	}
	return nil
}

func notifyTradeUser(ctx context.Context, q dbtx, userID, tradeID string, n notification) error {
	if n.Title != "" {
		if err := notify(ctx, q, userID, n); err != nil {
			return err
		}
	}
	var status string
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT status, updated_at FROM trades WHERE id = $1`, tradeID).Scan(&status, &at); err != nil {
		return err
	}
	return emitFrame(ctx, q, "user:"+userID, "trade.updated", nil, map[string]any{"transactionId": tradeID, "status": status, "updatedAt": at.UTC()})
}

// emitTradeFact publishes the trade snapshot (migrations/clickhouse/README.md, topic trade.status).
func emitTradeFact(ctx context.Context, q dbtx, tradeID, topic string) error {
	var payload []byte
	err := q.QueryRow(ctx, `
		SELECT json_build_object(
		  'code', t.code, 'status', t.status, 'item', t.title,
		  'via', CASE WHEN EXISTS (SELECT 1 FROM contract_orders WHERE trade_id = t.id) THEN 'contract'
		              WHEN EXISTS (SELECT 1 FROM settlement_lines WHERE trade_id = t.id) THEN 'settlement'
		              WHEN t.source_quote_id IS NOT NULL THEN 'rfq'
		              WHEN t.source_listing_id IS NOT NULL THEN 'direct'
		              WHEN a.type = 'dutch' THEN 'dutch'
		              WHEN t.auction_id IS NOT NULL THEN 'auction' ELSE 'direct' END,
		  'categoryId', coalesce(m.category_id, a.category_id, l.category_id, ''), 'region', coalesce(m.region, l.location, ''),
		  'marketId', t.market_id, 'auctionId', t.auction_id, 'buyerPartyId', t.buyer_party_id, 'supplierPartyId', t.supplier_party_id,
		  'buyerOrgId', bp.org_id, 'supplierOrgId', sp.org_id, 'quantity', t.quantity, 'unit', t.unit,
		  'unitPriceIdr', t.unit_price_idr, 'valueIdr', t.total_idr)
		FROM trades t JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
		LEFT JOIN markets m ON m.id = t.market_id LEFT JOIN auctions a ON a.id = t.auction_id LEFT JOIN listings l ON l.id = t.source_listing_id
		WHERE t.id = $1`, tradeID).Scan(&payload)
	if err != nil {
		return err
	}
	return emit(ctx, q, topic, tradeID, payload)
}

// settleDisputeResolution applies an admin dispute decision to its trade, in the admin's transaction after the dispute
// row is resolved (admin_disputes.go). Escrowed money: refund d.RefundIdr plus its PPN to the buyer's wallet and release
// the rest of the escrow to the supplier for a subtotal of d.ReleaseIdr (fees on that subtotal); the invoice becomes
// d.Payment. Net terms with nothing paid yet: a refund cancels (nothing to move, invoice stays unpaid), a release sends
// the trade to `accepted` with a fresh payment due date (the buyer still pays through `pay`), and a partial amount is
// refused (422 refundIdr: there is no money to split). The trade gets the status, a "Putusan dispute: …" event, a
// trade.status fact and trade.updated frames; the admin flow notifies the parties and writes the audit entry.
func (s *Server) settleDisputeResolution(ctx context.Context, tx pgx.Tx, d disputeSettlement) error {
	t, err := lockTrade(ctx, tx, d.TradeID)
	if err != nil {
		return err
	}
	held, err := escrowHeld(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	by := &d.ActorUserID
	status := d.TradeStatus
	switch {
	case held > 0:
		var refund int64
		switch d.Kind {
		case "refund":
			refund = held
		case "partial":
			refund = min(held, breakdown(d.RefundIdr, 0, 0).BuyerPays)
		}
		if refund > 0 {
			if err := refundEscrow(ctx, tx, t, refund, by); err != nil {
				return err
			}
		}
		if err := releaseEscrow(ctx, tx, t, held-refund, d.ReleaseIdr, by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invoices SET status = $2, paid_at = coalesce(paid_at, now()) WHERE trade_id = $1`, t.ID, d.Payment); err != nil {
			return err
		}
	case d.Kind == "partial":
		msg := "Belum ada dana di escrow untuk dibagi; pilih refund penuh atau lepas dana"
		return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: msg, Fields: map[string]string{"refundIdr": msg}}
	case d.Kind == "release" && t.PaymentStatus == "unpaid":
		status = "accepted"
		if _, err := tx.Exec(ctx, `UPDATE trades SET due_at = now() + $2::interval WHERE id = $1`, t.ID, fmt.Sprintf("%d days", termsDays[t.Terms])); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE trades SET status = $2 WHERE id = $1`, t.ID, status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO trade_events (trade_id, status, note, actor_user_id) VALUES ($1, $2, $3, $4)`,
		t.ID, status, "Putusan dispute: "+d.Note, by); err != nil {
		return err
	}
	if err := tradeStatusChanged(ctx, tx, t.ID, status); err != nil {
		return err
	}
	return emitTradeUpdated(ctx, tx, t.ID)
}

// tradeStatusChanged publishes a new trade status: the trade.status fact and, on completion, the public activity.
func tradeStatusChanged(ctx context.Context, tx pgx.Tx, tradeID, status string) error {
	if err := emitTradeFact(ctx, tx, tradeID, "trade.status"); err != nil {
		return err
	}
	if status != "completed" {
		return nil
	}
	var title string
	var total int64
	var market *string
	if err := tx.QueryRow(ctx, `SELECT title, total_idr, market_id::text FROM trades WHERE id = $1`, tradeID).Scan(&title, &total, &market); err != nil {
		return err
	}
	return emitActivity(ctx, tx, "transaction_completed", "Transaksi selesai: "+title, &total, market)
}

// ── Money movements ──────────────────────────────────────────────

func escrowHeld(ctx context.Context, q dbtx, tradeID string) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `
		SELECT coalesce(-sum(e.amount), 0) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.trade_id = $1 AND a.kind = 'escrow'`, tradeID).Scan(&v)
	return v, err
}

// releaseEscrow pays `amount` of the buyer's escrow to the supplier for a subtotal of `subtotal` (the rest of amount
// is PPN), then takes the platform and maker fees off the supplier's wallet. Marks the invoice released.
func releaseEscrow(ctx context.Context, q dbtx, t tradeRow, amount, subtotal int64, by *string) error {
	buyer, supplier := t.Party["buyer"], t.Party["supplier"]
	keys := []string{"escrow:" + buyer, "wallet_available:" + supplier, "ppn_payable:" + supplier, "platform_revenue"}
	if t.MakerParty != nil {
		keys = append(keys, "maker_commission:"+*t.MakerParty)
	}
	if amount <= 0 {
		return nil
	}
	acc, err := accounts(ctx, q, keys...)
	if err != nil {
		return err
	}
	if err := post(ctx, q, journal{Label: "Pencairan " + t.label(), TradeID: &t.ID, CreatedBy: by, Lines: []ledgerLine{
		{acc["escrow:"+buyer], amount, "payout"},
		{acc["wallet_available:"+supplier], -subtotal, "payout"},
		{acc["ppn_payable:"+supplier], -(amount - subtotal), "payout"},
	}}); err != nil {
		return err
	}
	m := breakdown(subtotal, t.PlatformRate, t.MakerRate)
	label := "Fee platform " + t.Code
	makerAcc := acc["platform_revenue"]
	if m.MakerFee > 0 {
		label = "Fee platform + market maker " + t.Code
		if t.MakerParty != nil {
			makerAcc = acc["maker_commission:"+*t.MakerParty]
		}
	}
	if err := post(ctx, q, journal{Label: label, TradeID: &t.ID, CreatedBy: by, Lines: []ledgerLine{
		{acc["wallet_available:"+supplier], m.PlatformFee + m.MakerFee, "fee"},
		{acc["platform_revenue"], -m.PlatformFee, "fee"},
		{makerAcc, -m.MakerFee, "fee"},
	}}); err != nil {
		return err
	}
	_, err = q.Exec(ctx, `UPDATE invoices SET status = 'released', paid_at = coalesce(paid_at, now()) WHERE trade_id = $1`, t.ID)
	return err
}

// refundEscrow returns `amount` of the buyer's escrow to the buyer's wallet (withdrawable).
func refundEscrow(ctx context.Context, q dbtx, t tradeRow, amount int64, by *string) error {
	buyer := t.Party["buyer"]
	acc, err := accounts(ctx, q, "escrow:"+buyer, "wallet_available:"+buyer)
	if err != nil {
		return err
	}
	return post(ctx, q, journal{Label: "Refund " + t.Code, TradeID: &t.ID, CreatedBy: by, Lines: []ledgerLine{
		{acc["escrow:"+buyer], amount, "refund"}, {acc["wallet_available:"+buyer], -amount, "refund"}}})
}

// ── Disputes ─────────────────────────────────────────────────────

func openDispute(ctx context.Context, tx pgx.Tx, t tradeRow, actor tradeActor, reason, evidence, file string) error {
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO disputes (trade_id, reason, opened_by_side, opened_by, market_id) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		t.ID, reason, actor.Side, actor.UserID, t.MarketID).Scan(&id); err != nil {
		return err
	}
	// The opening statement is the first evidence row; the "Dispute dibuka" step is derived from the dispute row
	// (admin case timeline), so no dispute_events row here.
	return addEvidence(ctx, tx, id, actor, evidence, file)
}

func addEvidence(ctx context.Context, tx pgx.Tx, disputeID string, actor tradeActor, text, file string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO dispute_evidence (dispute_id, side, author_user_id, author_name, text, file_name) VALUES ($1, $2, $3, $4, $5, nullif($6, ''))`,
		disputeID, actor.Side, actor.UserID, actor.Name, text, file)
	return err
}

// ── small helpers ────────────────────────────────────────────────

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// isUUID: path ids are compared against uuid columns; anything else is simply not found.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return false
		}
	}
	return true
}
