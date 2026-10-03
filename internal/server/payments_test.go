package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/payments"
)

func (e *testEnv) fake() *payments.Fake { return e.server.Payments.(*payments.Fake) }

// payViaGateway pays as c at base (/me/transactions/{id} or /orgs/{org}/transactions/{id}) with a BCA VA, lets the
// fake gateway settle it and runs the reconciler; returns the trade read back, which must be in `status`.
func (e *testEnv) payViaGateway(c *http.Client, base, status string) resp {
	e.t.Helper()
	r := e.call(c, "POST", base+"/payments", map[string]any{"method": "bank_transfer", "bank": "bca"})
	if r.Status != 201 || r.Body["status"] != "pending" {
		e.t.Fatalf("create payment: %d %v", r.Status, r.Body)
	}
	e.fake().SetStatus(r.Body["orderId"].(string), "settlement")
	if err := e.server.ReconcilePayments(t0()); err != nil {
		e.t.Fatal(err)
	}
	got := e.call(c, "GET", base, nil)
	if got.Status != 200 || got.Body["status"] != status {
		e.t.Fatalf("after payment want %s: %d %v", status, got.Status, got.Body)
	}
	return got
}

// notifyMidtrans posts a notification signed with the fake gateway's key.
func (e *testEnv) notifyMidtrans(order, statusCode, gross, txStatus string) resp {
	e.t.Helper()
	return e.call(e.client(), "POST", "/payments/midtrans/notification", map[string]any{"order_id": order, "status_code": statusCode, "gross_amount": gross,
		"transaction_status": txStatus, "signature_key": payments.Signature(order, statusCode, gross, e.fake().Key), "payment_type": "bank_transfer"})
}

func TestPaymentFlowAndWebhook(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Rina Bayar")
	supplier, supplierID := e.bidder("Ajar Terima")
	id := e.newTestTrade(newTrade{Title: "Kopi 100 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID), Quantity: 100, UnitPriceIdr: 10_000})
	base := "/me/transactions/" + id
	pay := func(c *http.Client, body map[string]any) resp { return e.call(c, "POST", base+"/payments", body) }
	bca := map[string]any{"method": "bank_transfer", "bank": "bca"}

	if r := pay(buyer, bca); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatal("before the invoice", r.Status, r.Body)
	}
	e.mustAct(buyer, id, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, id, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, id, map[string]any{"action": "issue_invoice"}, "invoiced")
	if r := e.call(buyer, "GET", base+"/payments/current", nil); r.Status != 200 || r.Body != nil {
		t.Fatal("no payment yet: null", r.Status, r.Body)
	}
	if r := pay(supplier, bca); r.Status != 409 {
		t.Fatal("the supplier does not pay", r.Status)
	}
	if r := pay(buyer, map[string]any{"method": "bank_transfer"}); r.Status != 422 || r.field("bank") == "" {
		t.Fatal("VA without a bank", r.Status, r.Body)
	}
	if r := pay(buyer, map[string]any{"method": "credit_card"}); r.Status != 422 {
		t.Fatal("no cards", r.Status)
	}
	if r := e.act(buyer, id, map[string]any{"action": "pay"}); r.Status != 409 || r.code() != "payment_required" {
		t.Fatal("pay action is closed to users", r.Status, r.Body)
	}

	// A VA: pending, 24 h, the engine's amount (subtotal 1.000.000 + PPN).
	va := pay(buyer, bca)
	if va.Status != 201 || va.Body["status"] != "pending" || num(va.Body["amountIdr"]) != 1_110_000 || va.Body["bank"] != "bca" ||
		!strings.HasPrefix(va.Body["vaNumber"].(string), "39021") || !strings.Contains(va.Body["orderId"].(string), "-1-") {
		t.Fatalf("VA: %d %v", va.Status, va.Body)
	}
	if exp, _ := time.Parse(time.RFC3339, va.Body["expiresAt"].(string)); time.Until(exp) < 23*time.Hour {
		t.Fatal("VA expiry", exp)
	}
	if r := e.call(buyer, "GET", base+"/payments/current", nil); r.Body["id"] != va.Body["id"] {
		t.Fatal("current", r.Body)
	}
	if r := e.call(supplier, "GET", base+"/payments/current", nil); r.Status != 200 || r.Body != nil {
		t.Fatal("the supplier has no payments", r.Body)
	}
	if err := e.server.ReconcilePayments(t0()); err != nil {
		t.Fatal(err)
	}
	if r := e.call(buyer, "GET", base, nil); r.Body["status"] != "invoiced" {
		t.Fatal("pending moves nothing", r.Body["status"])
	}

	// "Ganti metode": the old one is cancelled at the gateway, one pending per invoice.
	qr := pay(buyer, map[string]any{"method": "qris"})
	if qr.Status != 201 || !strings.HasPrefix(qr.Body["qrUrl"].(string), "data:image/svg+xml") || qr.Body["bank"] != nil {
		t.Fatal("QRIS", qr.Status, qr.Body)
	}
	if st, _ := e.fake().Status(t0(), va.Body["orderId"].(string)); st.TransactionStatus != "cancel" {
		t.Fatal("old payment cancelled at the gateway", st)
	}
	if n := e.scalar(`SELECT count(*) FROM payments WHERE trade_id = $1 AND status = 'pending'`, id).(int64); n != 1 {
		t.Fatal("pending payments", n)
	}
	if r := e.call(buyer, "POST", base+"/payments/current/cancel", nil); r.Status != 200 || r.Body["status"] != "cancel" {
		t.Fatal("cancel", r.Status, r.Body)
	}
	if r := e.call(buyer, "POST", base+"/payments/current/cancel", nil); r.Status != 409 {
		t.Fatal("nothing left to cancel", r.Status)
	}

	// Webhook: a bad signature is refused; a signed notification is confirmed with the gateway, never trusted.
	gp := pay(buyer, map[string]any{"method": "gopay"})
	order := gp.Body["orderId"].(string)
	if gp.Status != 201 || gp.Body["deeplinkUrl"] == nil || gp.Body["qrUrl"] == nil {
		t.Fatal("gopay", gp.Status, gp.Body)
	}
	bad := e.call(e.client(), "POST", "/payments/midtrans/notification", map[string]any{"order_id": order, "status_code": "200", "gross_amount": "1110000.00", "signature_key": "nope"})
	if bad.Status != 403 {
		t.Fatal("bad signature", bad.Status)
	}
	if r := e.notifyMidtrans(order, "200", "1110000.00", "settlement"); r.Status != 200 {
		t.Fatal("signed notification", r.Status, r.Body)
	}
	if r := e.call(buyer, "GET", base, nil); r.Body["status"] != "invoiced" {
		t.Fatal("the gateway still says pending: nothing moves", r.Body["status"])
	}
	e.fake().SetStatus(order, "settlement")
	if r := e.notifyMidtrans(order, "200", "1110000.00", "settlement"); r.Status != 200 {
		t.Fatal(r.Status)
	}
	got := e.call(buyer, "GET", base, nil)
	if got.Body["status"] != "paid" || got.Body["payment"].(map[string]any)["status"] != "escrow" {
		t.Fatal("settled", got.Body)
	}
	// Same ledger as the old pay step: 1.110.000 held in escrow for the buyer, receivable for the supplier (minus the 1% platform fee).
	fb := e.call(buyer, "GET", "/me/finance", nil)
	if num(fb.Body["escrowHeldIdr"]) != 1_110_000 || len(fb.Body["entries"].([]any)) != 1 {
		t.Fatalf("buyer finance: %v", fb.Body)
	}
	if fs := e.call(supplier, "GET", "/me/finance", nil); num(fs.Body["receivableIdr"]) != 1_100_000 {
		t.Fatalf("supplier finance: %v", fs.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM trade_events WHERE trade_id = $1 AND status = 'paid' AND note = $2`, id, "Dana masuk escrow · Midtrans GoPay · "+order).(int64); n != 1 {
		t.Fatal("event note carries the payment reference", n)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'payment' AND title LIKE '%: Dana masuk escrow'`, supplierID).(int64); n != 1 {
		t.Fatal("supplier notified", n)
	}
	if v := e.scalar(`SELECT last_notification::text FROM payments WHERE order_id = $1`, order).(string); strings.Contains(v, "signature_key") || !strings.Contains(v, "payment_type") {
		t.Fatal("stored notification", v)
	}

	// Duplicates and the reconciler change nothing; an unknown order is acknowledged.
	entries := e.scalar(`SELECT count(*) FROM ledger_entries WHERE trade_id = $1`, id).(int64)
	if r := e.notifyMidtrans(order, "200", "1110000.00", "settlement"); r.Status != 200 {
		t.Fatal(r.Status)
	}
	if err := e.server.ReconcilePayments(t0()); err != nil {
		t.Fatal(err)
	}
	if n := e.scalar(`SELECT count(*) FROM ledger_entries WHERE trade_id = $1`, id).(int64); n != entries {
		t.Fatal("duplicate notification moved money", entries, n)
	}
	if r := e.notifyMidtrans("TRX-NOPE-1-abcd", "200", "5.00", "settlement"); r.Status != 200 {
		t.Fatal("unknown order", r.Status)
	}
	if r := pay(buyer, bca); r.Status != 409 {
		t.Fatal("paid: no new payment", r.Status)
	}
}

func TestPaymentExpiry(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Budi Kedaluwarsa")
	supplier, supplierID := e.bidder("Sari Supplier")
	id := e.newTestTrade(newTrade{Title: "Gula 10 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID), Quantity: 10, UnitPriceIdr: 15_000})
	base := "/me/transactions/" + id
	e.mustAct(buyer, id, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, id, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, id, map[string]any{"action": "issue_invoice"}, "invoiced")

	// The gateway expires it.
	r := e.call(buyer, "POST", base+"/payments", map[string]any{"method": "echannel"})
	if r.Status != 201 || r.Body["bank"] != "mandiri" || r.Body["billKey"] == nil || r.Body["billerCode"] == nil {
		t.Fatal("mandiri bill", r.Status, r.Body)
	}
	e.fake().SetStatus(r.Body["orderId"].(string), "expire")
	if err := e.server.ReconcilePayments(t0()); err != nil {
		t.Fatal(err)
	}
	if c := e.call(buyer, "GET", base+"/payments/current", nil); c.Body["status"] != "expire" {
		t.Fatal("expired", c.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'payment' AND title LIKE '%: Pembayaran kedaluwarsa'`, buyerID).(int64); n != 1 {
		t.Fatal("buyer notified", n)
	}
	if c := e.call(buyer, "GET", base, nil); c.Body["status"] != "invoiced" {
		t.Fatal("trade stays invoiced", c.Body["status"])
	}

	// Overdue while the gateway still says pending: expired locally.
	r = e.call(buyer, "POST", base+"/payments", map[string]any{"method": "shopeepay"})
	if r.Status != 201 || r.Body["deeplinkUrl"] == nil {
		t.Fatal("shopeepay", r.Status, r.Body)
	}
	e.exec(`UPDATE payments SET expires_at = now() - interval '2 minutes' WHERE id = $1`, r.Body["id"])
	if err := e.server.ReconcilePayments(t0()); err != nil {
		t.Fatal(err)
	}
	if c := e.call(buyer, "GET", base+"/payments/current", nil); c.Body["status"] != "expire" {
		t.Fatal("overdue", c.Body)
	}
	// A new attempt still works.
	e.payViaGateway(buyer, base, "paid")
}

func TestOrgPayments(t *testing.T) {
	e := newEnv(t)
	_, _, buyerOrg := e.workspace("Org Bayar")
	finance, _ := e.member(buyerOrg, "finance")
	procurement, _ := e.member(buyerOrg, "procurement")
	ops, _ := e.member(buyerOrg, "operations")
	supplier, supplierID := e.bidder("Pemasok Perorangan")
	party, err := orgParty(t0(), e.db.Primary(), buyerOrg)
	if err != nil {
		t.Fatal(err)
	}
	id := e.newTestTrade(newTrade{Title: "Karton 500 pcs", BuyerParty: party, SupplierParty: e.partyOf(supplierID), Quantity: 500, Unit: "pcs", UnitPriceIdr: 2_000})
	base := "/orgs/" + buyerOrg + "/transactions/" + id
	for _, a := range []string{"accept_agreement"} {
		if r := e.call(procurement, "POST", base+"/actions", map[string]any{"action": a}); r.Status != 200 {
			t.Fatal(a, r.Status, r.Body)
		}
	}
	e.mustAct(supplier, id, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, id, map[string]any{"action": "issue_invoice"}, "invoiced")

	bca := map[string]any{"method": "bank_transfer", "bank": "bni"}
	if r := e.call(procurement, "POST", base+"/payments", bca); r.Status != 403 || r.message() != "Bayar hanya untuk Owner, Finance; peranmu Procurement" {
		t.Fatal("procurement pays", r.Status, r.Body)
	}
	outsider, _ := e.bidder("Bukan Anggota")
	if r := e.call(outsider, "GET", base+"/payments/current", nil); r.Status != 403 {
		t.Fatal("outsider", r.Status)
	}
	r := e.call(finance, "POST", base+"/payments", bca)
	if r.Status != 201 || num(r.Body["amountIdr"]) != 1_110_000 {
		t.Fatal("finance pays", r.Status, r.Body)
	}
	if c := e.call(ops, "GET", base+"/payments/current", nil); c.Status != 200 || c.Body["id"] != r.Body["id"] {
		t.Fatal("team sees the payment", c.Status, c.Body)
	}
	if c := e.call(procurement, "POST", base+"/payments/current/cancel", nil); c.Status != 403 {
		t.Fatal("procurement cancels", c.Status)
	}
	e.fake().SetStatus(r.Body["orderId"].(string), "settlement")
	if err := e.server.ReconcilePayments(t0()); err != nil {
		t.Fatal(err)
	}
	page := e.call(finance, "GET", base, nil)
	if page.Body["status"] != "paid" {
		t.Fatal("paid", page.Body["status"])
	}
	acts := page.Body["activity"].([]any)
	if a := acts[0].(map[string]any); a["action"] != "Dana masuk escrow" || !strings.HasSuffix(a["actor"].(string), "(Finance)") {
		t.Fatal("org activity: the finance member paid", a)
	}
}
