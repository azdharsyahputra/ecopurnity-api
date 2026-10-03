package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// bidder registers a user with a verified email (qualification needs it) and returns the client and user id.
func (e *testEnv) bidder(name string) (*http.Client, string) {
	e.t.Helper()
	c, email := e.signedIn(name)
	e.exec(`UPDATE users SET email_verified_at = now() WHERE email = $1`, email)
	return c, e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
}

func (e *testEnv) qualify(c *http.Client, auctionID string) {
	e.t.Helper()
	if r := e.call(c, "POST", "/auctions/"+auctionID+"/qualification", map[string]any{"documentName": "spek.pdf", "acceptRules": true}); r.Status != 200 {
		e.t.Fatalf("qualify: %d %v", r.Status, r.Body)
	}
}

// frames returns the realtime frames queued in the outbox for a channel, oldest first.
func (e *testEnv) frames(channel string) []map[string]any {
	e.t.Helper()
	rows, err := e.db.Primary().Query(t0(), `SELECT payload FROM outbox WHERE topic = 'rt' AND aggregate_id = $1 ORDER BY id`, channel)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			e.t.Fatal(err)
		}
		var f map[string]any
		_ = json.Unmarshal(b, &f)
		out = append(out, f)
	}
	return out
}

// buyerAuction: a buyer with a demand opens a reverse auction (opening 10.000/kg, step 500, 100 kg).
func (e *testEnv) buyerAuction(visibility, typ string) (*http.Client, string, string) {
	e.t.Helper()
	buyer, _ := e.bidder("Pembeli")
	d := e.call(buyer, "POST", "/me/listings", demandBody("Gula aren", 2_000_000))
	if d.Status != 201 {
		e.t.Fatalf("demand: %v", d.Body)
	}
	r := e.call(buyer, "POST", "/me/auctions", map[string]any{"demandId": d.Body["id"], "type": typ, "durationMinutes": 120,
		"openingPriceIdr": 10000, "minStepIdr": 500, "visibility": visibility, "invite": []string{}})
	if r.Status != 201 {
		e.t.Fatalf("create auction: %d %v", r.Status, r.Body)
	}
	return buyer, r.Body["id"].(string), d.Body["id"].(string)
}

func TestBuyerAuctionBiddingAndAward(t *testing.T) {
	e := newEnv(t)
	buyer, auctionID, demandID := e.buyerAuction("full", "reverse")

	// Creating again for the same demand, and bad inputs.
	if r := e.call(buyer, "POST", "/me/auctions", map[string]any{"demandId": demandID, "type": "reverse", "durationMinutes": 120, "openingPriceIdr": 10000,
		"minStepIdr": 500, "visibility": "full", "invite": []string{}}); r.Status != 409 || r.code() != "already_in_auction" {
		t.Fatalf("second auction: %d %v", r.Status, r.Body)
	}
	if d := e.call(buyer, "GET", "/me/listings/"+demandID, nil); d.Body["status"] != "in_market" || d.Body["auctionId"] != auctionID {
		t.Fatalf("demand after auction: %v", d.Body)
	}

	b1, b1ID := e.bidder("Supplier Satu")
	b2, _ := e.bidder("Supplier Dua")
	bid := func(c *http.Client, price int) resp {
		return e.call(c, "POST", "/auctions/"+auctionID+"/bids", map[string]any{"priceIdr": price})
	}

	if r := bid(b1, 9500); r.Status != 403 || r.code() != "not_qualified" {
		t.Fatalf("unqualified: %d %v", r.Status, r.Body)
	}
	e.qualify(b1, auctionID)
	e.qualify(b2, auctionID)
	e.qualify(buyer, auctionID)
	if r := bid(buyer, 9000); r.Status != 403 || r.code() != "owner" {
		t.Fatalf("owner bid: %d %v", r.Status, r.Body)
	}
	if r := bid(b1, 10001); r.Status != 422 || r.code() != "invalid_bid" || r.field("price") != "Bid harus ≤ Rp 10.000" {
		t.Fatalf("above opening: %d %v", r.Status, r.Body)
	}
	r := bid(b1, 9500)
	if r.Status != 200 || r.Body["status"] != "leading" || r.Body["rank"] != float64(1) {
		t.Fatalf("first bid: %d %v", r.Status, r.Body)
	}
	if r := bid(b2, 9400); r.Status != 422 || r.field("price") != "Bid harus ≤ Rp 9.000" {
		t.Fatalf("step: %d %v", r.Status, r.Body)
	}
	if r := bid(b2, 9000); r.Status != 200 || r.Body["status"] != "leading" {
		t.Fatalf("second bid: %d %v", r.Status, r.Body)
	}
	// b1 is outbid, notified, and sees rank 2.
	if r := e.call(b1, "GET", "/auctions/"+auctionID+"/me", nil); r.Body["bid"].(map[string]any)["status"] != "outbid" || r.Body["bid"].(map[string]any)["rank"] != float64(2) {
		t.Fatalf("b1 state: %v", r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'outbid'`, b1ID); n != int64(1) {
		t.Fatalf("outbid notifications: %v", n)
	}

	// Public view: full visibility shows prices with masked names; only the viewer's own bid is "mine".
	pub := e.call(e.client(), "GET", "/auctions/"+auctionID, nil)
	bids := pub.Body["bids"].([]any)
	if pub.Body["currentPriceIdr"] != float64(9000) || len(bids) != 2 || !strings.HasPrefix(bids[0].(map[string]any)["bidder"].(string), "Supplier ") || bids[0].(map[string]any)["mine"] != nil {
		t.Fatalf("public: %v", pub.Body)
	}
	mine := e.call(b2, "GET", "/auctions/"+auctionID, nil).Body["bids"].([]any)[0].(map[string]any)
	if mine["mine"] != true || strings.Contains(fmt.Sprint(pub.Body), "Supplier Dua") {
		t.Fatalf("mine flag / name leak: %v", mine)
	}

	// Realtime: gapless seq on the auction channel, masked frames, bid.status to the outbid user.
	fr := e.frames("auction:" + auctionID)
	if len(fr) != 2 || fr[0]["seq"] != float64(1) || fr[1]["seq"] != float64(2) || fr[1]["type"] != "auction.bid" {
		t.Fatalf("auction frames: %v", fr)
	}
	var sawOutbid bool
	for _, f := range e.frames("user:" + b1ID) {
		if f["type"] == "bid.status" && f["payload"].(map[string]any)["status"] == "outbid" {
			sawOutbid = true
		}
	}
	if !sawOutbid {
		t.Fatal("no bid.status outbid frame")
	}

	// Withdraw: the outbid bid can go (plenty of time left); the leading one can't.
	if r := e.call(b2, "DELETE", "/auctions/"+auctionID+"/bids/mine", nil); r.Status != 409 || r.code() != "cannot_withdraw" {
		t.Fatalf("withdraw leading: %d %v", r.Status, r.Body)
	}
	if r := e.call(b1, "DELETE", "/auctions/"+auctionID+"/bids/mine", nil); r.Status != 200 || r.Body["status"] != "withdrawn" {
		t.Fatalf("withdraw: %d %v", r.Status, r.Body)
	}

	// Anti-sniping: a bid in the last 2 minutes adds 5, at most 3 times.
	e.exec(`UPDATE auctions SET ends_at = now() + interval '1 minute' WHERE id = $1`, auctionID)
	b3, _ := e.bidder("Supplier Tiga")
	e.qualify(b3, auctionID)
	for i, price := range []int{8500, 8000, 7500, 7000} {
		if i > 0 {
			e.exec(`UPDATE auctions SET ends_at = now() + interval '1 minute' WHERE id = $1`, auctionID)
		}
		c := b3
		if i%2 == 1 {
			c = b2
		}
		if r := bid(c, price); r.Status != 200 {
			t.Fatalf("snipe bid %d: %d %v", i, r.Status, r.Body)
		}
	}
	if n := e.scalar(`SELECT extension_count FROM auctions WHERE id = $1`, auctionID); n != int32(3) {
		t.Fatalf("extensions: %v", n)
	}
	if st := e.scalar(`SELECT status FROM auctions WHERE id = $1`, auctionID); st != "extended" {
		t.Fatalf("status: %v", st)
	}

	// Award before close is refused; the clock closes it.
	if r := e.call(buyer, "POST", "/auctions/"+auctionID+"/award", map[string]any{"lines": []any{map[string]any{"offerId": "x", "supplier": "x", "quantity": 1, "priceIdr": 1}}}); r.Status != 409 || r.code() != "not_closed" {
		t.Fatalf("award before close: %d %v", r.Status, r.Body)
	}
	e.exec(`UPDATE auctions SET starts_at = now() - interval '1 hour', ends_at = now() - interval '1 second' WHERE id = $1`, auctionID)
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if st := e.scalar(`SELECT status || '/' || clearing_price_idr FROM auctions WHERE id = $1`, auctionID); st != "closed/7000" {
		t.Fatalf("closed: %v", st)
	}

	// Evaluation: owner only, offers best first.
	if r := e.call(b1, "GET", "/auctions/"+auctionID+"/evaluation", nil); r.Status != 404 {
		t.Fatalf("evaluation by non-owner: %d", r.Status)
	}
	ev := e.call(buyer, "GET", "/auctions/"+auctionID+"/evaluation", nil)
	offers := ev.Body["offers"].([]any)
	if ev.Status != 200 || len(offers) != 2 || offers[0].(map[string]any)["priceIdr"] != float64(7000) {
		t.Fatalf("evaluation: %d %v", ev.Status, ev.Body)
	}
	best := offers[0].(map[string]any)
	second := offers[1].(map[string]any)

	// Award: split 60/40, prices taken from the offers (not the request); a bogus offer is rejected.
	if r := e.call(buyer, "POST", "/auctions/"+auctionID+"/award", map[string]any{"lines": []any{map[string]any{"offerId": "bogus", "supplier": "x", "quantity": 10, "priceIdr": 1}}}); r.Status != 422 {
		t.Fatalf("bogus offer: %d %v", r.Status, r.Body)
	}
	r = e.call(buyer, "POST", "/auctions/"+auctionID+"/award", map[string]any{"lines": []any{
		map[string]any{"offerId": best["id"], "supplier": "x", "quantity": 60, "priceIdr": 1},
		map[string]any{"offerId": second["id"], "supplier": "x", "quantity": 40, "priceIdr": 1},
	}})
	if r.Status != 200 || len(r.Body["transactionIds"].([]any)) != 2 {
		t.Fatalf("award: %d %v", r.Status, r.Body)
	}
	if v := e.scalar(`SELECT sum(total_idr)::bigint FROM trades WHERE auction_id = $1`, auctionID); fmt.Sprint(v) != fmt.Sprint(60*7000+40*7500) {
		t.Fatalf("trade totals: %v", v)
	}
	if st := e.scalar(`SELECT string_agg(status, ',' ORDER BY status) FROM (SELECT DISTINCT ON (bidder_party_id) status FROM bids WHERE auction_id = $1 AND status <> 'withdrawn' ORDER BY bidder_party_id, seq DESC) x`, auctionID); st != "won,won" {
		t.Fatalf("bid statuses: %v", st)
	}
	if d := e.call(buyer, "GET", "/me/listings/"+demandID, nil); d.Body["status"] != "matched" {
		t.Fatalf("demand after award: %v", d.Body["status"])
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = 'Tetapkan pemenang'`, auctionID); n != int64(1) {
		t.Fatalf("audit: %v", n)
	}
	if st := e.scalar(`SELECT status FROM auctions WHERE id = $1`, auctionID); st != "awarded" {
		t.Fatalf("auction after award: %v", st)
	}

	// My auctions: the supplier sees its bid, the buyer its owned auction.
	if r := e.call(b2, "GET", "/me/auctions", nil); len(r.Body["bids"].([]any)) != 1 {
		t.Fatalf("my bids: %v", r.Body)
	}
	if r := e.call(buyer, "GET", "/me/auctions", nil); len(r.Body["owned"].([]any)) != 1 {
		t.Fatalf("owned: %v", r.Body)
	}
}

func TestSealedAuctionHidesPrices(t *testing.T) {
	e := newEnv(t)
	_, auctionID, _ := e.buyerAuction("full", "sealed") // visibility is forced to sealed
	b1, b1ID := e.bidder("Sealed Satu")
	e.qualify(b1, auctionID)
	r := e.call(b1, "POST", "/auctions/"+auctionID+"/bids", map[string]any{"priceIdr": 9100})
	if r.Status != 200 || r.Body["status"] != "submitted" || r.Body["rank"] != nil {
		t.Fatalf("sealed bid: %d %v", r.Status, r.Body)
	}
	pub := e.call(e.client(), "GET", "/auctions/"+auctionID, nil)
	if pub.Body["visibility"] != "sealed" || len(pub.Body["bids"].([]any)) != 0 || pub.Body["currentPriceIdr"] != nil {
		t.Fatalf("sealed public view: %v", pub.Body)
	}
	f := e.frames("auction:" + auctionID)[0]
	if f["payload"].(map[string]any)["bid"].(map[string]any)["priceIdr"] != float64(0) || f["payload"].(map[string]any)["currentPriceIdr"] != nil {
		t.Fatalf("sealed frame leaks price: %v", f)
	}
	// The bidder's own channel carries the real price.
	own := e.frames("user:" + b1ID)
	if own[len(own)-1]["payload"].(map[string]any)["bid"].(map[string]any)["priceIdr"] != float64(9100) {
		t.Fatalf("own frame: %v", own)
	}
}

func TestDutchAndMarketRound(t *testing.T) {
	e := newEnv(t)
	market := e.seedMarket("Gula Aren Toba", "food", "kg", "active", "auto")
	var dutch string
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO auctions (title, market_id, round_no, category_id, type, status, visibility, lot_item, quantity, unit, opening_price_idr, min_step_idr, starts_at, ends_at, updated_at)
		VALUES ('Aren Toba dutch', $1, 1, 'food', 'dutch', 'live', 'full', 'Gula aren', 100, 'kg', 30000, 1000, now(), now() + interval '1 hour', now() - interval '1 minute')
		RETURNING id::text`, market).Scan(&dutch); err != nil {
		t.Fatal(err)
	}
	// The clock steps the ask down (once per tick pass, paced by dutchStepEvery).
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if p := e.scalar(`SELECT current_price_idr FROM auctions WHERE id = $1`, dutch); p != int64(29000) {
		t.Fatalf("dutch step: %v", p)
	}
	buyer, _ := e.bidder("Pembeli Dutch")
	if r := e.call(buyer, "POST", "/auctions/"+dutch+"/accept", nil); r.Status != 403 || r.code() != "not_qualified" {
		t.Fatalf("unqualified accept: %d %v", r.Status, r.Body)
	}
	e.qualify(buyer, dutch)
	// KYC level 0 allows Rp 10 jt; 100 kg × 29.000 = Rp 2,9 jt.
	r := e.call(buyer, "POST", "/auctions/"+dutch+"/accept", nil)
	if r.Status != 200 || r.Body["transactionId"] == nil {
		t.Fatalf("accept: %d %v", r.Status, r.Body)
	}
	if v := e.scalar(`SELECT t.unit_price_idr || '/' || (p.kind) FROM trades t JOIN parties p ON p.id = t.supplier_party_id WHERE t.auction_id = $1`, dutch); v != "29000/external" {
		t.Fatalf("dutch trade: %v", v)
	}
	if r := e.call(buyer, "POST", "/auctions/"+dutch+"/accept", nil); r.Status != 409 {
		t.Fatalf("second accept: %d", r.Status)
	}

	// A market round (no owner): the clock marks the best bidder won, the rest lost.
	var round string
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO auctions (title, market_id, round_no, category_id, type, status, visibility, lot_item, quantity, unit, opening_price_idr, min_step_idr, starts_at, ends_at)
		VALUES ('Aren Toba round 2', $1, 2, 'food', 'forward', 'scheduled', 'rank_only', 'Gula aren', 10, 'kg', 30000, 500, now() - interval '1 second', now() + interval '1 hour')
		RETURNING id::text`, market).Scan(&round); err != nil {
		t.Fatal(err)
	}
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if st := e.scalar(`SELECT status FROM auctions WHERE id = $1`, round); st != "live" {
		t.Fatalf("start: %v", st)
	}
	x, xID := e.bidder("Pembeli X")
	y, _ := e.bidder("Pembeli Y")
	e.qualify(x, round)
	e.qualify(y, round)
	if r := e.call(x, "POST", "/auctions/"+round+"/bids", map[string]any{"priceIdr": 30500}); r.Status != 200 {
		t.Fatalf("forward bid: %d %v", r.Status, r.Body)
	}
	if r := e.call(y, "POST", "/auctions/"+round+"/bids", map[string]any{"priceIdr": 30500}); r.Status != 422 || r.field("price") != "Bid harus ≥ Rp 31.000" {
		t.Fatalf("forward step: %d %v", r.Status, r.Body)
	}
	e.call(y, "POST", "/auctions/"+round+"/bids", map[string]any{"priceIdr": 31000})
	// rank_only: public prices hidden.
	if pub := e.call(e.client(), "GET", "/auctions/"+round, nil); pub.Body["currentPriceIdr"] != nil || len(pub.Body["bids"].([]any)) != 0 {
		t.Fatalf("rank_only public: %v", pub.Body)
	}
	e.exec(`UPDATE auctions SET starts_at = now() - interval '1 hour', ends_at = now() - interval '1 second' WHERE id = $1`, round)
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if me := e.call(x, "GET", "/auctions/"+round+"/me", nil); me.Body["bid"].(map[string]any)["status"] != "lost" {
		t.Fatalf("x after close: %v", me.Body)
	}
	if me := e.call(y, "GET", "/auctions/"+round+"/me", nil); me.Body["bid"].(map[string]any)["status"] != "won" {
		t.Fatalf("y after close: %v", me.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'auction_ending'`, xID); n != int64(1) {
		t.Fatalf("loser notified: %v", n)
	}

	// Public list: filters and the live-first order.
	list := e.call(e.client(), "GET", "/auctions?q=Aren%20Toba&status=closed,awarded", nil)
	if list.Status != 200 || list.Body["meta"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("list: %d %v", list.Status, list.Body["meta"])
	}
}

func TestBidKYCLimit(t *testing.T) {
	e := newEnv(t)
	market := e.seedMarket("Besar", "agri", "ton", "active", "auto")
	var id string
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO auctions (title, market_id, round_no, category_id, type, status, visibility, lot_item, quantity, unit, opening_price_idr, min_step_idr, starts_at, ends_at)
		VALUES ('Lot besar', $1, 1, 'agri', 'reverse', 'live', 'full', 'Kopi', 100, 'ton', 1000000, 1000, now(), now() + interval '1 hour') RETURNING id::text`, market).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c, _ := e.bidder("Kecil")
	e.qualify(c, id)
	// 100 ton × Rp 900.000 = Rp 90 jt > Rp 10 jt (email-only level).
	if r := e.call(c, "POST", "/auctions/"+id+"/bids", map[string]any{"priceIdr": 900000}); r.Status != 403 || r.code() != "kyc_limit" {
		t.Fatalf("kyc: %d %v", r.Status, r.Body)
	}
}
