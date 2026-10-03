package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Integration tests for notifications, opportunities (engine + personal), matches, reputation, profiles, dashboard,
// search and the ClickHouse-down behaviour of the public reads.

func uniq(prefix string) string { return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()) }

func (e *testEnv) notifyDirect(userID, typ, title string) {
	e.t.Helper()
	if err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		return notify(t0(), tx, userID, notification{Type: typ, Title: title, Body: "b", Href: "/x"})
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestNotifications(t *testing.T) {
	e := newEnv(t)
	c, _ := e.signedIn("Nina")
	me := e.userID(c)
	e.notifyDirect(me, "outbid", "Satu")
	e.notifyDirect(me, "payment", "Dua")
	list := e.callList(c, "/me/notifications")
	if len(list) != 2 || list[0]["title"] != "Dua" || list[0]["read"] != false {
		t.Fatalf("list: %v", list)
	}
	if r := e.call(c, "POST", "/me/notifications/read", map[string]any{"ids": []string{list[1]["id"].(string), "nope"}}); r.Status != 204 {
		t.Fatalf("read one: %d %v", r.Status, r.Body)
	}
	list = e.callList(c, "/me/notifications")
	if list[0]["read"] != false || list[1]["read"] != true {
		t.Fatalf("after read one: %v", list)
	}
	if r := e.call(c, "POST", "/me/notifications/read", map[string]any{}); r.Status != 204 {
		t.Fatalf("read all: %d", r.Status)
	}
	if list = e.callList(c, "/me/notifications"); list[0]["read"] != true {
		t.Fatalf("after read all: %v", list)
	}

	r := e.call(c, "GET", "/me/notification-prefs", nil)
	op, _ := r.Body["outbid"].(map[string]any)
	od, _ := r.Body["opportunity_detected"].(map[string]any)
	if r.Status != 200 || op["inApp"] != true || op["email"] != true || od["inApp"] != true || od["email"] != false {
		t.Fatalf("defaults: %d %v", r.Status, r.Body)
	}
	prefs := r.Body
	prefs["outbid"] = map[string]any{"inApp": false, "email": false}
	if r := e.call(c, "PUT", "/me/notification-prefs", prefs); r.Status != 200 || r.Body["outbid"].(map[string]any)["inApp"] != false {
		t.Fatalf("save: %d %v", r.Status, r.Body)
	}
	e.notifyDirect(me, "outbid", "Tidak tersimpan")
	e.notifyDirect(me, "payment", "Tersimpan")
	if list = e.callList(c, "/me/notifications"); len(list) != 3 || list[0]["title"] != "Tersimpan" {
		t.Fatalf("in-app off: %v", list)
	}
	delete(prefs, "delivery")
	if r := e.call(c, "PUT", "/me/notification-prefs", prefs); r.Status != 422 {
		t.Fatalf("missing key: %d", r.Status)
	}
	if r := e.call(e.client(), "GET", "/me/notifications", nil); r.Status != 401 {
		t.Fatalf("anon: %d", r.Status)
	}
}

func energyDemand(unit, location string, qty float64) map[string]any {
	b := demandBody("Panel surya 3 kWp", 20_000_000)
	b["categoryId"], b["location"] = "energy", location
	b["quantity"] = map[string]any{"value": qty, "unit": unit}
	return b
}

func TestOpportunityEngineAndPersonal(t *testing.T) {
	e := newEnv(t)
	unit := uniq("paket")
	var buyers []*http.Client
	var listings []string
	for i, loc := range []string{"Denpasar, Bali", "Bali", "Kuta, Bali"} {
		c, _ := e.signedIn(fmt.Sprintf("Pembeli %d", i))
		r := e.call(c, "POST", "/me/listings", energyDemand(unit, loc, 100))
		if r.Status != 201 {
			t.Fatalf("listing: %d %v", r.Status, r.Body)
		}
		buyers = append(buyers, c)
		listings = append(listings, r.Body["id"].(string))
	}
	fan, _ := e.signedIn("Penggemar Energi")
	fanID := e.userID(fan)
	e.exec(`UPDATE identities SET pref_categories = '{energy}', pref_locations = '{Bali}' WHERE user_id = $1`, fanID)

	if err := e.server.OpportunityTick(t0()); err != nil {
		t.Fatal(err)
	}
	var oppID, kind, mechanism string
	var demand, supply float64
	var participants int
	if err := e.db.Primary().QueryRow(t0(), `
		SELECT id::text, kind, suggested_mechanism, demand_value::float8, supply_value::float8, participant_count FROM opportunities WHERE unit = $1`, unit).
		Scan(&oppID, &kind, &mechanism, &demand, &supply, &participants); err != nil {
		t.Fatal(err)
	}
	if kind != "collective_demand" || mechanism != "collective_procurement" || demand != 300 || supply != 0 || participants != 3 {
		t.Fatalf("detected: %s %s %v %v %d", kind, mechanism, demand, supply, participants)
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'opportunity.detected' AND aggregate_id = $1`, oppID); n != int64(1) {
		t.Fatalf("fact: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'activity' AND payload->>'title' LIKE '%' || $1`, "Bali"); n.(int64) < 1 {
		t.Fatal("activity not emitted")
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND href = $2`, fanID, "/opportunities/"+oppID); n != int64(1) {
		t.Fatalf("fan notified: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE href = $1`, "/opportunities/"+oppID); n != int64(4) {
		t.Fatalf("owners + fan notified: %v", n)
	}
	// Idempotent; a 4th buyer refreshes the totals.
	c4, _ := e.signedIn("Pembeli 4")
	r := e.call(c4, "POST", "/me/listings", energyDemand(unit, "Denpasar", 50))
	l4 := r.Body["id"].(string)
	if err := e.server.OpportunityTick(t0()); err != nil {
		t.Fatal(err)
	}
	if n := e.scalar(`SELECT count(*) FROM opportunities WHERE unit = $1`, unit); n != int64(1) {
		t.Fatalf("duplicate: %v", n)
	}
	if d := e.scalar(`SELECT demand_value::float8 FROM opportunities WHERE id = $1`, oppID); d != 350.0 {
		t.Fatalf("refresh: %v", d)
	}

	// Public list and detail (visitors see initials).
	r = e.call(e.client(), "GET", "/opportunities?q="+unit[:5]+"&category=energy&region=Bali&status=detected&pageSize=50", nil)
	if r.Status != 200 {
		t.Fatalf("list: %d %v", r.Status, r.Body)
	}
	r = e.call(e.client(), "GET", "/opportunities/"+oppID, nil)
	preview, _ := r.Body["participantsPreview"].([]any)
	if r.Status != 200 || len(preview) != 4 || !strings.Contains(preview[0].(map[string]any)["name"].(string), "•••") {
		t.Fatalf("visitor detail: %d %v", r.Status, r.Body)
	}
	hist := r.Body["history"].([]any)
	if len(hist) != 6 || hist[5].(map[string]any)["demand"] != 350.0 {
		t.Fatalf("history: %v", hist)
	}
	if r := e.call(buyers[0], "GET", "/opportunities/"+oppID, nil); strings.Contains(fmt.Sprint(r.Body["participantsPreview"]), "•••") {
		t.Fatalf("member detail masked: %v", r.Body["participantsPreview"])
	}
	if r := e.call(e.client(), "GET", "/opportunities/nope", nil); r.Status != 404 {
		t.Fatalf("404: %d", r.Status)
	}

	// Personal view, join (replace on re-join), follow, leave.
	mine := e.callList(c4, "/me/opportunities?tab=collective")
	var po map[string]any
	for _, o := range mine {
		if o["id"] == oppID {
			po = o
		}
	}
	if po == nil || po["relation"] != "none" || !strings.Contains(fmt.Sprint(po["reasons"]), "Kapasitas cocok") {
		t.Fatalf("personal: %v", po)
	}
	join := func(c *http.Client, listing string, qty float64) resp {
		return e.call(c, "POST", "/me/opportunities/"+oppID+"/join", map[string]any{"kind": "demand", "listingId": listing,
			"quantity": map[string]any{"value": qty, "unit": unit}})
	}
	if r := join(c4, listings[0], 10); r.Status != 422 || r.field("listingId") == "" {
		t.Fatalf("foreign listing: %d %v", r.Status, r.Body)
	}
	if r := join(c4, l4, 0); r.Status != 422 {
		t.Fatalf("zero qty: %d %v", r.Status, r.Body)
	}
	if r := join(c4, l4, 30); r.Status != 200 || r.Body["relation"] != "joined" || r.Body["demand"].(map[string]any)["value"] != 380.0 ||
		r.Body["participants"] != 5.0 {
		t.Fatalf("join: %d %v", r.Status, r.Body)
	}
	if r := join(c4, l4, 40); r.Status != 200 || r.Body["demand"].(map[string]any)["value"] != 390.0 || r.Body["participants"] != 5.0 ||
		r.Body["contribution"].(map[string]any)["quantity"].(map[string]any)["value"] != 40.0 {
		t.Fatalf("re-join replaces: %d %v", r.Status, r.Body)
	}
	if got := e.callList(c4, "/me/opportunities?tab=joined"); len(got) != 1 || got[0]["id"] != oppID {
		t.Fatalf("joined tab: %v", got)
	}
	if r := e.call(fan, "POST", "/me/opportunities/"+oppID+"/follow", nil); r.Status != 204 {
		t.Fatalf("follow: %d", r.Status)
	}
	if r := e.call(fan, "POST", "/me/opportunities/00000000-0000-0000-0000-000000000000/follow", nil); r.Status != 404 {
		t.Fatalf("follow 404: %d", r.Status)
	}
	if got := e.callList(fan, "/me/opportunities?tab=following"); len(got) != 1 {
		t.Fatalf("following tab: %v", got)
	}
	if r := e.call(c4, "DELETE", "/me/opportunities/"+oppID, nil); r.Status != 204 {
		t.Fatalf("leave: %d", r.Status)
	}
	if d := e.scalar(`SELECT demand_value::float8 FROM opportunities WHERE id = $1`, oppID); d != 350.0 {
		t.Fatalf("leave subtracts: %v", d)
	}
	if got := e.callList(c4, "/me/opportunities?tab=joined"); len(got) != 0 {
		t.Fatalf("left: %v", got)
	}
}

func TestMatchesConnect(t *testing.T) {
	e := newEnv(t)
	unit := uniq("jam")
	marketID := e.seedMarket(uniq("Talenta "), "it", unit, "active", "auto")
	maker, makerEmail := e.signedIn("Dimas Maker")
	makerID := e.userID(maker)
	_ = makerEmail
	var oppID string
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO opportunities (title, kind, category_id, region, unit, demand_value, supply_value, participant_count, potential_value_idr,
		                           suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
		VALUES ($1, 'supply_gap', 'it', 'DI Yogyakarta', $2, 100, 10, 4, 400000000, 'direct_market', 0.8, 'r', 'd', 'Freelancer: Go')
		RETURNING id::text`, "Backend "+unit, unit).Scan(&oppID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE markets SET opportunity_id = $1 WHERE id = $2`, oppID, marketID)
	e.exec(`INSERT INTO market_operators (market_id, user_id) VALUES ($1, $2)`, marketID, makerID)

	c, _ := e.signedIn("Fajar Dev")
	me := e.userID(c)
	body := supplyBody("Jasa backend Go", 150000, 50, unit)
	body["categoryId"], body["location"] = "it", "Sleman, Yogyakarta"
	r := e.call(c, "POST", "/me/listings", body)
	if r.Status != 201 {
		t.Fatalf("listing: %d %v", r.Status, r.Body)
	}
	listingID := r.Body["id"].(string)
	e.exec(`INSERT INTO capacity_items (user_id, position, kind, name, detail) VALUES ($1, 0, 'skill', 'Backend developer', 'Go, 5 tahun')`, me)

	ms := e.callList(c, "/me/matches")
	var m map[string]any
	idOf := listingID + "--" + oppID
	for _, x := range ms {
		if x["id"] == idOf {
			m = x
		}
	}
	if m == nil {
		t.Fatalf("matches: %v", ms)
	}
	parts := m["parts"].(map[string]any)
	if m["state"] != "new" || parts["category"] != 35.0 || parts["coverage"] != 14.0 || parts["confidence"] != 12.0 ||
		m["estimatedValueIdr"] != 7_500_000.0 || m["need"].(map[string]any)["gap"].(map[string]any)["value"] != 90.0 {
		t.Fatalf("match: %v", m)
	}
	if !strings.Contains(fmt.Sprint(ms), "identity") {
		t.Fatalf("identity item not matched: %v", ms)
	}

	r = e.call(c, "POST", "/me/matches/"+idOf, map[string]any{"action": "connect", "reason": "  tertarik  "})
	conv, _ := r.Body["conversationId"].(string)
	if r.Status != 200 || r.Body["state"] != "connected" || conv == "" {
		t.Fatalf("connect: %d %v", r.Status, r.Body)
	}
	if s := e.scalar(`SELECT subject FROM conversations WHERE id = $1`, conv); s != "Match: Backend "+unit {
		t.Fatalf("subject: %v", s)
	}
	if n := e.scalar(`SELECT count(*) FROM conversation_participants WHERE conversation_id = $1`, conv); n != int64(2) {
		t.Fatalf("participants: %v", n)
	}
	if b := e.scalar(`SELECT body FROM messages WHERE conversation_id = $1 AND seq = 1`, conv).(string); !strings.HasPrefix(b, "Halo, saya punya Jasa backend Go") {
		t.Fatalf("opening message: %v", b)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title = 'Fajar Dev ingin terhubung' AND href = $2`,
		makerID, "/app/messages/"+conv); n != int64(1) {
		t.Fatalf("maker notified: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM opportunity_follows WHERE user_id = $1 AND opportunity_id = $2`, me, oppID); n != int64(1) {
		t.Fatal("connect follows")
	}
	if r := e.call(c, "POST", "/me/matches/"+idOf, map[string]any{"action": "connect"}); r.Body["conversationId"] != conv {
		t.Fatalf("reconnect reuses: %v", r.Body)
	}
	if r := e.call(c, "POST", "/me/matches/"+idOf, map[string]any{"action": "dismiss"}); r.Status != 200 || r.Body["state"] != "dismissed" {
		t.Fatalf("dismiss: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/me/matches/nope--nope", map[string]any{"action": "save"}); r.Status != 404 {
		t.Fatalf("404: %d", r.Status)
	}
	if r := e.call(c, "POST", "/me/matches/"+idOf, map[string]any{"action": "explode"}); r.Status != 422 {
		t.Fatalf("422: %d", r.Status)
	}
}

// completedTrade creates a trade between two users and walks it to completed, with a 5-star review for the supplier.
func (e *testEnv) completedTrade(buyer, supplier, title string) string {
	e.t.Helper()
	var id string
	if err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		b, err := userParty(t0(), tx, buyer)
		if err != nil {
			return err
		}
		s, err := userParty(t0(), tx, supplier)
		if err != nil {
			return err
		}
		t, err := createTrade(t0(), tx, newTrade{Title: title, BuyerParty: b, SupplierParty: s, Quantity: 10, Unit: "kg", UnitPriceIdr: 1000,
			DeliveryAddress: "Bandung", Via: "direct", Category: "agri", Region: "Jawa Barat"})
		id = t.ID
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE trades SET status = 'completed' WHERE id = $1`, id)
	e.exec(`INSERT INTO trade_events (trade_id, status, at) VALUES ($1, 'invoiced', now() + interval '1 hour'), ($1, 'completed', now() + interval '2 hours')`, id)
	e.exec(`INSERT INTO reviews (trade_id, side, rating, quality, timeliness, communication, text) VALUES ($1, 'buyer', 5, 5, 5, 5, 'Mantap')`, id)
	return id
}

func TestReputationProfilesDashboard(t *testing.T) {
	e := newEnv(t)
	cb, _ := e.signedIn("Budi Pembeli")
	cs, sellerEmail := e.signedIn("Sari Penjual")
	buyer, seller := e.userID(cb), e.userID(cs)
	trade := e.completedTrade(buyer, seller, "Kopi 10 kg")
	if r := e.call(cs, "POST", "/me/listings", supplyBody("Kopi arabika", 90000, 20, "kg")); r.Status != 201 {
		t.Fatalf("listing: %d", r.Status)
	}

	r := e.call(cs, "GET", "/me/reputation", nil)
	counts := r.Body["counts"].(map[string]any)
	bd := r.Body["breakdown"].(map[string]any)
	if r.Status != 200 || counts["successful"] != 1.0 || bd["ratingAvg"] != 5.0 || bd["responseHours"] != 1.0 || len(r.Body["trend"].([]any)) != 12 {
		t.Fatalf("reputation: %d %v", r.Status, r.Body)
	}
	if ev := r.Body["events"].([]any); len(ev) != 1 || ev[0].(map[string]any)["title"] != "Transaksi selesai: Kopi 10 kg" {
		t.Fatalf("events: %v", ev)
	}

	username := e.scalar(`SELECT username::text FROM users WHERE id = $1`, seller).(string)
	r = e.call(e.client(), "GET", "/profiles/u/"+username, nil)
	if r.Status != 200 || r.Body["name"] != "Sari Penjual" || len(r.Body["supply"].([]any)) != 1 ||
		r.Body["reputation"].(map[string]any)["counts"].(map[string]any)["successful"] != 1.0 {
		t.Fatalf("person profile: %d %v", r.Status, r.Body)
	}
	if raw := fmt.Sprint(r.Body); strings.Contains(raw, sellerEmail) || strings.Contains(raw, "Budi") {
		t.Fatalf("profile leaks private data: %v", raw)
	}
	if !strings.Contains(fmt.Sprint(r.Body["activity"]), "Menyelesaikan transaksi Kopi 10 kg") {
		t.Fatalf("activity: %v", r.Body["activity"])
	}
	if r := e.call(e.client(), "GET", "/profiles/u/tidak-ada", nil); r.Status != 404 {
		t.Fatalf("404: %d", r.Status)
	}

	// Business profiles: an external market maker by slugified name, and a verified org by slug.
	name := uniq("Pasar ")
	e.seedMarket(name, "agri", "kg", "active", "auto")
	slug := slugify("Koperasi " + name)
	r = e.call(e.client(), "GET", "/profiles/b/"+slug, nil)
	if r.Status != 200 || len(r.Body["markets"].([]any)) != 1 || !strings.Contains(r.Body["description"].(string), "mengoperasikan 1 market") {
		t.Fatalf("maker profile: %d %v", r.Status, r.Body)
	}
	orgSlug := uniq("pt-maju-")
	var orgID string
	if err := e.db.Primary().QueryRow(t0(), `INSERT INTO orgs (name, slug) VALUES ($1, $2) RETURNING id::text`, orgSlug, orgSlug).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO org_profiles (org_id, location, verification, nib) VALUES ($1, 'Bandung, Jawa Barat', 'verified', '1234567890123')`, orgID)
	r = e.call(e.client(), "GET", "/profiles/b/"+orgSlug, nil)
	if r.Status != 200 || r.Body["verified"] != true || r.Body["region"] != "Jawa Barat" || len(r.Body["documents"].([]any)) != 3 ||
		strings.Contains(fmt.Sprint(r.Body), "1234567890123") {
		t.Fatalf("org profile: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "GET", "/profiles/b/tidak-ada-"+orgSlug, nil); r.Status != 404 {
		t.Fatalf("404: %d", r.Status)
	}

	// Dashboard: a fresh trade waiting for the buyer's acceptance.
	if err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		b, _ := userParty(t0(), tx, buyer)
		s, _ := userParty(t0(), tx, seller)
		_, err := createTrade(t0(), tx, newTrade{Title: "Gula 5 kg", BuyerParty: b, SupplierParty: s, Quantity: 5, Unit: "kg", UnitPriceIdr: 15000,
			DeliveryAddress: "Bandung", Via: "direct", Category: "food", Region: "Jawa Barat"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := e.call(cb, "POST", "/me/listings", demandBody("Gula aren", 900000)); r.Status != 201 {
		t.Fatalf("demand: %d", r.Status)
	}
	r = e.call(cb, "GET", "/me/dashboard", nil)
	st := r.Body["stats"].(map[string]any)
	if r.Status != 200 || st["openDemands"] != 1.0 || st["runningTransactions"] != 1.0 || r.Body["connections"].(map[string]any)["count"] != 1.0 {
		t.Fatalf("dashboard: %d %v", r.Status, r.Body)
	}
	if !strings.Contains(fmt.Sprint(r.Body["actions"]), "Menunggu kamu: 1 aksi") {
		t.Fatalf("actions: %v", r.Body["actions"])
	}
	if r.Body["activity"] == nil || r.Body["liveBids"] == nil || r.Body["topMatches"] == nil {
		t.Fatalf("arrays: %v", r.Body)
	}
	if st := e.call(cs, "GET", "/me/dashboard", nil).Body["stats"].(map[string]any); st["earnings30dIdr"] != 10000.0 || st["currentOffers"] != 1.0 {
		t.Fatalf("seller stats: %v", st)
	}
	_ = trade
}

func TestSearchAndPublicWithoutAnalytics(t *testing.T) {
	e := newEnv(t)
	tok := uniq("zq")
	mkt := e.seedMarket("Market "+tok, "it", "jam", "active", "auto")
	e.exec(`INSERT INTO auctions (title, market_id, category_id, type, visibility, lot_item, lot_spec, quantity, unit, opening_price_idr, starts_at, ends_at)
		VALUES ($1, $2, 'it', 'reverse', 'full', $3, 'Go', 10, 'jam', 100000, now(), now() + interval '1 hour')`, "Lelang "+tok, mkt, "Jam backend "+tok)
	c, _ := e.signedIn("Penjual Cari")
	b := supplyBody("Kopi "+tok, 1000, 5, "kg")
	if r := e.call(c, "POST", "/me/listings", b); r.Status != 201 {
		t.Fatalf("listing: %d", r.Status)
	}
	hits := e.callList(e.client(), "/search?q="+tok)
	types := map[string]int{}
	for _, h := range hits {
		types[h["type"].(string)]++
	}
	if types["market"] != 1 || types["auction"] != 1 || types["service"] != 1 || types["product"] != 1 {
		t.Fatalf("hits: %v", hits)
	}
	maker := e.callList(e.client(), "/search?type=business&q=Koperasi+Market+"+tok)
	if len(maker) != 1 || maker[0]["href"] != "/b/"+slugify("Koperasi Market "+tok) {
		t.Fatalf("business: %v", maker)
	}
	if got := e.callList(e.client(), "/search?q="+tok+"&type=product"); len(got) != 1 {
		t.Fatalf("type filter: %v", got)
	}
	if got := e.callList(e.client(), "/search?q=%20"); len(got) != 0 {
		t.Fatalf("empty q: %v", got)
	}

	// No ClickHouse: zeros and empty lists, never an error.
	if r := e.call(e.client(), "GET", "/public/stats", nil); r.Status != 200 || r.Body["activeMarkets"] != 0.0 {
		t.Fatalf("stats: %d %v", r.Status, r.Body)
	}
	if got := e.callList(e.client(), "/public/activity?limit=5"); len(got) != 0 {
		t.Fatalf("activity: %v", got)
	}
	r := e.call(e.client(), "GET", "/explorer/overview?range=7d", nil)
	if r.Status != 200 || len(r.Body["volume"].([]any)) != 7 || len(r.Body["priceIndex"].([]any)) != 7 || r.Body["demandSupply"] == nil {
		t.Fatalf("overview: %d %v", r.Status, r.Body)
	}
	if got := e.callList(e.client(), "/explorer/demand?category=agri"); len(got) != 0 {
		t.Fatalf("aggregates: %v", got)
	}
	if r := e.call(e.client(), "GET", "/explorer/sideways", nil); r.Status != 404 {
		t.Fatalf("side 404: %d", r.Status)
	}
	if r := e.call(e.client(), "GET", "/explorer/overview?range=1y", nil); r.Status != 422 {
		t.Fatalf("range 422: %d", r.Status)
	}
}
