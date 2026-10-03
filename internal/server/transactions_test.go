package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

// partyOf returns (creating) the user's party.
func (e *testEnv) partyOf(userID string) string {
	e.t.Helper()
	id, err := userParty(t0(), e.db.Primary(), userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *testEnv) externalParty(name string) string {
	e.t.Helper()
	return e.scalar(`INSERT INTO parties (kind, name) VALUES ('external', $1) RETURNING id::text`, name).(string)
}

// newTestTrade creates a trade directly through createTrade.
func (e *testEnv) newTestTrade(nt newTrade) string {
	e.t.Helper()
	if nt.Unit == "" {
		nt.Unit = "kg"
	}
	nt.DeliveryAddress, nt.Via = "Gudang Bandung", "auction"
	var id string
	if err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		t, err := createTrade(t0(), tx, nt)
		id = t.ID
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *testEnv) act(c *http.Client, id string, body map[string]any) resp {
	e.t.Helper()
	return e.call(c, "POST", "/me/transactions/"+id+"/actions", body)
}

func (e *testEnv) mustAct(c *http.Client, id string, body map[string]any, status string) resp {
	e.t.Helper()
	r := e.act(c, id, body)
	if r.Status != 200 || r.Body["status"] != status {
		e.t.Fatalf("%v: %d %v", body["action"], r.Status, r.Body)
	}
	return r
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func TestTradeEscrowFlowBetweenUsers(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Rina Pembeli")
	supplier, supplierID := e.bidder("Ajar Supplier")
	market := e.seedMarket("Kopi Escrow", "agri", "kg", "active", "auto")
	id := e.newTestTrade(newTrade{Title: "Green bean 100 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID),
		Quantity: 100, UnitPriceIdr: 10_000, MakerFeeRate: 0.005, MarketID: &market})

	// Each side sees the one row from its own side.
	if l := e.callList(supplier, "/me/transactions?role=supplier&status=agreement"); !slices.ContainsFunc(l, func(x map[string]any) bool { return x["id"] == id }) {
		t.Fatal("supplier list", l)
	}
	got := e.call(supplier, "GET", "/me/transactions/"+id, nil)
	if got.Status != 200 || got.Body["role"] != "supplier" || got.Body["counterparty"].(map[string]any)["name"] != "Rina Pembeli" ||
		got.Body["peer"].(map[string]any)["txId"] != id || got.Body["peer"].(map[string]any)["userId"] != buyerID {
		t.Fatalf("supplier view: %v", got.Body)
	}
	if docs := got.Body["documents"].([]any); len(docs) != 1 || docs[0].(map[string]any)["kind"] != "order" {
		t.Fatalf("order document: %v", docs)
	}
	if r := e.call(e.client(), "GET", "/me/transactions/"+id, nil); r.Status != 401 {
		t.Fatal("anonymous", r.Status)
	}
	outsider, _ := e.bidder("Orang Lain")
	if r := e.call(outsider, "GET", "/me/transactions/"+id, nil); r.Status != 404 {
		t.Fatal("outsider", r.Status)
	}
	if r := e.act(outsider, id, map[string]any{"action": "accept_agreement"}); r.Status != 404 {
		t.Fatal("outsider action", r.Status)
	}

	// Agreement by both before the invoice; the buyer cannot invoice.
	if r := e.act(supplier, id, map[string]any{"action": "issue_invoice"}); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatal("invoice before agreement", r.Status, r.Body)
	}
	e.mustAct(buyer, id, map[string]any{"action": "accept_agreement"}, "agreement")
	if r := e.act(buyer, id, map[string]any{"action": "accept_agreement"}); r.Status != 409 {
		t.Fatal("accept twice", r.Status)
	}
	e.mustAct(supplier, id, map[string]any{"action": "accept_agreement"}, "agreement")
	if r := e.act(buyer, id, map[string]any{"action": "issue_invoice"}); r.Status != 409 {
		t.Fatal("buyer invoice", r.Status)
	}
	r := e.mustAct(supplier, id, map[string]any{"action": "issue_invoice"}, "invoiced")
	if inv := r.Body["invoice"].(map[string]any); !strings.HasPrefix(inv["number"].(string), "INV-") {
		t.Fatal(inv)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE '%Terbitkan invoice'`, buyerID).(int64); n != 1 {
		t.Fatal("buyer not notified of the invoice", n)
	}

	// Pay into escrow: 1.000.000 + PPN 110.000.
	r = e.mustAct(buyer, id, map[string]any{"action": "pay"}, "paid")
	if r.Body["payment"].(map[string]any)["status"] != "escrow" {
		t.Fatal(r.Body["payment"])
	}
	fb := e.call(buyer, "GET", "/me/finance", nil)
	if num(fb.Body["escrowHeldIdr"]) != 1_110_000 || num(fb.Body["availableIdr"]) != 0 {
		t.Fatalf("buyer finance: %v", fb.Body)
	}
	if es := fb.Body["entries"].([]any); len(es) != 1 || num(es[0].(map[string]any)["amountIdr"]) != -1_110_000 || es[0].(map[string]any)["kind"] != "escrow" {
		t.Fatalf("buyer entries: %v", es)
	}
	fs := e.call(supplier, "GET", "/me/finance", nil)
	if num(fs.Body["receivableIdr"]) != 1_095_000 || num(fs.Body["availableIdr"]) != 0 {
		t.Fatalf("supplier finance: %v", fs.Body)
	}

	// Staged shipments.
	ship := func(q float64) map[string]any {
		return map[string]any{"action": "ship", "shipment": map[string]any{"quantity": q, "dropPoint": "Gudang Bandung", "carrier": "", "scheduledAt": time.Now().UTC().Format(time.RFC3339)}}
	}
	r = e.mustAct(supplier, id, ship(60), "fulfilling")
	if r := e.act(supplier, id, ship(50)); r.Status != 422 || r.field("quantity") != "Maksimal 40 kg" {
		t.Fatal("over-ship", r.Status, r.Body)
	}
	e.mustAct(supplier, id, ship(40), "fulfilling")
	if r := e.act(supplier, id, map[string]any{"action": "upload_proof"}); r.Status != 422 || r.field("file") == "" {
		t.Fatal("proof without file", r.Status, r.Body)
	}
	e.mustAct(supplier, id, map[string]any{"action": "upload_proof", "file": "sj-1.jpg"}, "fulfilling")
	r = e.mustAct(supplier, id, map[string]any{"action": "upload_proof", "file": "sj-2.jpg"}, "delivered")
	sh := r.Body["shipments"].([]any)
	if len(sh) != 2 || sh[0].(map[string]any)["carrier"] != "Armada supplier" || sh[1].(map[string]any)["proof"] != "sj-2.jpg" {
		t.Fatalf("shipments: %v", sh)
	}

	// Partial QC: 90 accepted, the short 10 kg refunded with its PPN, the rest released minus fees.
	if r := e.act(buyer, id, map[string]any{"action": "confirm_receipt", "qc": map[string]any{"outcome": "partial", "acceptedQty": 100, "note": "x"}}); r.Status != 422 || r.field("acceptedQty") == "" {
		t.Fatal("partial out of range", r.Status, r.Body)
	}
	if r := e.act(buyer, id, map[string]any{"action": "confirm_receipt", "qc": map[string]any{"outcome": "partial", "acceptedQty": 90}}); r.Status != 422 || r.field("note") == "" {
		t.Fatal("partial without note", r.Status, r.Body)
	}
	r = e.mustAct(buyer, id, map[string]any{"action": "confirm_receipt", "qc": map[string]any{"outcome": "partial", "acceptedQty": 90, "note": "10 kg basah"}}, "completed")
	if num(r.Body["totalIdr"]) != 900_000 || r.Body["quantity"].(map[string]any)["value"] != float64(90) || r.Body["payment"].(map[string]any)["status"] != "released" {
		t.Fatalf("after partial QC: %v", r.Body)
	}
	fb = e.call(buyer, "GET", "/me/finance", nil)
	if num(fb.Body["escrowHeldIdr"]) != 0 || num(fb.Body["availableIdr"]) != 111_000 {
		t.Fatalf("buyer after QC: %v", fb.Body)
	}
	fs = e.call(supplier, "GET", "/me/finance", nil)
	// Released 999.000 (1.110.000 - refund 111.000) minus fees on 900.000: platform 9.000 + maker 4.500.
	if num(fs.Body["availableIdr"]) != 985_500 || num(fs.Body["receivableIdr"]) != 0 {
		t.Fatalf("supplier after QC: %v", fs.Body)
	}
	kinds := map[string]int64{}
	for _, x := range fs.Body["entries"].([]any) {
		m := x.(map[string]any)
		kinds[m["kind"].(string)] += num(m["amountIdr"])
	}
	if kinds["payout"] != 999_000 || kinds["fee"] != -13_500 {
		t.Fatalf("supplier entries: %v", kinds)
	}
	if v := e.scalar(`SELECT coalesce(sum(amount), 0)::bigint FROM ledger_entries WHERE trade_id = $1`, id).(int64); v != 0 {
		t.Fatal("trade journals must balance", v)
	}
	if v := e.scalar(`SELECT -sum(e.amount)::bigint FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.trade_id = $1 AND a.kind = 'maker_commission'`, id).(int64); v != 4_500 {
		t.Fatal("maker commission", v)
	}
	if v := e.scalar(`SELECT -sum(e.amount)::bigint FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.trade_id = $1 AND a.kind = 'ppn_payable'`, id).(int64); v != 99_000 {
		t.Fatal("supplier PPN", v)
	}

	// Reviews, once per side.
	review := map[string]any{"action": "review", "review": map[string]any{"rating": 4, "quality": 4, "timeliness": 5, "communication": 4.5, "text": "Oke"}}
	e.mustAct(buyer, id, review, "completed")
	if r := e.act(buyer, id, review); r.Status != 409 {
		t.Fatal("second review", r.Status)
	}
	r = e.mustAct(supplier, id, review, "completed")
	if rv := r.Body["reviews"].(map[string]any); rv["buyer"].(map[string]any)["by"] != "Rina Pembeli" || rv["supplier"] == nil {
		t.Fatalf("reviews: %v", rv)
	}
	tl := r.Body["timeline"].([]any)
	if len(tl) != 6 || tl[5].(map[string]any)["status"] != "completed" || tl[5].(map[string]any)["at"] == nil {
		t.Fatalf("timeline: %v", tl)
	}

	// Realtime and analytics side effects.
	for _, u := range []string{buyerID, supplierID} {
		n := 0
		for _, f := range e.frames("user:" + u) {
			if f["type"] == "trade.updated" && f["payload"].(map[string]any)["transactionId"] == id {
				n++
			}
		}
		if n < 10 {
			t.Fatalf("trade.updated frames for %s: %d", u, n)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'trade.status' AND aggregate_id = $1`, id).(int64); n != 6 {
		t.Fatal("trade.status facts", n)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_type = 'transaction' AND entity_id = $1`, id).(int64); n < 10 {
		t.Fatal("audit", n)
	}

	// List shape: summary without detail fields, with F6 fields.
	lr := e.callList(buyer, "/me/transactions?status=completed,paid&role=buyer")
	var mine map[string]any
	for _, x := range lr {
		if x["id"] == id {
			mine = x
		}
	}
	if mine == nil || mine["timeline"] != nil || mine["invoice"] == nil || mine["qc"] == nil || mine["role"] != "buyer" {
		t.Fatalf("list item: %v", mine)
	}
	if lr := e.callList(buyer, "/me/transactions?role=supplier"); slices.ContainsFunc(lr, func(x map[string]any) bool { return x["id"] == id }) {
		t.Fatal("role filter")
	}

	// Withdrawals: bank first, never more than available.
	wd := func(amount int) resp {
		return e.call(supplier, "POST", "/me/finance/withdrawals", map[string]any{"amountIdr": amount})
	}
	if r := wd(1000); r.Status != 422 || r.field("amountIdr") != "Tambahkan rekening pencairan dulu" {
		t.Fatal("no bank", r.Status, r.Body)
	}
	if r := e.call(supplier, "PUT", "/me/finance/bank", map[string]any{"bank": " ", "accountNo": "1234567890", "holder": "Ajar"}); r.Status != 422 || r.field("bank") == "" {
		t.Fatal("bank validation", r.Status, r.Body)
	}
	r = e.call(supplier, "PUT", "/me/finance/bank", map[string]any{"bank": "BCA", "accountNo": "1234567890", "holder": "Ajar"})
	if r.Status != 200 || r.Body["bank"].(map[string]any)["accountNo"] != "••••7890" {
		t.Fatal("save bank", r.Status, r.Body)
	}
	party := e.partyOf(supplierID)
	sealed := e.scalar(`SELECT account_no_enc FROM bank_accounts WHERE party_id = $1 AND replaced_at IS NULL`, party).([]byte)
	if plain, err := secure.Decrypt(e.server.Keys.BankCipher, sealed, []byte(party)); err != nil || string(plain) != "1234567890" || strings.Contains(string(sealed), "1234567890") {
		t.Fatal("account number must be encrypted to the party", err)
	}
	if r := wd(985_501); r.Status != 422 || r.field("amountIdr") != "Maksimal Rp 985.500" {
		t.Fatal("over balance", r.Status, r.Body)
	}
	r = wd(900_000)
	if r.Status != 201 || num(r.Body["availableIdr"]) != 85_500 || num(r.Body["withdrawnIdr"]) != 900_000 ||
		r.Body["withdrawals"].([]any)[0].(map[string]any)["status"] != "processing" {
		t.Fatal("withdraw", r.Status, r.Body)
	}
	if r := wd(85_500); r.Status != 201 || num(r.Body["availableIdr"]) != 0 {
		t.Fatal("withdraw the rest (incl. PPN)", r.Status, r.Body)
	}
	if v := e.scalar(`SELECT coalesce(sum(e.amount), 0)::bigint FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE a.owner_party_id = $1 AND a.kind IN ('wallet_available','ppn_payable')`, party).(int64); v != 0 {
		t.Fatal("wallet + PPN drained", v)
	}
}

func (e *testEnv) callList(c *http.Client, path string) []map[string]any {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+BasePath+path, nil)
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		e.t.Fatalf("%s: %d %v", path, res.StatusCode, err)
	}
	return out
}

func TestTradeNetTermsWithSimulatedSupplier(t *testing.T) {
	e := newEnv(t)
	e.server.SimulateCounterparties = true
	buyer, buyerID := e.bidder("Budi Net")
	ext := e.externalParty("CV Kemasan Jaya")
	id := e.newTestTrade(newTrade{Title: "Box karton 300 pcs", BuyerParty: e.partyOf(buyerID), SupplierParty: ext,
		Quantity: 300, Unit: "pcs", UnitPriceIdr: 2_000, Terms: "net14"})
	tick := func() {
		t.Helper()
		if err := e.server.CounterpartyTick(t0(), 0); err != nil {
			t.Fatal(err)
		}
	}
	status := func() string { return e.scalar(`SELECT status FROM trades WHERE id = $1`, id).(string) }

	tick() // supplier bot accepts the agreement
	if n := e.scalar(`SELECT count(*) FROM trade_acceptances WHERE trade_id = $1 AND side = 'supplier' AND accepted_by IS NULL`, id).(int64); n != 1 {
		t.Fatal("bot acceptance", n)
	}
	tick() // nothing to do until the buyer accepts
	e.mustAct(buyer, id, map[string]any{"action": "accept_agreement"}, "agreement")
	tick()
	if status() != "invoiced" {
		t.Fatal("bot invoice", status())
	}
	if r := e.act(buyer, id, map[string]any{"action": "pay"}); r.Status != 409 {
		t.Fatal("net terms: no payment before receipt", r.Status)
	}
	tick() // ship everything
	tick() // proof
	if status() != "delivered" {
		t.Fatal("bot delivery", status())
	}
	r := e.mustAct(buyer, id, map[string]any{"action": "confirm_receipt"}, "accepted")
	if due, _ := time.Parse(time.RFC3339, r.Body["dueAt"].(string)); due.Before(time.Now().Add(13 * day)) {
		t.Fatal("payment due date = +14 days", due)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'delivery'`, buyerID).(int64); n != 2 {
		t.Fatal("delivery notifications", n)
	}
	r = e.mustAct(buyer, id, map[string]any{"action": "pay"}, "completed")
	if r.Body["payment"].(map[string]any)["status"] != "released" {
		t.Fatal(r.Body["payment"])
	}
	f := e.call(buyer, "GET", "/me/finance", nil)
	es := f.Body["entries"].([]any)
	if num(f.Body["escrowHeldIdr"]) != 0 || len(es) != 1 || num(es[0].(map[string]any)["amountIdr"]) != -666_000 || es[0].(map[string]any)["kind"] != "payment" {
		t.Fatalf("buyer finance: %v", f.Body)
	}
	tick() // supplier reviews
	r = e.call(buyer, "GET", "/me/transactions/"+id, nil)
	if rv := r.Body["reviews"].(map[string]any); rv["supplier"].(map[string]any)["by"] != "CV Kemasan Jaya" {
		t.Fatalf("bot review: %v", rv)
	}
	if r.Body["peer"] != nil {
		t.Fatal("external counterparty has no peer")
	}
}

func TestTradeDisputes(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Dewi Dispute")
	supplier, supplierID := e.bidder("Eko Dispute")
	mk := func() string {
		id := e.newTestTrade(newTrade{Title: "Gula 10 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID), Quantity: 10, UnitPriceIdr: 15_000})
		e.mustAct(buyer, id, map[string]any{"action": "accept_agreement"}, "agreement")
		e.mustAct(supplier, id, map[string]any{"action": "accept_agreement"}, "agreement")
		e.mustAct(supplier, id, map[string]any{"action": "issue_invoice"}, "invoiced")
		e.mustAct(buyer, id, map[string]any{"action": "pay"}, "paid")
		return id
	}

	// Dispute while paid; evidence from the other side moves it to `evidence`; escrow stays held.
	id := mk()
	if r := e.act(supplier, id, map[string]any{"action": "dispute", "note": "  "}); r.Status != 422 || r.field("note") == "" {
		t.Fatal("dispute without reason", r.Status, r.Body)
	}
	r := e.mustAct(supplier, id, map[string]any{"action": "dispute", "note": "Pembeli minta ganti spek", "file": "chat.png"}, "disputed")
	if d := r.Body["dispute"].(map[string]any); d["status"] != "open" || d["evidence"].([]any)[0].(map[string]any)["file"] != "chat.png" {
		t.Fatalf("dispute: %v", d)
	}
	r = e.mustAct(buyer, id, map[string]any{"action": "add_evidence", "note": "Spek sesuai PO"}, "disputed")
	if d := r.Body["dispute"].(map[string]any); d["status"] != "evidence" || len(d["evidence"].([]any)) != 2 {
		t.Fatalf("evidence: %v", d)
	}
	if e.scalar(`SELECT count(*) FROM dispute_events d JOIN disputes x ON x.id = d.dispute_id WHERE x.trade_id = $1`, id).(int64) != 2 {
		t.Fatal("dispute events")
	}
	if f := e.call(buyer, "GET", "/me/finance", nil); num(f.Body["escrowHeldIdr"]) != 166_500 {
		t.Fatal("escrow stays held", f.Body)
	}
	if r := e.act(buyer, id, map[string]any{"action": "cancel"}); r.Status != 409 {
		t.Fatal("cancel after payment", r.Status)
	}

	// QC rejection opens a dispute with the note as evidence.
	id = mk()
	e.mustAct(supplier, id, map[string]any{"action": "ship", "shipment": map[string]any{"quantity": 10, "dropPoint": "Toko", "carrier": "JNE", "scheduledAt": time.Now().UTC().Format(time.RFC3339)}}, "fulfilling")
	e.mustAct(supplier, id, map[string]any{"action": "upload_proof", "file": "sj.jpg"}, "delivered")
	r = e.mustAct(buyer, id, map[string]any{"action": "confirm_receipt", "qc": map[string]any{"outcome": "rejected", "note": "Basah semua"}}, "disputed")
	if d := r.Body["dispute"].(map[string]any); d["reason"] != "QC menolak barang: Basah semua" || r.Body["qc"].(map[string]any)["acceptedQty"] != float64(0) {
		t.Fatalf("rejected QC: %v", r.Body)
	}

	// Cancel before payment.
	id = e.newTestTrade(newTrade{Title: "Gula batal", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID), Quantity: 1, UnitPriceIdr: 15_000})
	r = e.mustAct(buyer, id, map[string]any{"action": "cancel"}, "cancelled")
	tl := r.Body["timeline"].([]any)
	if tl[1].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("off-path status follows the last reached step: %v", tl)
	}
	if r := e.call(buyer, "GET", "/me/transactions/not-a-uuid", nil); r.Status != 404 {
		t.Fatal("bad id", r.Status)
	}
}

func TestDirectMarketOrder(t *testing.T) {
	e := newEnv(t)
	seller, _ := e.bidder("Sari Penjual")
	buyer, buyerID := e.bidder("Tono Pembeli")
	market := e.seedMarket("Kopi Langsung", "agri", "kg", "active", "auto")
	e.exec(`UPDATE markets SET mechanism = 'direct_market' WHERE id = $1`, market)
	r := e.call(seller, "POST", "/me/listings", supplyBody("Kopi robusta", 50_000, 100, "kg"))
	if r.Status != 201 {
		t.Fatal(r.Status, r.Body)
	}
	listing := r.Body["id"].(string)
	e.exec(`UPDATE listings SET market_id = $2, status = 'in_market' WHERE id = $1`, listing, market)
	order := func(c *http.Client, m string, qty float64) resp {
		return e.call(c, "POST", "/markets/"+m+"/orders", map[string]any{"listingId": listing, "quantity": qty})
	}
	other := e.seedMarket("Kopi Lelang", "agri", "kg", "active", "auto")
	if r := order(buyer, other, 1); r.Status != 409 || r.code() != "not_direct" {
		t.Fatal("not direct", r.Status, r.Body)
	}
	if r := order(buyer, "nope", 1); r.Status != 409 {
		t.Fatal("unknown market", r.Status)
	}
	if r := order(seller, market, 1); r.Status != 409 || r.code() != "own_listing" {
		t.Fatal("own listing", r.Status, r.Body)
	}
	if r := order(buyer, market, 101); r.Status != 422 || r.field("quantity") != "1–100 kg" {
		t.Fatal("quantity", r.Status, r.Body)
	}
	if r := e.call(buyer, "POST", "/markets/"+market+"/orders", map[string]any{"listingId": "x", "quantity": 1}); r.Status != 404 {
		t.Fatal("unknown listing", r.Status)
	}
	if r := order(buyer, market, 30); r.Status != 201 {
		t.Fatal("order", r.Status, r.Body)
	}
	if q := e.scalar(`SELECT quantity::float8 FROM listings WHERE id = $1`, listing).(float64); q != 70 {
		t.Fatal("listing decremented", q)
	}
	var id string
	var rate float64
	var src string
	if err := e.db.Primary().QueryRow(t0(), `SELECT id::text, maker_fee_rate::float8, source_listing_id::text FROM trades
		WHERE source_listing_id = $1 AND buyer_party_id = $2 ORDER BY created_at LIMIT 1`, listing, e.partyOf(buyerID)).Scan(&id, &rate, &src); err != nil {
		t.Fatal(err)
	}
	if rate != 0.005 {
		t.Fatal("maker fee", rate)
	}
	d := e.call(buyer, "GET", "/me/transactions/"+id, nil)
	if !strings.HasPrefix(d.Body["title"].(string), "Kopi robusta · 30 kg (MKT-") || d.Body["terms"] != "escrow" {
		t.Fatal(d.Body)
	}

	// The KYC limit applies to the order value (email level: Rp 10 jt).
	big := e.call(seller, "POST", "/me/listings", supplyBody("Kopi mahal", 1_000_000, 100, "kg"))
	e.exec(`UPDATE listings SET market_id = $2, status = 'in_market' WHERE id = $1`, big.Body["id"], market)
	if r := e.call(buyer, "POST", "/markets/"+market+"/orders", map[string]any{"listingId": big.Body["id"], "quantity": 11}); r.Status != 403 || r.code() != "kyc_limit" {
		t.Fatal("kyc limit", r.Status, r.Body)
	}
	// Buying the rest sells the listing out.
	if r := e.call(buyer, "POST", "/markets/"+market+"/orders", map[string]any{"listingId": big.Body["id"], "quantity": 10}); r.Status != 201 {
		t.Fatal(r.Status, r.Body)
	}
	e.exec(`UPDATE listings SET quantity = 5 WHERE id = $1`, big.Body["id"])
	if r := e.call(buyer, "POST", "/markets/"+market+"/orders", map[string]any{"listingId": big.Body["id"], "quantity": 5}); r.Status != 201 {
		t.Fatal(r.Status, r.Body)
	}
	if st := e.scalar(`SELECT status FROM listings WHERE id = $1`, big.Body["id"]).(string); st != "sold" {
		t.Fatal("sold out", st)
	}
	if n := e.scalar(`SELECT count(*) FROM listing_events WHERE listing_id = $1 AND note LIKE 'Order langsung%'`, big.Body["id"]).(int64); n != 2 {
		t.Fatal("listing history", n)
	}
}
