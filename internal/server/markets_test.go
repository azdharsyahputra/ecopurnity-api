package server

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

func names(r resp) []string {
	var out []string
	for _, m := range r.Body["data"].([]any) {
		out = append(out, m.(map[string]any)["name"].(string))
	}
	return out
}

func TestMarketsPublic(t *testing.T) {
	e := newEnv(t)
	tag := fmt.Sprint(time.Now().UnixNano())
	a := e.seedMarket("Kopi "+tag, "agri", "kg", "active", "auto")
	b := e.seedMarket("Karton "+tag, "packaging", "pcs", "formation", "auto")
	c := e.seedMarket("Pupuk "+tag, "agri", "kg", "closed", "auto")
	draft := e.seedMarket("Draft "+tag, "agri", "kg", "draft", "auto")
	e.exec(`UPDATE markets SET volume_30d_idr = CASE id WHEN $1 THEN 300 WHEN $2 THEN 500 ELSE 100 END WHERE id IN ($1, $2, $3)`, a, b, c)
	e.exec(`UPDATE markets SET region = 'Jawa Tengah' WHERE id = $1`, c)
	anon := e.client()

	// Filters, sort by 30-day volume, pagination; drafts are never listed.
	r := e.call(anon, "GET", "/markets?q="+tag, nil)
	if got := fmt.Sprint(names(r)); r.Status != 200 || got != fmt.Sprint([]string{"Karton " + tag, "Kopi " + tag, "Pupuk " + tag}) || r.Body["meta"].(map[string]any)["total"] != float64(3) {
		t.Fatalf("list: %d %v %v", r.Status, got, r.Body["meta"])
	}
	r = e.call(anon, "GET", "/markets?q="+tag+"&page=2&pageSize=2", nil)
	if got := names(r); len(got) != 1 || got[0] != "Pupuk "+tag || r.Body["meta"].(map[string]any)["total"] != float64(3) {
		t.Fatalf("page 2: %v %v", got, r.Body["meta"])
	}
	for query, want := range map[string]int{"&status=active,formation": 2, "&category=packaging": 1, "&region=jawa%20tengah": 1, "&status=draft": 0} {
		if r := e.call(anon, "GET", "/markets?q="+tag+query, nil); r.Body["meta"].(map[string]any)["total"] != float64(want) {
			t.Fatalf("%s: %v", query, r.Body["meta"])
		}
	}
	if r := e.call(anon, "GET", "/markets?q=Koperasi%20Kopi%20"+tag, nil); len(names(r)) != 1 {
		t.Fatalf("maker name: %v", r.Body)
	}

	// Detail: 404s.
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "nope", draft} {
		if r := e.call(anon, "GET", "/markets/"+id, nil); r.Status != 404 || r.code() != "not_found" {
			t.Fatalf("404 %s: %d %v", id, r.Status, r.Body)
		}
	}

	// Rules: v1 until round 3 has started, then v2.
	rules := `{"eligibility":"verified_docs","visibility":"full","minStepPct":%s,"minQuantity":1250.5,"maxQuantity":400,
	           "windowStart":"2026-03-02","windowEnd":"","region":"Garut","radiusKm":75,"award":"%s"}`
	e.exec(`INSERT INTO market_rule_versions (market_id, version, rules, effective_from_round, author) VALUES
	        ($1, 1, $2, 1, 'Sistem'), ($1, 2, $3, 3, 'Dimas')`, a, fmt.Sprintf(rules, "1.5", "highest_price"), fmt.Sprintf(rules, "2", "score"))
	r = e.call(anon, "GET", "/markets/"+a, nil)
	if r.Status != 200 || r.Body["name"] != "Kopi "+tag || r.Body["buyers"] == nil {
		t.Fatalf("detail: %d %v", r.Status, r.Body)
	}
	want := []string{
		"Eligibility=Akun terverifikasi + dokumen legal usaha", "Visibilitas bid=Harga terlihat, identitas disamarkan",
		"Kenaikan/penurunan minimum=1,5% dari harga pembuka", "Kuantitas minimum=1.250,5 kg per order",
		"Kuantitas maksimum=400 kg per peserta", "Jendela mulai=2 Mar 2026", "Jendela selesai=—", "Wilayah=Garut",
		"Radius=75 km", "Penetapan pemenang=Harga tertinggi",
	}
	if got := labeled(r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rules v1:\n%v\n%v", got, want)
	}
	for _, k := range []string{"priceHistory", "activity", "auctions"} {
		if l, ok := r.Body[k].([]any); !ok || len(l) != 0 {
			t.Fatalf("%s: %v", k, r.Body[k])
		}
	}
	auction := func(round int, status string, ends time.Duration) {
		e.exec(`INSERT INTO auctions (title, market_id, round_no, category_id, type, status, visibility, lot_item, quantity, unit,
		        opening_price_idr, starts_at, ends_at) VALUES ($1, $2, $3, 'agri', 'forward', $4, 'full', 'Kopi', 100, 'kg', 90000, $5, $6)`,
			fmt.Sprintf("Round %d", round), a, round, status, time.Now().Add(ends-time.Hour), time.Now().Add(ends))
	}
	auction(3, "closed", -24*time.Hour)
	auction(4, "live", time.Hour)
	auction(5, "scheduled", 48*time.Hour)
	r = e.call(anon, "GET", "/markets/"+a, nil)
	if got := labeled(r); got[2] != "Kenaikan/penurunan minimum=2% dari harga pembuka" || got[9] != "Penetapan pemenang=Skor harga 70% + kualitas 30%" {
		t.Fatalf("rules v2: %v", got)
	}
	var titles []string
	for _, x := range r.Body["auctions"].([]any) {
		titles = append(titles, x.(map[string]any)["title"].(string))
	}
	if fmt.Sprint(titles) != "[Round 4 Round 5 Round 3]" || r.Body["activeAuctions"] != float64(1) {
		t.Fatalf("auctions: %v active %v", titles, r.Body["activeAuctions"])
	}

	// Analytics reachable (no rows for this market) or down: empty lists, never an error.
	for _, addr := range []string{"localhost:9000", "localhost:1"} {
		ch, err := analytics.Open(addr, "ecopurnity", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		e.server.Analytics = ch
		r = e.call(anon, "GET", "/markets/"+a, nil)
		_ = ch.Close()
		if h, _ := r.Body["priceHistory"].([]any); r.Status != 200 || h == nil || len(h) != 0 || r.Body["activity"] == nil {
			t.Fatalf("analytics %s: %d %v", addr, r.Status, r.Body)
		}
	}
	e.server.Analytics = nil
}

func labeled(r resp) []string {
	var out []string
	for _, x := range r.Body["rules"].([]any) {
		m := x.(map[string]any)
		out = append(out, m["label"].(string)+"="+m["value"].(string))
	}
	return out
}

func TestMarketMembership(t *testing.T) {
	e := newEnv(t)
	tag := fmt.Sprint(time.Now().UnixNano())
	auto := e.seedMarket("Otomatis "+tag, "agri", "kg", "active", "auto")
	manual := e.seedMarket("Manual "+tag, "agri", "kg", "formation", "manual")
	closed := e.seedMarket("Tutup "+tag, "agri", "kg", "paused", "auto")
	c, _ := e.signedIn("Rani " + tag)
	op, opEmail := e.signedIn("Operator " + tag)
	opID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, opEmail)
	e.exec(`INSERT INTO market_operators (market_id, user_id) VALUES ($1, $2)`, manual, opID)
	participant := func(market string) any {
		return e.scalar(`SELECT coalesce(string_agg(mp.role || '/' || mp.status, ','), '') FROM market_participants mp
		                 JOIN parties p ON p.id = mp.party_id JOIN users u ON u.id = p.user_id WHERE mp.market_id = $1 AND u.name = $2`, market, "Rani "+tag)
	}
	notes := func() any {
		return e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'new_market' AND href LIKE '%' || $2 || '%'`, opID, manual)
	}
	myMarket := func(id string) map[string]any {
		t.Helper()
		res, err := c.Get(e.srv.URL + BasePath + "/me/markets")
		if err != nil {
			t.Fatal(err)
		}
		var list []any
		err = json.NewDecoder(res.Body).Decode(&list)
		res.Body.Close()
		if res.StatusCode != 200 || err != nil {
			t.Fatalf("me/markets: %d %v", res.StatusCode, err)
		}
		for _, x := range list {
			if m := x.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("market %s not in /me/markets", id)
		return nil
	}

	if r := e.call(e.client(), "POST", "/me/markets/"+auto+"/join", nil); r.Status != 401 {
		t.Fatalf("anon join: %d", r.Status)
	}
	if m := myMarket(auto); m["joined"] != false || m["approval"] != nil || m["myListings"] != float64(0) {
		t.Fatalf("before join: %v", m)
	}

	// Auto approval: active buyer right away, no operator notification.
	if r := e.call(c, "POST", "/me/markets/"+auto+"/join", nil); r.Status != 204 {
		t.Fatalf("join auto: %d %v", r.Status, r.Body)
	}
	if p := participant(auto); p != "buyer/active" {
		t.Fatalf("auto participant: %v", p)
	}
	if m := myMarket(auto); m["joined"] != true || m["approval"] != "active" {
		t.Fatalf("after join: %v", m)
	}

	// Manual approval: pending, operators notified once; joining again changes nothing.
	for range 2 {
		if r := e.call(c, "POST", "/me/markets/"+manual+"/join", nil); r.Status != 204 {
			t.Fatalf("join manual: %d %v", r.Status, r.Body)
		}
	}
	if p, n := participant(manual), notes(); p != "buyer/pending" || n != int64(1) {
		t.Fatalf("manual: participant %v notifications %v", p, n)
	}
	if m := myMarket(manual); m["joined"] != true || m["approval"] != "pending" {
		t.Fatalf("manual state: %v", m)
	}

	// Closed / missing markets.
	if r := e.call(c, "POST", "/me/markets/"+closed+"/join", nil); r.Status != 409 || r.code() != "market_closed" {
		t.Fatalf("closed: %d %v", r.Status, r.Body)
	}
	for _, path := range []string{"/join", "/leave"} {
		if r := e.call(c, "POST", "/me/markets/00000000-0000-0000-0000-000000000000"+path, nil); r.Status != 404 || r.code() != "not_found" {
			t.Fatalf("missing %s: %d %v", path, r.Status, r.Body)
		}
	}

	// Watch: validated, kept separately from joined, 0 clears.
	if r := e.call(c, "PUT", "/me/markets/"+auto+"/watch", map[string]any{"priceIdr": 90000}); r.Status != 204 {
		t.Fatalf("watch: %d %v", r.Status, r.Body)
	}
	if m := myMarket(auto); m["watchPriceIdr"] != float64(90000) || m["joined"] != true {
		t.Fatalf("watch state: %v", m)
	}
	if r := e.call(c, "PUT", "/me/markets/"+auto+"/watch", map[string]any{"priceIdr": -5}); r.Status != 422 || r.field("priceIdr") == "" {
		t.Fatalf("negative watch: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "PUT", "/me/markets/nope/watch", map[string]any{"priceIdr": 1}); r.Status != 404 {
		t.Fatalf("watch missing market: %d", r.Status)
	}
	e.call(c, "PUT", "/me/markets/"+auto+"/watch", map[string]any{"priceIdr": 0})
	if m := myMarket(auto); m["watchPriceIdr"] != nil {
		t.Fatalf("watch cleared: %v", m)
	}
	// Watching a market not joined does not join it.
	e.call(c, "PUT", "/me/markets/"+closed+"/watch", map[string]any{"priceIdr": 5000})
	if m := myMarket(closed); m["joined"] != false || m["watchPriceIdr"] != float64(5000) {
		t.Fatalf("watch only: %v", m)
	}

	// Leave: pending request withdrawn, active participation kept; joined and watch cleared.
	e.call(c, "PUT", "/me/markets/"+manual+"/watch", map[string]any{"priceIdr": 1000})
	if r := e.call(c, "POST", "/me/markets/"+manual+"/leave", nil); r.Status != 204 {
		t.Fatalf("leave: %d %v", r.Status, r.Body)
	}
	if m, p := myMarket(manual), participant(manual); m["joined"] != false || m["approval"] != nil || m["watchPriceIdr"] != nil || p != "" {
		t.Fatalf("after leave: %v participant %v", m, p)
	}
	e.call(c, "POST", "/me/markets/"+auto+"/leave", nil)
	if p := participant(auto); p != "buyer/active" {
		t.Fatalf("active kept: %v", p)
	}

	// myListings counts the caller's listings in the market.
	id := e.call(c, "POST", "/me/listings", supplyBody("Kopi "+tag, 80000, 10, "kg")).Body["id"].(string)
	e.call(c, "POST", "/me/listings/"+id+"/market", map[string]any{"marketId": auto})
	if m := myMarket(auto); m["myListings"] != float64(1) || m["joined"] != true {
		t.Fatalf("myListings: %v", m)
	}

	// Restricted accounts cannot join; operators get nothing for their own request.
	e.exec(`UPDATE users SET status = 'restricted' WHERE name = $1`, "Rani "+tag)
	if r := e.call(c, "POST", "/me/markets/"+manual+"/join", nil); r.Status != 403 || r.code() != "account_restricted" {
		t.Fatalf("restricted: %d %v", r.Status, r.Body)
	}
	if r := e.call(op, "POST", "/me/markets/"+manual+"/join", nil); r.Status != 204 || notes() != int64(1) {
		t.Fatalf("operator self-join: %d notifications %v", r.Status, notes())
	}
}
