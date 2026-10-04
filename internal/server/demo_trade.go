package server

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type demoLine struct {
	key    string
	amount int64
	kind   string
}

func (g *demoGen) journal(label string, at time.Time, tradeID, withdrawalID, by *string, lines ...demoLine) {
	var sum int64
	for _, l := range lines {
		sum += l.amount
	}
	if sum != 0 {
		g.err = fmt.Errorf("journal %q does not balance: %d", label, sum)
		return
	}
	j := newID()
	for _, l := range lines {
		if l.amount == 0 {
			continue
		}
		acc, ok := g.acct[l.key]
		if !ok {
			g.err = fmt.Errorf("no ledger account %s", l.key)
			return
		}
		g.ins("ledger_entries", "journal_id, account_id, amount, kind, label, trade_id, withdrawal_id, created_by, created_at", j, acc, l.amount, l.kind,
			label, tradeID, withdrawalID, by, at)
		if kind, party, _ := strings.Cut(l.key, ":"); party != "" && (kind == "wallet_available" || kind == "ppn_payable") {
			g.avail[party] = append(g.avail[party], demoMove{at, -l.amount})
		}
	}
}

type demoTrade struct {
	ID, Code, Title           string
	Buyer, Supplier           *demoParty
	Qty                       float64
	Unit                      string
	Price, Total              int64
	Terms                     string
	Maker                     *demoParty
	MarketID, AuctionID       *string
	ListingID, QuoteID        *string
	GroupLabel                *string
	GroupShare                float64
	Address                   string
	Start                     time.Time
	Via, Category, Region     string
	Item                      string
	BuyerOrgID, SupplierOrgID *string
	Budget, MarketUnit        int64
	Status                    string
	Done                      time.Time
}

func (t *demoTrade) makerRate() float64 {
	if t.Maker != nil {
		return 0.005
	}
	return 0
}

func (g *demoGen) tradeFact(t *demoTrade, status string, qty float64, total int64, at time.Time) {
	p := map[string]any{"code": t.Code, "status": status, "via": t.Via, "item": t.Title, "categoryId": t.Category, "region": t.Region,
		"marketId": t.MarketID, "auctionId": t.AuctionID, "buyerPartyId": t.Buyer.ID, "supplierPartyId": t.Supplier.ID, "buyerOrgId": t.Buyer.OrgID,
		"supplierOrgId": t.Supplier.OrgID, "quantity": qty, "unit": t.Unit, "unitPriceIdr": t.Price, "valueIdr": total}
	if status == "agreement" {
		if t.Item != "" {
			p["item"] = t.Item
		}
		p["budgetUnitIdr"], p["marketUnitIdr"] = t.Budget, t.MarketUnit
		p["buyerOrgId"], p["supplierOrgId"] = t.BuyerOrgID, t.SupplierOrgID
	}
	g.fact("trade.status", t.ID, at, p)
}

func (g *demoGen) trade(t *demoTrade) {
	if t.ID == "" {
		t.ID, t.Code = newID(), g.code("TRX")
	}
	if t.Terms == "" {
		t.Terms = "escrow"
	}
	t.Total = round(t.Qty * float64(t.Price))
	tail := strings.TrimPrefix(t.Code, "TRX-")
	q := math.Min(t.Buyer.Q, t.Supplier.Q)
	slow := 0.4 + (1-q)*2.2
	step := func(from time.Time, a, b float64) time.Time { return g.workHours(from.Add(hrs(g.f(a, b) * slow))) }
	escrow := t.Terms == "escrow"
	buyerBy, supplierBy := t.Buyer.actorID(), t.Supplier.actorID()

	outcome := ""
	switch r := g.r.Float64(); {
	case r < 0.022+(1-q)*0.07:
		outcome = "cancel"
	case escrow && r < 0.04+(1-q)*0.13:
		outcome = "dispute"
	case escrow && r < 0.06+(1-q)*0.16:
		outcome = "partial"
	}

	type child struct {
		sql  string
		args []any
	}
	var kids []child
	add := func(table, cols string, args ...any) {
		ph := make([]string, len(args))
		for i := range args {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		kids = append(kids, child{"INSERT INTO " + table + " (" + cols + ") VALUES (" + strings.Join(ph, ", ") + ")", args})
		g.n[table]++
	}
	var journals []func()
	var facts []func()
	status, last := "agreement", t.Start
	due := t.Start.Add(14 * day)
	qty, total := t.Qty, t.Total
	event := func(st, note string, at time.Time, by *string) {
		status, last = st, at
		add("trade_events", "trade_id, status, at, note, actor_user_id", t.ID, st, at, note, by)
		q, tot := qty, total
		facts = append(facts, func() { g.tradeFact(t, st, q, tot, at) })
		if st == "completed" {
			facts = append(facts, func() {
				g.activity("transaction_completed", "Transaksi selesai: "+t.Title, idrPtr(tot), t.MarketID, at)
			})
		}
	}
	reached := func(at time.Time) bool { return !at.After(g.now) }

	add("trade_documents", "trade_id, kind, name, uploaded_by, created_at", t.ID, "order", "PO-"+tail+".pdf", buyerBy, t.Start)
	event("agreement", "Transaksi dibuat", t.Start, buyerBy)
	accB, accS := step(t.Start, 0.3, 10), step(t.Start, 0.3, 16)
	var accepted []time.Time
	for _, a := range []struct {
		side string
		at   time.Time
		by   *string
	}{{"buyer", accB, buyerBy}, {"supplier", accS, supplierBy}} {
		if reached(a.at) {
			add("trade_acceptances", "trade_id, side, accepted_at, accepted_by", t.ID, a.side, a.at, a.by)
			add("trade_documents", "trade_id, kind, name, uploaded_by, created_at", t.ID, "agreement", fmt.Sprintf("Agreement-%s-%s.pdf", tail, a.side), a.by, a.at)
			accepted = append(accepted, a.at)
			last = maxTime(last, a.at)
		}
	}
	m := breakdown(t.Total, 0.01, t.makerRate())
	invoiceStatus := "unpaid"
	var paidAt *time.Time
	var invRow []any
	finish := func() {
		if status == "completed" && !t.Done.IsZero() {
			for _, r := range g.reviews(t) {
				add("reviews", "trade_id, side, rating, quality, timeliness, communication, text, reviewed_by, created_at", r...)
				last = maxTime(last, r[8].(time.Time))
			}
		}
		var group any
		var share any
		if t.GroupLabel != nil {
			group, share = *t.GroupLabel, t.GroupShare
		}
		t.Status = status
		g.ins("trades", "id, code, title, buyer_party_id, supplier_party_id, quantity, unit, unit_price_idr, total_idr, terms, status, maker_fee_rate, "+
			"platform_fee_rate, market_id, auction_id, source_listing_id, source_quote_id, group_label, group_share, delivery_address, due_at, created_at, updated_at",
			t.ID, t.Code, t.Title, t.Buyer.ID, t.Supplier.ID, qty, t.Unit, t.Price, total, t.Terms, status, t.makerRate(), 0.01, t.MarketID, t.AuctionID,
			t.ListingID, t.QuoteID, group, share, t.Address, due, t.Start, last)
		if invRow != nil {
			g.ins("invoices", "trade_id, number, issued_at, due_at, subtotal_idr, ppn_idr, buyer_pays_idr, platform_fee_idr, maker_fee_idr, "+
				"supplier_receives_idr, created_at, updated_at, status, paid_at", append(invRow, invoiceStatus, paidAt)...)
		}
		for _, k := range kids {
			g.q(k.sql, k.args...)
		}
		for _, j := range journals {
			j()
		}
		for _, f := range facts {
			f()
		}
		g.trades = append(g.trades, t)
	}
	if len(accepted) < 2 {
		finish()
		return
	}
	inv := step(maxTime(accB, accS), 0.5, 8)
	if outcome == "cancel" {
		at := step(maxTime(accB, accS), 0.2, 0.4)
		if g.chance(0.5) {
			at = step(inv, 4, 48)
		}
		if reached(at) && at.After(inv) && reached(inv) {
			invRow = g.invoice(t, tail, inv, &due, m, add)
		}
		if reached(at) {
			event("cancelled", "Batalkan", at, buyerBy)
		}
		finish()
		return
	}
	if !reached(inv) {
		finish()
		return
	}
	invRow = g.invoice(t, tail, inv, &due, m, add)
	event("invoiced", "Terbitkan invoice", inv, supplierBy)
	release := func(at time.Time, amount, subtotal int64, by *string) {
		lines := []demoLine{{"escrow:" + t.Buyer.ID, amount, "payout"}, {"wallet_available:" + t.Supplier.ID, -subtotal, "payout"},
			{"ppn_payable:" + t.Supplier.ID, -(amount - subtotal), "payout"}}
		f := breakdown(subtotal, 0.01, t.makerRate())
		label, makerKey := "Fee platform "+t.Code, "platform_revenue:"
		if f.MakerFee > 0 {
			label = "Fee platform + market maker " + t.Code
			if t.Maker != nil {
				makerKey = "maker_commission:" + t.Maker.ID
			}
		}
		journals = append(journals, func() {
			g.journal("Pencairan "+t.Code+" · "+t.Title, at, &t.ID, nil, by, lines...)
			g.journal(label, at, &t.ID, nil, by, demoLine{"wallet_available:" + t.Supplier.ID, f.PlatformFee + f.MakerFee, "fee"},
				demoLine{"platform_revenue:", -f.PlatformFee, "fee"}, demoLine{makerKey, -f.MakerFee, "fee"})
		})
		invoiceStatus = "released"
	}
	pay := func(at time.Time, kind string) {
		b := m.BuyerPays
		journals = append(journals, func() {
			g.journal("Bayar "+t.Code+" · "+t.Title, at, &t.ID, nil, nil, demoLine{"bank_clearing:", b, kind}, demoLine{"escrow:" + t.Buyer.ID, -b, kind})
		})
		paidAt = &at
	}
	ship := step(inv, 4, 30)
	if escrow {
		paid := step(inv, 3, 60)
		if !reached(paid) {
			finish()
			return
		}
		pay(paid, "escrow")
		invoiceStatus = "escrow"
		event("paid", "Dana masuk escrow", paid, nil)
		ship = step(paid, 4, 30)
	}
	if !reached(ship) {
		finish()
		return
	}
	transit := func() time.Duration {
		if t.Category == "it" || t.Category == "logistics" || t.Unit == "jam" {
			return hrs(g.f(24, 96))
		}
		km := distanceKm(t.Supplier.Location, t.Address)
		return time.Duration(math.Min(7, (0.4+km/380)*g.f(0.85, 1.3)) * float64(24*time.Hour))
	}
	parts := []float64{qty}
	if t.Total > 150_000_000 && qty >= 4 {
		n := g.i(2, 3)
		parts = parts[:0]
		left := qty
		for k := range n {
			pq := left
			if k < n-1 {
				pq = math.Round(qty / float64(n))
			}
			parts = append(parts, pq)
			left -= pq
		}
	}
	carrier := demoPick(g, demoCarriers)
	event("fulfilling", fmt.Sprintf("Kirim %s %s ke %s", qtyLabel(parts[0]), t.Unit, t.Address), ship, supplierBy)
	var deliv time.Time
	delivered := 0
	at := ship
	for k, pq := range parts {
		if k > 0 {
			at = step(at, 24, 96)
			if !reached(at) {
				break
			}
		}
		d := g.workHours(at.Add(transit()))
		id := newID()
		if !reached(d) {
			add("shipments", "id, trade_id, quantity, drop_point, carrier, scheduled_at, status, created_at, updated_at", id, t.ID, pq, t.Address, carrier, at, "in_transit", at, at)
			break
		}
		proof := newID()
		add("trade_documents", "id, trade_id, kind, name, uploaded_by, created_at", proof, t.ID, "proof", fmt.Sprintf("surat-jalan-%s-%d.jpg", tail, k+1), supplierBy, d)
		add("shipments", "id, trade_id, quantity, drop_point, carrier, scheduled_at, status, delivered_at, proof_document_id, created_at, updated_at",
			id, t.ID, pq, t.Address, carrier, at, "delivered", d, proof, at, d)
		deliv = d
		delivered++
		last = maxTime(last, d)
	}
	if delivered < len(parts) {
		finish()
		return
	}
	event("delivered", fmt.Sprintf("Terkirim %s %s ke %s", qtyLabel(parts[len(parts)-1]), t.Unit, t.Address), deliv, supplierBy)
	qc := step(deliv, 1, 8)

	if outcome == "dispute" {
		open := step(deliv, 1, 20)
		if !reached(open) {
			finish()
			return
		}
		reason := g.disputeReason(t.Category)
		rejected := g.chance(0.4)
		if rejected {
			add("qc_results", "trade_id, outcome, accepted_quantity, note, at, checked_by", t.ID, "rejected", 0, reason, open, buyerBy)
			event("disputed", "Barang ditolak saat QC, dispute dibuka", open, buyerBy)
			reason = "QC menolak barang: " + reason
		} else {
			event("disputed", "Ajukan dispute", open, buyerBy)
		}
		did, dcode := newID(), g.code("DSP")
		resolve := open.Add(hrs(g.f(30, 140)))
		dst := []string{"open", "evidence", "review"}[g.r.IntN(3)]
		var kind, refund, releaseAmt, reasonR, resolvedAt, resolvedBy any
		var devents [][]any
		if reached(resolve) && g.chance(0.6) {
			dst = "resolved"
			k := []string{"refund", "release", "partial"}[g.r.IntN(3)]
			ref := map[string]int64{"refund": total, "release": 0, "partial": nicePrice(float64(total) * g.f(0.2, 0.5))}[k]
			kind, refund, releaseAmt, reasonR, resolvedAt, resolvedBy = k, ref, total-ref, "Bukti foto dan surat jalan sudah diperiksa.", resolve, g.staff.ID
			held := m.BuyerPays
			back := min(held, breakdown(ref, 0, 0).BuyerPays)
			if k == "release" {
				back = 0
			}
			by := &g.staff.ID
			if back > 0 {
				journals = append(journals, func() {
					g.journal("Refund "+t.Code, resolve, &t.ID, nil, by, demoLine{"escrow:" + t.Buyer.ID, back, "refund"}, demoLine{"wallet_available:" + t.Buyer.ID, -back, "refund"})
				})
			}
			newStatus, pay := "completed", "released"
			note := fmt.Sprintf("Dana %s dilepas ke supplier", rupiah(total))
			switch k {
			case "refund":
				newStatus, pay, note = "cancelled", "refunded", fmt.Sprintf("Refund penuh %s ke pembeli", rupiah(total))
			case "partial":
				note = fmt.Sprintf("%s dikembalikan ke pembeli, %s dilepas ke supplier", rupiah(ref), rupiah(total-ref))
			}
			if held-back > 0 {
				release(resolve, held-back, total-ref, by)
			}
			invoiceStatus = pay
			event(newStatus, "Putusan dispute: "+note, resolve, by)
			if newStatus == "completed" {
				t.Done = resolve
			}
			devents = [][]any{{did, open.Add(hrs(20)), g.staff.ID, g.staff.Name + " (Admin)", "Review dimulai"},
				{did, resolve, g.staff.ID, g.staff.Name + " (Admin)", "Diputuskan: " + note}}
		}
		add("disputes", "id, code, trade_id, status, reason, opened_by_side, opened_by, opened_at, market_id, resolution_kind, refund_idr, release_idr, "+
			"resolution_reason, resolved_at, resolved_by, created_at, updated_at", did, dcode, t.ID, dst, reason, "buyer", buyerBy, open, t.MarketID,
			kind, refund, releaseAmt, reasonR, resolvedAt, resolvedBy, open, open)
		add("dispute_evidence", "dispute_id, side, author_user_id, author_name, text, file_name, created_at", did, "buyer", buyerBy, t.Buyer.Actor.Name,
			reason, "foto-barang-"+tail+".jpg", open)
		for _, e := range devents {
			add("dispute_events", "dispute_id, at, actor_user_id, actor_label, label", e...)
		}
		if dst != "open" {
			add("dispute_evidence", "dispute_id, side, author_user_id, author_name, text, created_at", did, "supplier", supplierBy, t.Supplier.Actor.Name,
				demoPick(g, demoSupplierDefense), step(open, 3, 20))
		}
		finish()
		return
	}

	if !reached(qc) {
		finish()
		return
	}
	note := "Barang diterima"
	if outcome == "partial" {
		acc := math.Round(qty * g.f(0.85, 0.95))
		if acc > 0 && acc < qty {
			refundAmt := partialRefund(t.Price, qty, acc)
			qty, total = acc, round(acc*float64(t.Price))
			note = fmt.Sprintf("Diterima sebagian (%s %s), refund %s", qtyLabel(acc), t.Unit, rupiah(refundAmt))
			add("qc_results", "trade_id, outcome, accepted_quantity, note, at, checked_by", t.ID, "partial", acc, "Sebagian tidak lolos QC", qc, buyerBy)
			journals = append(journals, func() {
				g.journal("Refund "+t.Code, qc, &t.ID, nil, buyerBy, demoLine{"escrow:" + t.Buyer.ID, refundAmt, "refund"}, demoLine{"wallet_available:" + t.Buyer.ID, -refundAmt, "refund"})
			})
			release(qc, m.BuyerPays-refundAmt, total, buyerBy)
		} else {
			outcome = ""
		}
	}
	if outcome != "partial" {
		add("qc_results", "trade_id, outcome, accepted_quantity, at, checked_by", t.ID, "accepted", qty, qc, buyerBy)
		if escrow {
			release(qc, m.BuyerPays, total, buyerBy)
		}
	}
	if escrow {
		event("completed", note, qc, buyerBy)
		t.Done = qc
	} else {
		due = qc.Add(time.Duration(termsDays[t.Terms]) * day)
		event("accepted", note, qc, buyerBy)
		paid := g.workHours(maxTime(qc.Add(24*time.Hour), due.Add(hrs(g.f(-72, 30)*slow))))
		if reached(paid) {
			pay(paid, "payment")
			release(paid, m.BuyerPays, total, nil)
			event("completed", "Pembayaran diterima supplier", paid, nil)
			t.Done = paid
		}
	}
	finish()
}

func (g *demoGen) invoice(t *demoTrade, tail string, at time.Time, due *time.Time, m money, add func(string, string, ...any)) []any {
	*due = at.Add(3 * day)
	if t.Terms != "escrow" {
		*due = at.Add(time.Duration(termsDays[t.Terms]+5) * day)
	}
	add("trade_documents", "trade_id, kind, name, uploaded_by, created_at", t.ID, "invoice", "INV-"+tail+".pdf", t.Supplier.actorID(), at)
	return []any{t.ID, "INV-" + tail, at, *due, m.Subtotal, m.VAT, m.BuyerPays, m.PlatformFee, m.MakerFee, m.SupplierReceives, at, at}
}

func (g *demoGen) reviews(t *demoTrade) [][]any {
	var out [][]any
	for _, side := range []string{"buyer", "supplier"} {
		other, me := t.Supplier, t.Buyer
		if side == "supplier" {
			other, me = t.Buyer, t.Supplier
		}
		at := g.workHours(t.Done.Add(hrs(g.f(4, 110))))
		if at.After(g.now) || !g.chance(map[string]float64{"buyer": 0.85, "supplier": 0.7}[side]) {
			continue
		}
		r := int(math.Max(2, math.Min(5, math.Round(1.6+3.6*other.Q+g.f(-0.7, 0.5)))))
		sub := func() float64 { return math.Max(1, math.Min(5, math.Round((float64(r)+g.f(-0.8, 0.6))*2)/2)) }
		item := strings.Split(t.Title, " · ")[0]
		if t.Item != "" {
			item = t.Item
		}
		out = append(out, []any{t.ID, side, r, sub(), sub(), sub(), g.reviewText(t.Category, item, r, side == "buyer"), me.actorID(), at})
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
