package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

// maker: a signed-in, email-verified user with the market_maker capability.
func (e *testEnv) maker(name string) (*http.Client, string) {
	e.t.Helper()
	c, id := e.bidder(name)
	e.exec(`INSERT INTO user_capabilities (user_id, capability) VALUES ($1, 'market_maker')`, id)
	return c, id
}

func marketInput(name string) map[string]any {
	return map[string]any{
		"name": name, "objective": "procurement", "mechanism": "reverse_auction", "categoryId": "agri", "unit": "kg",
		"demand": 1000, "supply": 800, "referencePriceIdr": 10000, "autoInvite": true, "approval": "auto", "supplierVerification": "documents",
		"rules": map[string]any{"eligibility": "verified_docs", "visibility": "full", "minStepPct": 1, "minQuantity": 10, "maxQuantity": 400,
			"windowStart": "2026-10-05", "windowEnd": "2026-10-09", "region": "Jawa Barat", "radiusKm": 75, "award": "lowest_price"},
	}
}

func (e *testEnv) createMarket(c *http.Client, body map[string]any) string {
	e.t.Helper()
	r := e.call(c, "POST", "/mm/markets", body)
	if r.Status != 201 {
		e.t.Fatalf("create market: %d %v", r.Status, r.Body)
	}
	return r.Body["id"].(string)
}

func (e *testEnv) notified(userID, title string) bool {
	e.t.Helper()
	return e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE $2`, userID, title).(int64) > 0
}

// org: an organization (owned by the first member) with its party and the other members as active procurement members.
func (e *testEnv) org(name string, owner string, members ...string) (orgID, partyID string) {
	e.t.Helper()
	full := name + " " + fmt.Sprint(time.Now().UnixNano())
	err := pgx.BeginFunc(t0(), e.db.Primary(), func(tx pgx.Tx) error {
		var err error
		orgID, err = createOrg(t0(), tx, newOrg{Name: full, Industry: "Pangan", Location: "Bandung, Jawa Barat", OwnerUserID: owner})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, u := range members {
		e.exec(`INSERT INTO org_members (org_id, user_id, email, name, role, status, joined_at)
			SELECT $1, id, email, name, 'procurement', 'active', now() FROM users WHERE id = $2`, orgID, u)
	}
	return orgID, e.scalar(`SELECT id::text FROM parties WHERE org_id = $1`, orgID).(string)
}

func TestMmAccessAndMarketCreation(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	if r := e.call(anon, "GET", "/mm/overview", nil); r.Status != 401 {
		t.Fatalf("anon: %d", r.Status)
	}
	plain, _ := e.signedIn("Biasa")
	if r := e.call(plain, "GET", "/mm/overview", nil); r.Status != 403 || r.code() != "forbidden" {
		t.Fatalf("non-maker: %d %v", r.Status, r.Body)
	}
	mm, mmID := e.maker("Dimas")
	tag := fmt.Sprint(time.Now().UnixNano())

	// An opportunity with a contributor (listing) and a follower.
	contributor, contributorID := e.bidder("Rina")
	_ = contributor
	follower, followerID := e.signedIn("Fani")
	_ = follower
	followerID = e.scalar(`SELECT id::text FROM users WHERE email = $1`, followerID).(string)
	opp := e.scalar(`INSERT INTO opportunities (title, kind, category_id, region, unit, demand_value, supply_value, potential_value_idr,
		suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
		VALUES ($1, 'collective_demand', 'agri', 'Jawa Barat', 'kg', 1000, 500, 10000000, 'reverse_auction', 0.8, 'alasan', 'desc', 'kontribusi')
		RETURNING id::text`, "Gula "+tag).(string)
	party := e.scalar(`INSERT INTO parties (kind, user_id, name, display_kind) VALUES ('user', $1, 'Rina', 'person') RETURNING id::text`, contributorID).(string)
	listing := e.scalar(`INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, delivery, budget_idr, deadline)
		VALUES (next_code('DEM'), 'demand', 'open', $1, 'Gula aren', 'agri', 200, 'kg', 'Garut', 'both', 2000000, now() + interval '30 days') RETURNING id::text`, party).(string)
	e.exec(`INSERT INTO opportunity_participants (opportunity_id, party_id, role, listing_id, quantity) VALUES ($1, $2, 'buyer', $3, 200)`, opp, party, listing)
	e.exec(`INSERT INTO opportunity_follows (user_id, opportunity_id) VALUES ($1, $2)`, followerID, opp)

	// Validation.
	bad := marketInput(" ")
	bad["referencePriceIdr"] = 0
	bad["rules"].(map[string]any)["maxQuantity"] = 5
	bad["rules"].(map[string]any)["windowEnd"] = "2026-10-01"
	if r := e.call(mm, "POST", "/mm/markets", bad); r.Status != 422 || r.field("name") == "" || r.field("referencePriceIdr") == "" ||
		r.field("maxQuantity") == "" || r.field("windowEnd") == "" {
		t.Fatalf("validation: %d %v", r.Status, r.Body)
	}
	unknown := marketInput("Pasar " + tag)
	unknown["opportunityId"] = "00000000-0000-0000-0000-000000000000"
	if r := e.call(mm, "POST", "/mm/markets", unknown); r.Status != 422 || r.field("opportunityId") == "" {
		t.Fatalf("unknown opportunity: %d %v", r.Status, r.Body)
	}

	in := marketInput("Gula Jabar " + tag)
	in["opportunityId"] = opp
	id := e.createMarket(mm, in)
	if st := e.scalar(`SELECT status || ' ' || price_min_idr || '-' || price_max_idr || ' ' || region FROM markets WHERE id = $1`, id); st != "active 9500-10500 Jawa Barat" {
		t.Fatalf("market: %v", st)
	}
	if n := e.scalar(`SELECT count(*) FROM market_operators WHERE market_id = $1 AND user_id = $2`, id, mmID); n != int64(1) {
		t.Fatalf("operator: %v", n)
	}
	if v := e.scalar(`SELECT version || ':' || effective_from_round || ':' || author FROM market_rule_versions WHERE market_id = $1`, id); v != "1:1:Dimas (Market Maker)" {
		t.Fatalf("rule version: %v", v)
	}
	// autoInvite with auto approval: the opportunity's participant is active.
	if st := e.scalar(`SELECT status FROM market_participants WHERE market_id = $1 AND party_id = $2`, id, party); st != "active" {
		t.Fatalf("participant: %v", st)
	}
	if st := e.scalar(`SELECT o.status || ' ' || p.stage FROM opportunities o JOIN mm_pipeline p ON p.opportunity_id = o.id WHERE o.id = $1`, opp); st != "market_live market_live" {
		t.Fatalf("opportunity: %v", st)
	}
	if st := e.scalar(`SELECT status || ' ' || (market_id = $2) FROM listings WHERE id = $1`, listing, id); st != "in_market true" {
		t.Fatalf("listing: %v", st)
	}
	if !e.notified(contributorID, "Market baru dari Gula "+tag) || !e.notified(followerID, "Market baru dari Gula "+tag) {
		t.Fatal("opportunity relations not notified")
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_type = 'market' AND entity_id = $1 AND action = 'Publish market'`, id); n != int64(1) {
		t.Fatalf("audit: %v", n)
	}
	if f := e.frames("user:" + mmID); len(f) == 0 || f[len(f)-1]["type"] != "mm.activity" {
		t.Fatalf("mm.activity frame: %v", f)
	}

	// Other makers get 404; members of the maker's org operate it too.
	other, otherID := e.maker("Lain")
	if r := e.call(other, "GET", "/mm/markets/"+id, nil); r.Status != 404 {
		t.Fatalf("other maker: %d", r.Status)
	}
	if r := e.call(other, "GET", "/mm/markets/"+id+"/audit", nil); r.Status != 404 {
		t.Fatalf("other maker audit: %d", r.Status)
	}
	orgID, _ := e.org("PT Pasar", mmID)
	orgMarket := e.createMarket(mm, marketInput("Org market "+tag))
	if r := e.call(other, "GET", "/mm/markets/"+orgMarket, nil); r.Status != 404 {
		t.Fatalf("before membership: %d", r.Status)
	}
	e.exec(`INSERT INTO org_members (org_id, user_id, email, name, role, status, joined_at) SELECT $1, id, email, name, 'procurement', 'active', now() FROM users WHERE id = $2`, orgID, otherID)
	if r := e.call(other, "GET", "/mm/markets/"+orgMarket, nil); r.Status != 200 || !strings.HasPrefix(r.Body["market"].(map[string]any)["maker"].(map[string]any)["name"].(string), "PT Pasar") {
		t.Fatalf("org member: %d %v", r.Status, r.Body)
	}

	// Overview: both markets, the creation in the feed.
	r := e.call(mm, "GET", "/mm/overview", nil)
	if r.Status != 200 || len(r.Body["markets"].([]any)) != 2 || r.Body["stats"].(map[string]any)["activeMarkets"] != float64(2) {
		t.Fatalf("overview: %d %v", r.Status, r.Body)
	}
	ev := r.Body["events"].([]any)
	if len(ev) == 0 || ev[0].(map[string]any)["type"] != "market_formed" || !strings.HasPrefix(ev[0].(map[string]any)["title"].(string), "Publish market · ") {
		t.Fatalf("events: %v", ev)
	}
	row := r.Body["markets"].([]any)[0].(map[string]any)
	if alerts := row["alerts"].([]any); len(alerts) == 0 || alerts[0].(map[string]any)["kind"] != "low_liquidity" {
		t.Fatalf("alerts: %v", row)
	}
}

func TestMmRoundsRulesAndStatus(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	tag := fmt.Sprint(time.Now().UnixNano())
	id := e.createMarket(mm, marketInput("Kopi "+tag))
	member, memberID := e.bidder("Peserta")
	_ = member
	e.exec(`INSERT INTO market_participants (market_id, party_id, role, status)
		VALUES ($1, (SELECT id FROM parties WHERE user_id = $2), 'supplier', 'active')`, id, e.partyOf(memberID))

	round := map[string]any{"title": "Lot minggu ini", "quantity": 100, "openingPriceIdr": 10000, "durationMinutes": 60}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds", map[string]any{"title": " ", "quantity": 0, "openingPriceIdr": 0, "durationMinutes": 0}); r.Status != 422 ||
		r.field("title") == "" || r.field("quantity") == "" || r.field("openingPriceIdr") == "" || r.field("durationMinutes") == "" {
		t.Fatalf("round validation: %d %v", r.Status, r.Body)
	}
	r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds", round)
	if r.Status != 201 {
		t.Fatalf("round: %d %v", r.Status, r.Body)
	}
	a1 := r.Body["id"].(string)
	if st := e.scalar(`SELECT a.status || ' ' || a.round_no || ' v' || v.version || ' ' || a.visibility || ' ' || a.current_price_idr || ' ' || a.min_step_idr || ' ' || a.type
		FROM auctions a JOIN market_rule_versions v ON v.id = a.rule_version_id WHERE a.id = $1`, a1); st != "live 1 v1 full 10000 100 reverse" {
		t.Fatalf("round 1: %v", st)
	}
	if !e.notified(memberID, "Round 1 dibuka di Kopi "+tag) {
		t.Fatal("participant not invited")
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'market.round_result' AND aggregate_id = $1 AND payload->>'status' = 'live'`, id); n != int64(1) {
		t.Fatalf("round_result fact: %v", n)
	}

	// Rules: invalid, unchanged, then v2 from round 2.
	rules := marketInput("")["rules"].(map[string]any)
	if r := e.call(mm, "PUT", "/mm/markets/"+id+"/rules", map[string]any{"rules": rules}); r.Status != 422 || r.code() != "no_change" {
		t.Fatalf("no change: %d %v", r.Status, r.Body)
	}
	rules["minQuantity"] = 500
	if r := e.call(mm, "PUT", "/mm/markets/"+id+"/rules", map[string]any{"rules": rules}); r.Status != 422 || r.field("maxQuantity") == "" {
		t.Fatalf("invalid rules: %d %v", r.Status, r.Body)
	}
	rules["minQuantity"], rules["visibility"], rules["eligibility"] = 20, "rank_only", "verified"
	if status, vs := e.callList(mm, "PUT", "/mm/markets/"+id+"/rules", map[string]any{"rules": rules, "reason": "Perketat"}); status != 200 || len(vs) != 2 ||
		vs[1].(map[string]any)["effectiveFromRound"] != float64(2) || vs[1].(map[string]any)["reason"] != "Perketat" {
		t.Fatalf("rules: %d %v", status, vs)
	}
	if ch := e.scalar(`SELECT changes::text FROM audit_log WHERE entity_id = $1 AND action LIKE 'Aturan v2%' AND reason = 'Perketat'`, id).(string); !strings.Contains(ch, "Kuantitas minimum") {
		t.Fatalf("diff: %v", ch)
	}
	// Round 2 starts later: it waits in qualification (eligibility not open) under v2.
	later := map[string]any{"title": "Lot depan", "quantity": 50, "openingPriceIdr": 10000, "durationMinutes": 60,
		"startsAt": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)}
	r = e.call(mm, "POST", "/mm/markets/"+id+"/rounds", later)
	if r.Status != 201 {
		t.Fatalf("round 2: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT a.status || ' ' || a.round_no || ' v' || v.version || ' ' || a.visibility || ' ' || (a.current_price_idr IS NULL)
		FROM auctions a JOIN market_rule_versions v ON v.id = a.rule_version_id WHERE a.id = $1`, r.Body["id"]); st != "qualification 2 v2 rank_only true" {
		t.Fatalf("round 2: %v", st)
	}

	// Ops detail.
	r = e.call(mm, "GET", "/mm/markets/"+id, nil)
	if r.Status != 200 || r.Body["currentRound"] != float64(2) || len(r.Body["ruleVersions"].([]any)) != 2 || len(r.Body["rounds"].([]any)) != 2 ||
		len(r.Body["results"].([]any)) != 1 || r.Body["settings"].(map[string]any)["approval"] != "auto" || len(r.Body["participants"].([]any)) != 1 {
		t.Fatalf("ops: %d %v", r.Status, r.Body)
	}
	if res := r.Body["results"].([]any)[0].(map[string]any); res["status"] != "live" || res["currentIdr"] != float64(10000) || res["round"] != float64(1) {
		t.Fatalf("result: %v", res)
	}
	if v2 := r.Body["ruleVersions"].([]any)[1].(map[string]any); v2["effectiveFromRound"] != float64(2) || v2["rules"].(map[string]any)["windowStart"] != "2026-10-05" {
		t.Fatalf("v2: %v", v2)
	}

	// Status: pause blocks rounds, resume, close is final.
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/status", map[string]any{"action": "pause", "reason": "  "}); r.Status != 422 || r.field("reason") == "" {
		t.Fatalf("blank reason: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/status", map[string]any{"action": "resume", "reason": "x"}); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("resume active: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/status", map[string]any{"action": "pause", "reason": "Audit stok"}); r.Status != 200 || r.Body["status"] != "paused" {
		t.Fatalf("pause: %d %v", r.Status, r.Body)
	}
	if !e.notified(memberID, "Kopi "+tag+": Paused") {
		t.Fatal("pause not notified")
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds", round); r.Status != 409 || r.code() != "market_inactive" {
		t.Fatalf("round while paused: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/status", map[string]any{"action": "resume", "reason": "Stok aman"}); r.Status != 200 || r.Body["status"] != "active" {
		t.Fatalf("resume: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/status", map[string]any{"action": "close", "reason": "Musim selesai"}); r.Status != 200 || r.Body["status"] != "closed" {
		t.Fatalf("close: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "PUT", "/mm/markets/"+id+"/rules", map[string]any{"rules": rules}); r.Status != 409 || r.code() != "market_closed" {
		t.Fatalf("rules on closed: %d %v", r.Status, r.Body)
	}
	// Publish, 2 rounds, rules v2, pause, resume, close; newest first.
	if status, log := e.callList(mm, "GET", "/mm/markets/"+id+"/audit"); status != 200 || len(log) != 7 || log[0].(map[string]any)["action"] != "Tutup market" ||
		log[0].(map[string]any)["reason"] != "Musim selesai" || log[0].(map[string]any)["actor"] != "Dimas (Market Maker)" {
		t.Fatalf("audit: %d %v", status, log)
	}
}

// callList is call for endpoints that answer a JSON array.
func (e *testEnv) callList(c *http.Client, method, path string, body ...any) (int, []any) {
	e.t.Helper()
	var b any
	if len(body) > 0 {
		b = body[0]
	}
	req, _ := http.NewRequest(method, e.srv.URL+BasePath+path, jsonBody(b))
	if b != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out []any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func jsonBody(v any) io.Reader {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// partyOf makes sure the user has a party and returns the user id (for subqueries by user_id).
func (e *testEnv) partyOf(userID string) string {
	e.t.Helper()
	return e.scalar(`INSERT INTO parties (kind, user_id, name, display_kind) SELECT 'user', id, name, 'person' FROM users WHERE id = $1
		ON CONFLICT (user_id) DO UPDATE SET name = EXCLUDED.name RETURNING user_id::text`, userID).(string)
}

func TestMmParticipantsAndDisputes(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	admin, adminID := e.signedIn("Admin")
	_ = admin
	adminID = e.scalar(`SELECT id::text FROM users WHERE email = $1`, adminID).(string)
	e.exec(`INSERT INTO user_capabilities (user_id, capability) VALUES ($1, 'admin')`, adminID)
	tag := fmt.Sprint(time.Now().UnixNano())
	in := marketInput("Karton " + tag)
	in["approval"] = "manual"
	id := e.createMarket(mm, in)
	_, supplierID := e.bidder("Supplier")
	e.partyOf(supplierID)
	pid := e.scalar(`INSERT INTO market_participants (market_id, party_id, role) VALUES ($1, (SELECT id FROM parties WHERE user_id = $2), 'supplier') RETURNING id::text`,
		id, supplierID).(string)
	act := func(action, reason string) resp {
		return e.call(mm, "POST", "/mm/markets/"+id+"/participants/"+pid, map[string]any{"action": action, "reason": reason})
	}
	if r := act("suspend", "x"); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("suspend pending: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "GET", "/mm/overview", nil); !strings.Contains(fmt.Sprint(r.Body["markets"]), "1 menunggu approval") {
		t.Fatalf("approvals alert: %v", r.Body["markets"])
	}
	if r := act("approve", ""); r.Status != 200 || r.Body["status"] != "active" || r.Body["userId"] != supplierID {
		t.Fatalf("approve: %d %v", r.Status, r.Body)
	}
	if !e.notified(supplierID, "Karton "+tag+": approve") {
		t.Fatal("approve not notified")
	}
	if r := act("verify", ""); r.Status != 200 || r.Body["verified"] != true {
		t.Fatalf("verify: %d %v", r.Status, r.Body)
	}
	if r := act("verify", ""); r.Status != 409 {
		t.Fatalf("verify twice: %d", r.Status)
	}
	if r := act("suspend", " "); r.Status != 422 || r.field("reason") == "" {
		t.Fatalf("suspend without reason: %d %v", r.Status, r.Body)
	}
	if r := act("suspend", "Gagal kirim"); r.Status != 200 || r.Body["status"] != "suspended" || r.Body["note"] != "Gagal kirim" {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/participants/00000000-0000-0000-0000-000000000000", map[string]any{"action": "approve"}); r.Status != 404 {
		t.Fatalf("unknown participant: %d", r.Status)
	}

	// Disputes: review, then escalate to an admin case whose status the market dispute follows.
	did := e.scalar(`INSERT INTO market_disputes (market_id, title, parties) VALUES ($1, 'Selisih kuantitas', 'CV Pangan vs PT Kemas') RETURNING id::text`, id).(string)
	dact := func(action, note string) resp {
		return e.call(mm, "POST", "/mm/markets/"+id+"/disputes/"+did, map[string]any{"action": action, "note": note})
	}
	if r := dact("review", ""); r.Status != 200 || r.Body["status"] != "review" {
		t.Fatalf("review: %d %v", r.Status, r.Body)
	}
	if r := dact("review", ""); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("review twice: %d %v", r.Status, r.Body)
	}
	if r := dact("escalate", ""); r.Status != 422 || r.field("note") == "" {
		t.Fatalf("escalate without note: %d %v", r.Status, r.Body)
	}
	r := dact("escalate", "Butuh keputusan admin")
	if r.Status != 200 || r.Body["escalatedTo"] == nil || r.Body["status"] != "open" {
		t.Fatalf("escalate: %d %v", r.Status, r.Body)
	}
	caseID := r.Body["escalatedTo"].(string)
	if st := e.scalar(`SELECT d.reason || ' / ' || t.status || ' / ' || t.title || ' / ' || bp.name || ' vs ' || sp.name || ' / ' || ev.text
		FROM disputes d JOIN trades t ON t.id = d.trade_id JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
		JOIN dispute_evidence ev ON ev.dispute_id = d.id WHERE d.id = $1`, caseID); st != "Selisih kuantitas / disputed / Selisih kuantitas · Karton "+tag+" / CV Pangan vs PT Kemas / Catatan market maker: Butuh keputusan admin" {
		t.Fatalf("admin case: %v", st)
	}
	if !e.notified(adminID, "Eskalasi dispute dari Karton "+tag) {
		t.Fatal("admins not notified")
	}
	if r := dact("resolve", "x"); r.Status != 409 || r.code() != "escalated" {
		t.Fatalf("resolve escalated: %d %v", r.Status, r.Body)
	}
	e.exec(`UPDATE disputes SET status = 'evidence' WHERE id = $1`, caseID)
	r = e.call(mm, "GET", "/mm/markets/"+id, nil)
	if d := r.Body["disputes"].([]any)[0].(map[string]any); d["status"] != "evidence" {
		t.Fatalf("follows admin case: %v", d)
	}
	d2 := e.scalar(`INSERT INTO market_disputes (market_id, title, parties) VALUES ($1, 'Telat kirim', 'A vs B') RETURNING id::text`, id).(string)
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/disputes/"+d2, map[string]any{"action": "resolve"}); r.Status != 422 {
		t.Fatalf("resolve without note: %d", r.Status)
	}
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/disputes/"+d2, map[string]any{"action": "resolve", "note": "Supplier ganti rugi"}); r.Status != 200 ||
		r.Body["status"] != "resolved" || r.Body["resolution"] != "Supplier ganti rugi" {
		t.Fatalf("resolve: %d %v", r.Status, r.Body)
	}
}

func TestMmPipeline(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	tag := fmt.Sprint(time.Now().UnixNano())
	opp := e.scalar(`INSERT INTO opportunities (title, kind, category_id, region, unit, demand_value, supply_value, potential_value_idr,
		suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
		VALUES ($1, 'supply_gap', 'food', 'Bali', 'kg', 10, 5, 1000, 'reverse_auction', 0.5, 'alasan', 'd', 'k') RETURNING id::text`, "Opp "+tag).(string)
	card := func() map[string]any {
		status, list := e.callList(mm, "GET", "/mm/opportunities")
		if status != 200 {
			t.Fatalf("list: %d", status)
		}
		for _, c := range list {
			if c.(map[string]any)["id"] == opp {
				return c.(map[string]any)
			}
		}
		t.Fatal("card missing")
		return nil
	}
	if c := card(); c["stage"] != "detected" || c["mechanismReason"] != "alasan" || c["title"] != "Opp "+tag {
		t.Fatalf("card: %v", c)
	}
	move := func(stage, reason string) resp {
		return e.call(mm, "POST", "/mm/opportunities/"+opp+"/stage", map[string]any{"stage": stage, "reason": reason})
	}
	if r := move("market_live", ""); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("to market_live: %d %v", r.Status, r.Body)
	}
	if r := move("evaluating", ""); r.Status != 204 {
		t.Fatalf("evaluating: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT o.status || ' ' || p.stage FROM opportunities o JOIN mm_pipeline p ON p.opportunity_id = o.id WHERE o.id = $1`, opp); st != "detected evaluating" {
		t.Fatalf("evaluating stored: %v", st)
	}
	if r := move("dismissed", " "); r.Status != 422 || r.field("reason") == "" {
		t.Fatalf("dismiss without reason: %d %v", r.Status, r.Body)
	}
	if r := move("dismissed", "Terlalu kecil"); r.Status != 204 {
		t.Fatalf("dismiss: %d", r.Status)
	}
	if c := card(); c["stage"] != "dismissed" || c["dismissReason"] != "Terlalu kecil" || c["status"] != "dismissed" {
		t.Fatalf("dismissed card: %v", c)
	}
	if r := e.call(mm, "POST", "/mm/opportunities/00000000-0000-0000-0000-000000000000/stage", map[string]any{"stage": "forming"}); r.Status != 404 {
		t.Fatalf("unknown: %d", r.Status)
	}
	if a := e.scalar(`SELECT action || ' / ' || reason FROM audit_log WHERE entity_type = 'opportunity' AND entity_id = $1 ORDER BY id DESC LIMIT 1`, opp); a != "Pipeline: Evaluating → Dismissed / Terlalu kecil" {
		t.Fatalf("audit: %v", a)
	}
}

func TestMmAnalytics(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	other, _ := e.maker("Lain")
	tag := fmt.Sprint(time.Now().UnixNano())
	id := e.createMarket(mm, marketInput("Analitik "+tag))
	e.createMarket(other, marketInput("Bukan punyaku "+tag))
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds", map[string]any{"title": "R1", "quantity": 10, "openingPriceIdr": 1000, "durationMinutes": 30}); r.Status != 201 {
		t.Fatalf("round: %v", r.Body)
	}
	// The queries run against the real ClickHouse schema when it is up.
	if ch, err := analytics.Open("localhost:9000", "ecopurnity", "default", ""); err == nil && ch.Ping(t0()) == nil {
		if _, err := ch.MmEfficiency(t0(), []string{id}); err != nil {
			t.Fatalf("efficiency query: %v", err)
		}
		if _, err := ch.MmGrowth(t0(), []string{id}); err != nil {
			t.Fatalf("growth query: %v", err)
		}
		_ = ch.Close()
	}
	// Analytics reachable or down: 8 weeks, never an error.
	for _, addr := range []string{"localhost:9000", "localhost:1"} {
		ch, err := analytics.Open(addr, "ecopurnity", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		e.server.Analytics = ch
		r := e.call(mm, "GET", "/mm/analytics?market="+id, nil)
		_ = ch.Close()
		if r.Status != 200 || len(r.Body["efficiency"].([]any)) != 8 || len(r.Body["growth"].([]any)) != 8 || len(r.Body["byMarket"].([]any)) != 1 {
			t.Fatalf("analytics %s: %d %v", addr, r.Status, r.Body)
		}
		pd := r.Body["priceDiscovery"].([]any)[0].(map[string]any)
		if rounds := pd["rounds"].([]any); len(rounds) != 1 || pd["unit"] != "kg" {
			t.Fatalf("price discovery: %v", pd)
		}
		if l := r.Body["liquidity"].(map[string]any); l["activeOrders"] != float64(0) {
			t.Fatalf("liquidity: %v", l)
		}
	}
	e.server.Analytics = nil
	if r := e.call(mm, "GET", "/mm/analytics", nil); r.Status != 200 || len(r.Body["byMarket"].([]any)) != 1 {
		t.Fatalf("all: %d %v", r.Status, r.Body)
	}
	otherID := e.scalar(`SELECT id::text FROM markets WHERE name = $1`, "Bukan punyaku "+tag).(string)
	if r := e.call(mm, "GET", "/mm/analytics?market="+otherID, nil); r.Status != 404 {
		t.Fatalf("not operated: %d", r.Status)
	}
}

// closeRound ends a round now and runs the auction clock.
func (e *testEnv) closeRound(auctionID string) {
	e.t.Helper()
	e.exec(`UPDATE auctions SET starts_at = now() - interval '2 hours', ends_at = now() - interval '1 minute' WHERE id = $1`, auctionID)
	if err := e.server.AuctionTick(t0()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) bidOn(auctionID string, price int) string {
	e.t.Helper()
	c, id := e.bidder("Supplier Menang")
	e.qualify(c, auctionID)
	if r := e.call(c, "POST", "/auctions/"+auctionID+"/bids", map[string]any{"priceIdr": price}); r.Status != 200 {
		e.t.Fatalf("bid: %d %v", r.Status, r.Body)
	}
	return id
}

func TestMmCollectiveSettlement(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	tag := fmt.Sprint(time.Now().UnixNano())
	in := marketInput("Beras " + tag)
	in["autoInvite"] = false
	id := e.createMarket(mm, in)

	// Two platform buyers with demand in the market (60 + 40) and an off-platform participant filling the rest.
	var buyers []string
	for _, q := range []int{60, 40} {
		_, uid := e.bidder(fmt.Sprintf("Pembeli %d", q))
		e.partyOf(uid)
		e.exec(`INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, delivery, budget_idr, deadline, market_id)
			VALUES (next_code('DEM'), 'demand', 'in_market', (SELECT id FROM parties WHERE user_id = $1), 'Beras', 'agri', $2, 'kg', 'Garut', 'both', 1000000,
			        now() + interval '30 days', $3)`, uid, q, id)
		buyers = append(buyers, uid)
	}
	ext := e.scalar(`INSERT INTO parties (kind, name) VALUES ('external', 'Koperasi Tani') RETURNING id::text`).(string)
	e.exec(`INSERT INTO market_participants (market_id, party_id, role, status) VALUES ($1, $2, 'buyer', 'active')`, id, ext)

	r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds", map[string]any{"title": "Beras 120 kg", "quantity": 120, "openingPriceIdr": 10000, "durationMinutes": 60})
	aid := r.Body["id"].(string)
	path := "/mm/markets/" + id + "/rounds/" + aid + "/settlement"
	if r := e.call(mm, "POST", path, nil); r.Status != 409 || r.code() != "not_closed" {
		t.Fatalf("live round: %d %v", r.Status, r.Body)
	}
	winner := e.bidOn(aid, 9000)
	e.closeRound(aid)
	if st := e.scalar(`SELECT status FROM bids WHERE auction_id = $1 AND bidder_user_id = $2`, aid, winner); st != "won" {
		t.Fatalf("clock: %v", st)
	}
	if n := e.scalar(`SELECT count(*) FROM outbox WHERE topic = 'market.round_result' AND payload->>'auctionId' = $1 AND payload->>'status' = 'closed'
		AND (payload->>'clearingIdr')::int = 9000`, aid); n != int64(1) {
		t.Fatalf("round_result at close: %v", n)
	}

	// Preview: 60 / 40 / 20 at the winning price; 404 for everyone else.
	other, _ := e.maker("Lain")
	for _, c := range []*http.Client{e.client(), other} {
		if r := e.call(c, "GET", path, nil); r.Status != 404 {
			t.Fatalf("guard: %d", r.Status)
		}
	}
	r = e.call(mm, "GET", path, nil)
	lines := r.Body["lines"].([]any)
	if r.Status != 200 || r.Body["settled"] != nil || r.Body["side"] != "procurement" || r.Body["priceIdr"] != float64(9000) ||
		r.Body["winnerUserId"] != winner || len(lines) != 3 {
		t.Fatalf("preview: %d %v", r.Status, r.Body)
	}
	got := ""
	for _, l := range lines {
		l := l.(map[string]any)
		got += fmt.Sprintf("%v:%v:%v ", l["member"], l["quantity"], l["amountIdr"])
	}
	if got != "Pembeli 60:60:540000 Pembeli 40:40:360000 Koperasi Tani:20:180000 " {
		t.Fatalf("lines: %s", got)
	}

	r = e.call(mm, "POST", path, nil)
	if r.Status != 200 || r.Body["settled"].(map[string]any)["trades"] != float64(3) || r.Body["settled"].(map[string]any)["by"] != "Dimas" {
		t.Fatalf("settle: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT count(*) || ' ' || min(t.maker_fee_rate) || ' ' || min(t.group_label) || ' ' || sum(t.group_share) || ' ' || bool_and(t.supplier_party_id = s.winner_party_id)
		FROM trades t JOIN settlement_lines sl ON sl.trade_id = t.id JOIN settlements s ON s.id = sl.settlement_id WHERE s.auction_id = $1`, aid); st != fmt.Sprintf("3 0.00500 Kolektif %s 1.00000 true", e.scalar(`SELECT code FROM auctions WHERE id = $1`, aid)) {
		t.Fatalf("trades: %v", st)
	}
	if !e.notified(buyers[0], "Bagianmu dari Beras 120 kg") || e.scalar(`SELECT status FROM auctions WHERE id = $1`, aid) != "awarded" {
		t.Fatal("member notification / awarded")
	}
	if r := e.call(mm, "POST", path, nil); r.Status != 409 || r.code() != "already_settled" {
		t.Fatalf("settle twice: %d %v", r.Status, r.Body)
	}
	if r := e.call(mm, "GET", path, nil); r.Body["settled"] == nil {
		t.Fatalf("settled preview: %v", r.Body)
	}

	// A round nobody bid on cannot be settled.
	r = e.call(mm, "POST", "/mm/markets/"+id+"/rounds", map[string]any{"title": "Kosong", "quantity": 10, "openingPriceIdr": 10000, "durationMinutes": 60})
	empty := r.Body["id"].(string)
	e.closeRound(empty)
	if r := e.call(mm, "POST", "/mm/markets/"+id+"/rounds/"+empty+"/settlement", nil); r.Status != 409 || r.code() != "no_bids" {
		t.Fatalf("no bids: %d %v", r.Status, r.Body)
	}
}

func TestMmPoolMarketAndSettlement(t *testing.T) {
	e := newEnv(t)
	mm, _ := e.maker("Dimas")
	_, uA := e.bidder("Ani")
	_, uA2 := e.bidder("Adi")
	_, uB := e.bidder("Budi")
	orgA, _ := e.org("PT Ani", uA, uA2)
	orgB, _ := e.org("CV Budi", uB)
	tag := fmt.Sprint(time.Now().UnixNano())
	title := "Gula pasir " + tag
	pool := e.scalar(`INSERT INTO collective_pools (title, category_id, spec, region, deadline, unit, base_unit_price_idr, ref_qty, threshold_qty, status, market_requested_at, created_by)
		VALUES ($1, 'food', 'Gula kristal putih', 'Bandung', now() + interval '14 days', 'kg', 5000, 10, 80, 'market_requested', now(), $2) RETURNING id::text`, title, uA).(string)
	e.exec(`INSERT INTO pool_members (pool_id, org_id, quantity, opt_in, created_at) VALUES ($1, $2, 60, true, now() - interval '3 minutes'), ($1, $3, 30, false, now() - interval '2 minutes')`, pool, orgA, orgB)
	e.exec(`INSERT INTO pool_members (pool_id, name, quantity, opt_in) VALUES ($1, 'Warung Bu Sri', 10, false)`, pool)
	prq := e.scalar(`INSERT INTO procurement_requests (org_id, need, category_id, quantity, unit, budget_idr, deadline, delivery_location, visibility, status, pool_id, created_by)
		VALUES ($1, 'Gula', 'food', 60, 'kg', 300000, now() + interval '14 days', 'Gudang A, Bandung', 'aggregate', 'in_collective', $2, $3) RETURNING id::text`, orgA, pool, uA).(string)

	findPool := func() map[string]any {
		status, list := e.callList(mm, "GET", "/mm/pools")
		if status != 200 {
			t.Fatalf("pools: %d", status)
		}
		for _, p := range list {
			if p.(map[string]any)["id"] == pool {
				return p.(map[string]any)
			}
		}
		t.Fatal("pool missing")
		return nil
	}
	p := findPool()
	names := ""
	for _, m := range p["members"].([]any) {
		names += m.(map[string]any)["name"].(string) + "|"
	}
	if !strings.HasPrefix(names, "PT Ani") || !strings.HasSuffix(names, "|Bisnis lain #2|Bisnis lain #3|") || p["status"] != "market_requested" {
		t.Fatalf("pool: %v", p)
	}

	if r := e.call(mm, "POST", "/mm/pools/"+pool+"/market", map[string]any{"durationMinutes": 30}); r.Status != 201 {
		t.Fatalf("form: %d %v", r.Status, r.Body)
	} else {
		p = r.Body
	}
	id, aid := p["marketId"].(string), p["auctionId"].(string)
	if r := e.call(mm, "POST", "/mm/pools/"+pool+"/market", map[string]any{"durationMinutes": 30}); r.Status != 409 || r.message() != "Market untuk pool ini sudah dibentuk" {
		t.Fatalf("form twice: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT m.mechanism || ' ' || a.type || ' ' || a.status || ' ' || a.quantity || ' ' || a.lot_spec || ' ' || cp.status
		FROM collective_pools cp JOIN markets m ON m.id = cp.market_id JOIN auctions a ON a.id = cp.auction_id WHERE cp.id = $1`, pool); st != "collective_procurement reverse live 100.000 Gula kristal putih market_live" {
		t.Fatalf("formed: %v", st)
	}
	for _, u := range []string{uA, uA2, uB} {
		if !e.notified(u, "Market terbentuk: "+title) {
			t.Fatalf("org user %s not notified", u)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE org_id = $1 AND action = 'Market terbentuk dari pool collective'`, orgA); n != int64(1) {
		t.Fatalf("org audit: %v", n)
	}
	r := e.call(mm, "GET", "/mm/markets/"+id, nil)
	ps := r.Body["participants"].([]any)
	if pn := fmt.Sprint(ps); len(ps) != 2 || !strings.Contains(pn, "name:Bisnis lain #2") || !strings.Contains(pn, "name:PT Ani") || strings.Contains(pn, "CV Budi") ||
		r.Body["market"].(map[string]any)["buyers"] != float64(2) {
		t.Fatalf("participants: %v", ps)
	}

	e.bidOn(aid, 4500)
	e.closeRound(aid)
	path := "/mm/markets/" + id + "/rounds/" + aid + "/settlement"
	r = e.call(mm, "GET", path, nil)
	lines := r.Body["lines"].([]any)
	if len(lines) != 3 || lines[1].(map[string]any)["member"] != "Bisnis lain #2" || lines[1].(map[string]any)["orgId"] != orgB ||
		lines[2].(map[string]any)["orgId"] != nil || lines[2].(map[string]any)["quantity"] != float64(10) {
		t.Fatalf("pool preview: %v", r.Body)
	}
	r = e.call(mm, "POST", path, nil)
	if r.Status != 200 || r.Body["settled"].(map[string]any)["trades"] != float64(2) {
		t.Fatalf("settle pool: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT string_agg(o.name || '=' || t.quantity || '@' || t.delivery_address || '/' || t.group_label, ', ' ORDER BY t.quantity DESC)
		FROM trades t JOIN parties p ON p.id = t.buyer_party_id JOIN orgs o ON o.id = p.org_id WHERE t.auction_id = $1`, aid).(string); !strings.Contains(st, "=60.000@Gudang A, Bandung/Pool kolektif AUC-") ||
		!strings.Contains(st, "=30.000@Bandung, Jawa Barat/Pool kolektif") {
		t.Fatalf("sub-POs: %v", st)
	}
	if st := e.scalar(`SELECT status FROM procurement_requests WHERE id = $1`, prq); st != "po_issued" {
		t.Fatalf("procurement: %v", st)
	}
	if n := e.scalar(`SELECT count(*) FROM pool_members WHERE pool_id = $1 AND settlement_line_id IS NOT NULL`, pool); n != int64(2) {
		t.Fatalf("member lines: %v", n)
	}
	if !e.notified(uA2, "Sub-PO pool: "+title) || !e.notified(uB, "Sub-PO pool: "+title) {
		t.Fatal("sub-PO notifications")
	}
	p = findPool()
	if p["status"] != "settled" || p["round"].(map[string]any)["status"] != "awarded" || len(p["settlement"].(map[string]any)["lines"].([]any)) != 2 ||
		p["settlement"].(map[string]any)["priceIdr"] != float64(4500) {
		t.Fatalf("settled pool: %v", p)
	}
}
