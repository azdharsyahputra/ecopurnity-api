package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Port of the frontend's src/domain/rfq.test.ts.
func TestQuoteRules(t *testing.T) {
	cases := []struct {
		status, side string
		action       api.QuoteAction
		open         bool
		want         string
	}{
		{"submitted", "buyer", "accept", true, "accepted"},
		{"submitted", "buyer", "counter", true, "countered"},
		{"countered", "buyer", "decline", true, "declined"},
		{"countered", "supplier", "revise", true, "submitted"},
		{"countered", "supplier", "accept_counter", true, "accepted"},
		{"submitted", "supplier", "withdraw", true, "withdrawn"},
		{"submitted", "buyer", "accept", false, ""},   // closes once the RFQ is not open
		{"accepted", "buyer", "accept", true, ""},     // already accepted
		{"submitted", "supplier", "accept", true, ""}, // wrong side
		{"countered", "buyer", "counter", true, ""},
		{"submitted", "supplier", "accept_counter", true, ""},
	}
	for _, c := range cases {
		if got := quoteTransition(c.status, c.side, c.action, c.open); got != c.want {
			t.Errorf("%s/%s/%s open=%v: %q, want %q", c.status, c.side, c.action, c.open, got, c.want)
		}
	}
	counter := int64(90)
	if dealPrice(100, &counter, "accept_counter") != 90 || dealPrice(100, &counter, "accept") != 100 || dealPrice(100, nil, "accept_counter") != 100 {
		t.Error("dealPrice")
	}
	if ok, _ := botCounterReply(1000, 960); !ok {
		t.Error("bot should accept a counter within 5%")
	}
	if ok, p := botCounterReply(1000, 800); ok || p != 900 {
		t.Errorf("bot should meet halfway: %v %d", ok, p)
	}
}

func rfqBody(item string, extra map[string]any) map[string]any {
	b := map[string]any{"item": item, "categoryId": "agri", "quantity": map[string]any{"value": 100, "unit": "kg"}, "targetPriceIdr": 50000,
		"deadline": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339), "location": "Garut, Jawa Barat", "spec": "Grade A"}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func quoteBody(price int) map[string]any {
	return map[string]any{"priceIdr": price, "quantity": 100, "leadTimeDays": 3, "terms": "net14", "note": "Siap kirim"}
}

func quotesOf(r resp) []map[string]any {
	var out []map[string]any
	for _, q := range r.Body["quotes"].([]any) {
		out = append(out, q.(map[string]any))
	}
	return out
}

// callArray is call for endpoints answering a JSON array (200 expected).
func (e *testEnv) callArray(c *http.Client, path string) []map[string]any {
	e.t.Helper()
	res, err := c.Get(e.srv.URL + BasePath + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d %v", path, res.StatusCode, err)
	}
	return out
}

func ids(list []map[string]any) []string {
	var out []string
	for _, it := range list {
		out = append(out, it["id"].(string))
	}
	return out
}

func TestRfqNegotiationAndAward(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Bu Rina")
	sup, supID := e.bidder("Pak Supri")
	inv, invID := e.bidder("Bu Invi")
	outsider, _ := e.bidder("Orang Luar")
	if r := e.call(sup, "POST", "/me/listings", supplyBody("Cabai rawit", 40000, 500, "kg")); r.Status != 201 {
		t.Fatalf("supply listing: %d %v", r.Status, r.Body)
	}
	ext := fmt.Sprintf("PT Undangan %d", time.Now().UnixNano())

	if r := e.call(buyer, "POST", "/me/rfqs", rfqBody("  ", map[string]any{"quantity": map[string]any{"value": 0, "unit": "kg"}})); r.Status != 422 || r.field("item") == "" || r.field("quantity") == "" {
		t.Fatalf("validation: %d %v", r.Status, r.Body)
	}
	if r := e.call(buyer, "POST", "/me/rfqs", rfqBody("Cabai", map[string]any{"source": map[string]any{"kind": "repeat", "id": "00000000-0000-0000-0000-000000000000"}})); r.Status != 422 || r.field("source") == "" {
		t.Fatalf("unknown source: %d %v", r.Status, r.Body)
	}
	r := e.call(buyer, "POST", "/me/rfqs", rfqBody("Cabai rawit merah", map[string]any{"inviteUserIds": []string{invID, buyerID, "nope"}, "inviteNames": []string{ext, " "}}))
	if r.Status != 201 || r.Body["status"] != "open" || r.Body["side"] != "buyer" || !strings.HasPrefix(r.Body["code"].(string), "RFQ-") {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	id, conv := r.Body["id"].(string), r.Body["conversationId"].(string)
	if inv := r.Body["invited"].([]any); len(inv) != 2 {
		t.Fatalf("invited: %v", inv)
	}
	for who, want := range map[string]int64{supID: 1, invID: 1, buyerID: 0} {
		if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'auction_invitation' AND href = $2`, who, "/app/rfq/"+id); n != want {
			t.Fatalf("invitation for %s: %v", who, n)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = 'Buat RFQ'`, id); n != int64(1) {
		t.Fatalf("audit: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM conversation_participants WHERE conversation_id = $1`, conv); n != int64(3) {
		t.Fatalf("conversation participants: %v", n)
	}

	// Visibility.
	if r := e.call(outsider, "GET", "/me/rfqs/"+id, nil); r.Status != 404 {
		t.Fatalf("outsider detail: %d", r.Status)
	}
	if r := e.call(outsider, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(45000)); r.Status != 404 {
		t.Fatalf("outsider quote: %d", r.Status)
	}
	if r := e.call(buyer, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(45000)); r.Status != 404 {
		t.Fatalf("buyer quoting own RFQ: %d", r.Status)
	}

	// Quotes.
	r = e.call(sup, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(48000))
	if r.Status != 201 || r.Body["side"] != "supplier" || len(quotesOf(r)) != 1 {
		t.Fatalf("quote: %d %v", r.Status, r.Body)
	}
	supQuote := quotesOf(r)[0]["id"].(string)
	if h := quotesOf(r)[0]["history"].([]any); len(h) != 1 || h[0].(map[string]any)["text"] != "Penawaran Rp 48.000/kg" {
		t.Fatalf("history: %v", h)
	}
	if r := e.call(sup, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(47000)); r.Status != 409 || r.code() != "duplicate" {
		t.Fatalf("duplicate: %d %v", r.Status, r.Body)
	}
	r = e.call(inv, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(52000))
	if r.Status != 201 {
		t.Fatalf("invited quote: %d %v", r.Status, r.Body)
	}
	invQuote := quotesOf(r)[0]["id"].(string)
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE 'Penawaran baru untuk %' AND href = $2`, buyerID, "/app/rfq/"+id); n != int64(2) {
		t.Fatalf("buyer quote notifications: %v", n)
	}
	if r := e.call(buyer, "GET", "/me/rfqs/"+id, nil); r.Status != 200 || len(quotesOf(r)) != 2 {
		t.Fatalf("buyer sees all quotes: %d %v", r.Status, r.Body)
	}
	if r := e.call(sup, "GET", "/me/rfqs/"+id, nil); r.Status != 200 || len(quotesOf(r)) != 1 || quotesOf(r)[0]["id"] != supQuote {
		t.Fatalf("supplier sees own quote only: %v", r.Body)
	}
	if !slices.Contains(ids(e.callArray(buyer, "/me/rfqs")), id) || slices.Contains(ids(e.callArray(buyer, "/me/rfqs?side=supplier")), id) {
		t.Fatal("buyer lists")
	}
	if !slices.Contains(ids(e.callArray(sup, "/me/rfqs?side=supplier")), id) || slices.Contains(ids(e.callArray(outsider, "/me/rfqs?side=supplier")), id) {
		t.Fatal("supplier lists")
	}
	for _, it := range e.callArray(inv, "/me/rfqs?side=supplier") {
		if it["id"] == id && (it["side"] != "supplier" || len(it["quotes"].([]any)) != 1) {
			t.Fatalf("supplier list shows competitors: %v", it)
		}
	}

	clients := map[string]*http.Client{"buyer": buyer, "sup": sup, "inv": inv, "out": outsider}
	actOn := func(who string, qid string, body map[string]any) resp {
		return e.call(clients[who], "POST", "/me/rfqs/"+id+"/quotes/"+qid+"/actions", body)
	}
	if r := actOn("out", supQuote, map[string]any{"action": "accept"}); r.Status != 403 || r.code() != "forbidden" {
		t.Fatalf("outsider act: %d %v", r.Status, r.Body)
	}
	if r := actOn("inv", supQuote, map[string]any{"action": "withdraw"}); r.Status != 403 {
		t.Fatalf("other supplier act: %d %v", r.Status, r.Body)
	}
	if r := actOn("buyer", supQuote, map[string]any{"action": "revise", "priceIdr": 1}); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("buyer revise: %d %v", r.Status, r.Body)
	}
	if r := actOn("buyer", supQuote, map[string]any{"action": "counter"}); r.Status != 422 || r.field("priceIdr") == "" {
		t.Fatalf("counter without price: %d %v", r.Status, r.Body)
	}
	if r := actOn("buyer", invQuote, map[string]any{"action": "counter", "priceIdr": 200000}); r.Status != 403 || r.code() != "kyc_limit" {
		t.Fatalf("counter above the buyer's limit: %d %v", r.Status, r.Body)
	}
	r = actOn("buyer", supQuote, map[string]any{"action": "counter", "priceIdr": 45000, "note": "Bisa kurang?"})
	if r.Status != 200 {
		t.Fatalf("counter: %d %v", r.Status, r.Body)
	}
	for _, q := range quotesOf(r) {
		if q["id"] == supQuote {
			h := q["history"].([]any)
			if q["status"] != "countered" || q["counterPriceIdr"] != 45000.0 || h[len(h)-1].(map[string]any)["text"] != "Tawar balik Rp 45.000 · Bisa kurang?" {
				t.Fatalf("countered quote: %v", q)
			}
		}
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE '%menawar balik'`, supID); n != int64(1) {
		t.Fatalf("supplier counter notification: %v", n)
	}
	r = actOn("sup", supQuote, map[string]any{"action": "revise", "priceIdr": 47000})
	if r.Status != 200 || len(quotesOf(r)) != 1 || quotesOf(r)[0]["status"] != "submitted" || quotesOf(r)[0]["counterPriceIdr"] != nil || quotesOf(r)[0]["priceIdr"] != 47000.0 {
		t.Fatalf("revise: %d %v", r.Status, r.Body)
	}

	// Accept: trade at the quoted price, the other quote declined, RFQ awarded.
	r = actOn("buyer", supQuote, map[string]any{"action": "accept"})
	if r.Status != 200 || r.Body["status"] != "awarded" || r.Body["transactionId"] == nil {
		t.Fatalf("accept: %d %v", r.Status, r.Body)
	}
	tx := r.Body["transactionId"].(string)
	for _, q := range quotesOf(r) {
		if want := map[string]string{supQuote: "accepted", invQuote: "declined"}[q["id"].(string)]; q["status"] != want {
			t.Fatalf("quote %v after accept", q)
		}
	}
	var total int64
	var terms, quote string
	if err := e.db.Primary().QueryRow(t0(), `SELECT total_idr, terms, source_quote_id::text FROM trades WHERE id = $1`, tx).Scan(&total, &terms, &quote); err != nil {
		t.Fatal(err)
	}
	if total != 4_700_000 || terms != "net14" || quote != supQuote {
		t.Fatalf("trade: %d %s %s", total, terms, quote)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'winning_bid' AND href = $2`, supID, "/app/transactions/"+tx); n != int64(1) {
		t.Fatalf("winning_bid: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = 'Terima penawaran Pak Supri'`, id); n != int64(1) {
		t.Fatalf("accept audit: %v", n)
	}
	if r := actOn("buyer", supQuote, map[string]any{"action": "accept"}); r.Status != 409 {
		t.Fatalf("accept twice: %d", r.Status)
	}
	if r := e.call(buyer, "POST", "/me/rfqs/"+id+"/close", nil); r.Status != 409 || r.code() != "closed" {
		t.Fatalf("close awarded: %d %v", r.Status, r.Body)
	}
	if r := e.call(sup, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(40000)); r.Status != 409 && r.code() != "closed" {
		t.Fatalf("quote on awarded: %d %v", r.Status, r.Body)
	}
}

func TestRfqClose(t *testing.T) {
	e := newEnv(t)
	buyer, _ := e.bidder("Bu Tutup")
	sup, _ := e.bidder("Pak Tutup")
	r := e.call(buyer, "POST", "/me/rfqs", rfqBody("Jagung pipil", map[string]any{"inviteUserIds": []string{e.userID(sup)}}))
	if r.Status != 201 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	id := r.Body["id"].(string)
	if r := e.call(sup, "POST", "/me/rfqs/"+id+"/quotes", quoteBody(3000)); r.Status != 201 {
		t.Fatalf("quote: %d %v", r.Status, r.Body)
	}
	if r := e.call(sup, "POST", "/me/rfqs/"+id+"/close", nil); r.Status != 404 {
		t.Fatalf("supplier close: %d", r.Status)
	}
	r = e.call(buyer, "POST", "/me/rfqs/"+id+"/close", nil)
	if r.Status != 200 || r.Body["status"] != "closed" || quotesOf(r)[0]["status"] != "declined" {
		t.Fatalf("close: %d %v", r.Status, r.Body)
	}
	if r := e.call(buyer, "POST", "/me/rfqs/"+id+"/close", nil); r.Status != 409 || r.code() != "closed" {
		t.Fatalf("close twice: %d %v", r.Status, r.Body)
	}
	// Closed RFQs with a quote of theirs stay in the supplier's list; actions are over.
	if !slices.Contains(ids(e.callArray(sup, "/me/rfqs?side=supplier")), id) {
		t.Fatal("closed RFQ with own quote missing from supplier list")
	}
	q := quotesOf(r)[0]["id"].(string)
	if r := e.call(sup, "POST", "/me/rfqs/"+id+"/quotes/"+q+"/actions", map[string]any{"action": "withdraw"}); r.Status != 409 {
		t.Fatalf("act on closed: %d", r.Status)
	}
	if r := e.call(buyer, "GET", "/me/rfqs/not-a-uuid", nil); r.Status != 404 {
		t.Fatalf("bad id: %d", r.Status)
	}
}
