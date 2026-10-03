package server

import (
	"fmt"
	"testing"
	"time"
)

// The demo bots act on a tick, from database state; tests age the rows instead of waiting.
func TestSimulatedCounterparties(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Bu Simulasi")
	mitra := fmt.Sprintf("Mitra Uji %d", time.Now().UnixNano())
	r := e.call(buyer, "POST", "/me/rfqs", rfqBody("Kentang granola", map[string]any{"inviteNames": []string{mitra}}))
	if r.Status != 201 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	id, conv := r.Body["id"].(string), r.Body["conversationId"].(string)
	tick := func() {
		t.Helper()
		if err := e.server.CounterpartyTick(t0()); err != nil {
			t.Fatal(err)
		}
	}

	// Not due yet: nothing. 12 s old: the first two bots (invited one first). 20 s: the third.
	tick()
	e.exec(`UPDATE rfqs SET created_at = now() - interval '12 seconds' WHERE id = $1`, id)
	tick()
	r = e.call(buyer, "GET", "/me/rfqs/"+id, nil)
	if q := quotesOf(r); len(q) != 2 || q[0]["supplier"].(map[string]any)["name"] != mitra || q[0]["priceIdr"] != 47000.0 ||
		q[1]["supplier"].(map[string]any)["name"] != "Koperasi Mitra Tani" || q[1]["priceIdr"] != 49500.0 {
		t.Fatalf("bot quotes at 12 s: %v", q)
	}
	e.exec(`UPDATE rfqs SET created_at = now() - interval '20 seconds' WHERE id = $1`, id)
	tick()
	tick() // idempotent
	r = e.call(buyer, "GET", "/me/rfqs/"+id, nil)
	q := quotesOf(r)
	if len(q) != 3 || q[2]["supplier"].(map[string]any)["verified"] != false || q[2]["terms"] != "net30" || q[2]["leadTimeDays"] != 7.0 {
		t.Fatalf("bot quotes at 20 s: %v", q)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE 'Penawaran baru untuk %'`, buyerID); n != int64(3) {
		t.Fatalf("quote notifications: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM conversation_participants WHERE conversation_id = $1`, conv); n != int64(4) {
		t.Fatalf("bots joined the conversation: %v", n)
	}

	// A low counter: the bot meets halfway after 6 s.
	low, close := q[0]["id"].(string), q[1]["id"].(string)
	act := func(qid string, body map[string]any) resp {
		return e.call(buyer, "POST", "/me/rfqs/"+id+"/quotes/"+qid+"/actions", body)
	}
	if r := act(low, map[string]any{"action": "counter", "priceIdr": 40000}); r.Status != 200 {
		t.Fatalf("counter: %d %v", r.Status, r.Body)
	}
	tick()
	if s := e.scalar(`SELECT status FROM quotes WHERE id = $1`, low); s != "countered" {
		t.Fatalf("answered too early: %v", s)
	}
	e.exec(`UPDATE quote_events SET at = at - interval '7 seconds' WHERE quote_id = $1`, low)
	tick()
	var status string
	var price int64
	if err := e.db.Primary().QueryRow(t0(), `SELECT status, price_idr FROM quotes WHERE id = $1`, low).Scan(&status, &price); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" || price != 43500 {
		t.Fatalf("bot revise: %s %d", status, price)
	}

	// A counter within 5%: the bot accepts, which awards the RFQ and creates the trade.
	if r := act(close, map[string]any{"action": "counter", "priceIdr": 48000}); r.Status != 200 {
		t.Fatalf("counter: %d %v", r.Status, r.Body)
	}
	e.exec(`UPDATE quote_events SET at = at - interval '7 seconds' WHERE quote_id = $1`, close)
	tick()
	r = e.call(buyer, "GET", "/me/rfqs/"+id, nil)
	if r.Body["status"] != "awarded" || r.Body["transactionId"] == nil {
		t.Fatalf("bot accept: %v", r.Body)
	}
	if v := e.scalar(`SELECT unit_price_idr FROM trades WHERE id = $1`, r.Body["transactionId"]); v != int64(48000) {
		t.Fatalf("trade price: %v", v)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE '% menerima tawaran balikmu'`, buyerID); n != int64(1) {
		t.Fatalf("buyer told: %v", n)
	}

	// Chat: the first external participant answers the latest human message once.
	if r := e.call(buyer, "POST", "/me/conversations/"+conv+"/messages", map[string]any{"text": "Bisa kirim Senin?"}); r.Status != 201 {
		t.Fatalf("send: %d %v", r.Status, r.Body)
	}
	tick()
	e.exec(`UPDATE conversations SET last_message_at = now() - interval '6 seconds' WHERE id = $1`, conv)
	tick()
	e.exec(`UPDATE conversations SET last_message_at = now() - interval '6 seconds' WHERE id = $1`, conv)
	tick()
	r = e.call(buyer, "GET", "/me/conversations/"+conv, nil)
	msgs := r.Body["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); len(msgs) != 2 || last["by"] != mitra || last["userId"] != nil || last["text"] != botReplies[1] {
		t.Fatalf("bot reply: %v", msgs)
	}
}
