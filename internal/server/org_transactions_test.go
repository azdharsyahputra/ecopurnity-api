package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestOrgTransactions(t *testing.T) {
	e := newEnv(t)
	owner, _, buyerOrg := e.workspace("Pembeli Org")
	finance, financeID := e.member(buyerOrg, "finance")
	procurement, _ := e.member(buyerOrg, "procurement")
	ops, _ := e.member(buyerOrg, "operations")
	_, supOwnerID, supplierOrg := e.workspace("Pemasok Org")
	sales, _ := e.member(supplierOrg, "sales")
	supOps, _ := e.member(supplierOrg, "operations")
	supFinance, _ := e.member(supplierOrg, "finance")
	party := func(org string) string {
		id, err := orgParty(t0(), e.db.Primary(), org)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := e.newTestTrade(newTrade{Title: "Karton 1.000 pcs", BuyerParty: party(buyerOrg), SupplierParty: party(supplierOrg), Quantity: 1000, Unit: "pcs", UnitPriceIdr: 2_000})
	base := func(org string) string { return "/orgs/" + org + "/transactions/" + id }
	do := func(org string, c *http.Client, action string, extra map[string]any) resp {
		t.Helper()
		body := map[string]any{"action": action}
		for k, v := range extra {
			body[k] = v
		}
		return e.call(c, "POST", base(org)+"/actions", body)
	}
	ok := func(r resp, status string) {
		t.Helper()
		if r.Status != 200 || r.Body["status"] != status {
			t.Fatalf("want %s: %d %v", status, r.Status, r.Body)
		}
	}

	// Reads: members only, from the org's side.
	if l := e.list(finance, "/orgs/"+buyerOrg+"/transactions"); len(l) != 1 || l[0]["role"] != "buyer" || l[0]["timeline"] != nil {
		t.Fatalf("list: %v", l)
	}
	if r := e.call(sales, "GET", base(buyerOrg), nil); r.Status != 403 {
		t.Fatal("other org", r.Status)
	}
	if r := e.call(sales, "GET", base(supplierOrg), nil); r.Status != 200 || r.Body["role"] != "supplier" || r.Body["activity"] == nil {
		t.Fatal("supplier org view", r.Status, r.Body)
	}

	// Role gate before the state machine: finance may not accept an agreement (and the message says who may).
	r := do(buyerOrg, finance, "accept_agreement", nil)
	if r.Status != 403 || r.message() != "Setujui agreement hanya untuk Owner, Procurement, Sales; peranmu Finance" {
		t.Fatal("finance accept", r.Status, r.Body)
	}
	if r := do(buyerOrg, finance, "issue_invoice", nil); r.Status != 409 {
		t.Fatal("finance may invoice in general, but not as the buyer: 409", r.Status, r.Body)
	}
	ok(do(buyerOrg, procurement, "accept_agreement", nil), "agreement")
	if r := do(supplierOrg, supOps, "accept_agreement", nil); r.Status != 403 {
		t.Fatal("supplier operations accept", r.Status)
	}
	ok(do(supplierOrg, sales, "accept_agreement", nil), "agreement")
	ok(do(supplierOrg, supFinance, "issue_invoice", nil), "invoiced")
	if r := do(buyerOrg, procurement, "pay", nil); r.Status != 403 {
		t.Fatal("procurement pays", r.Status)
	}
	r = do(buyerOrg, finance, "pay", nil)
	ok(r, "paid")
	if acts := r.Body["activity"].([]any); len(acts) != 2 || !strings.HasSuffix(acts[0].(map[string]any)["actor"].(string), "(Finance)") {
		t.Fatalf("org activity on the trade: %v", acts)
	}
	ok(do(supplierOrg, supOps, "ship", map[string]any{"shipment": map[string]any{"quantity": 1000, "dropPoint": "Gudang Bandung"}}), "fulfilling")
	ok(do(supplierOrg, supOps, "upload_proof", map[string]any{"file": "sj.jpg"}), "delivered")
	if r := do(buyerOrg, finance, "confirm_receipt", nil); r.Status != 403 {
		t.Fatal("finance confirms receipt", r.Status)
	}
	ok(do(buyerOrg, ops, "confirm_receipt", nil), "completed")
	ok(do(buyerOrg, owner, "review", map[string]any{"review": map[string]any{"rating": 5, "quality": 5, "timeliness": 5, "communication": 5, "text": "Mantap"}}), "completed")

	// Fan-out: members who may see transactions get notified with the org link and trade.updated frames.
	if !slices.ContainsFunc(e.notifications(supOwnerID), func(s string) bool { return strings.HasSuffix(s, ": Dana masuk escrow") }) {
		t.Fatal("supplier org owner notified of the payment", e.notifications(supOwnerID))
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND href = $2`, financeID, "/org/"+buyerOrg+"/transactions/"+id).(int64); n == 0 {
		t.Fatal("org link in notifications")
	}
	frames := 0
	for _, f := range e.frames("user:" + financeID) {
		if f["type"] == "trade.updated" {
			frames++
		}
	}
	if frames < 8 {
		t.Fatal("trade.updated frames for org members", frames)
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'activity' AND payload->>'type' = 'transaction_completed' AND payload->>'title' = 'Transaksi selesai: Karton 1.000 pcs'`).(int64); n != 1 {
		t.Fatal("transaction_completed activity", n)
	}
}

// activity returns the public activity facts (outbox topic `activity`) with this title.
func (e *testEnv) activity(typ, title string) []map[string]any {
	e.t.Helper()
	rows, err := e.db.Primary().Query(t0(), `SELECT payload FROM outbox WHERE topic = 'activity' AND payload->>'type' = $1 AND payload->>'title' = $2`, typ, title)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var m map[string]any
		if err := rows.Scan(&m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestAuctionActivityEvents(t *testing.T) {
	e := newEnv(t)
	market := e.seedMarket("Aktivitas", "agri", "kg", "active", "auto")
	ins := func(title, status, visibility string, round any) string {
		return e.scalar(`
			INSERT INTO auctions (title, market_id, round_no, category_id, type, status, visibility, lot_item, quantity, unit, opening_price_idr, min_step_idr, starts_at, ends_at)
			VALUES ($1, $2, $3, 'agri', 'forward', $4, $5, 'Kopi', 10, 'kg', 1000, 10, now() - interval '1 minute', now() + interval '1 hour') RETURNING id::text`,
			title, market, round, status, visibility).(string)
	}
	full := ins("Aktivitas terbuka", "scheduled", "full", nil)
	sealed := ins("Aktivitas tertutup", "live", "sealed", nil)
	round := ins("Aktivitas round", "scheduled", "full", 1)
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if a := e.activity("auction_started", "Auction dimulai: Aktivitas terbuka"); len(a) != 1 || a[0]["amountIdr"] != float64(10_000) {
		t.Fatal("auction_started", a)
	}
	if a := e.activity("auction_started", "Auction dimulai: Aktivitas round"); len(a) != 0 {
		t.Fatal("market rounds are announced by the maker, not again by the clock", a)
	}
	c, _ := e.bidder("Penawar Aktif")
	for _, id := range []string{full, sealed} {
		e.qualify(c, id)
		if r := e.call(c, "POST", "/auctions/"+id+"/bids", map[string]any{"priceIdr": 1100}); r.Status != 200 {
			t.Fatal(r.Status, r.Body)
		}
	}
	if a := e.activity("bid_placed", "Bid baru di auction Aktivitas terbuka"); len(a) != 1 || a[0]["amountIdr"] != float64(11_000) {
		t.Fatal("bid_placed full", a)
	}
	if a := e.activity("bid_placed", "Bid baru di auction Aktivitas tertutup"); len(a) != 1 || a[0]["amountIdr"] != nil {
		t.Fatal("bid_placed sealed hides the amount", a)
	}
	e.exec(`UPDATE auctions SET ends_at = now() - interval '1 second' WHERE id IN ($1, $2, $3)`, full, sealed, round)
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if a := e.activity("auction_closed", "Auction ditutup: Aktivitas tertutup"); len(a) != 1 {
		t.Fatal("auction_closed", a)
	}
}
