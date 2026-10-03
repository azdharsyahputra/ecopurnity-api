package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func supplyBody(item string, price int, qty float64, unit string) map[string]any {
	return map[string]any{"kind": "supply", "item": item, "categoryId": "agri", "quantity": map[string]any{"value": qty, "unit": unit},
		"location": "Garut, Jawa Barat", "spec": "Grade 1", "delivery": "both", "attachments": []string{"foto.jpg"},
		"priceIdr": price, "availableFrom": time.Now().UTC().Format(time.RFC3339)}
}

func demandBody(item string, budget int) map[string]any {
	return map[string]any{"kind": "demand", "item": item, "categoryId": "agri", "quantity": map[string]any{"value": 100, "unit": "kg"},
		"location": "Bandung, Jawa Barat", "spec": "", "delivery": "deliver", "attachments": []string{},
		"budgetIdr": budget, "deadline": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
}

// seedMarket inserts a market (and its maker party) directly; the market-maker endpoints that create them come later.
func (e *testEnv) seedMarket(name, category, unit, status, approval string) string {
	e.t.Helper()
	var party, id string
	if err := e.db.Primary().QueryRow(t0(), `INSERT INTO parties (kind, name) VALUES ('external', $1) RETURNING id`, "Koperasi "+name).Scan(&party); err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.Primary().QueryRow(t0(), `
		INSERT INTO markets (code, name, category_id, region, objective, mechanism, status, maker_party_id, unit, demand_value, supply_value, price_min_idr, price_max_idr)
		VALUES (next_code('MKT'), $1, $2, 'Jawa Barat', 'selling', 'forward_auction', $3, $4, $5, 1000, 800, 80000, 96000) RETURNING id`,
		name, category, status, party, unit).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO market_settings (market_id, approval, supplier_verification) VALUES ($1, $2, 'none')`, id, approval)
	return id
}

func TestMyListingsLifecycle(t *testing.T) {
	e := newEnv(t)
	c, _ := e.signedIn("Lina")

	// Create and validate.
	r := e.call(c, "POST", "/me/listings", supplyBody("Biji kopi arabika", 82000, 500, "kg"))
	if r.Status != 201 || r.Body["status"] != "available" || !strings.HasPrefix(r.Body["code"].(string), "SUP-") || r.Body["priceIdr"] != float64(82000) {
		t.Fatalf("create supply: %d %v", r.Status, r.Body)
	}
	supplyID := r.Body["id"].(string)
	if att := r.Body["attachments"].([]any); len(att) != 1 || att[0] != "foto.jpg" {
		t.Fatalf("attachments: %v", r.Body["attachments"])
	}
	r = e.call(c, "POST", "/me/listings", demandBody("Karung goni 60 kg", 900000))
	if r.Status != 201 || r.Body["status"] != "open" || !strings.HasPrefix(r.Body["code"].(string), "DEM-") {
		t.Fatalf("create demand: %d %v", r.Status, r.Body)
	}
	demandID := r.Body["id"].(string)
	bad := supplyBody("  ", 1000, 0, "kg")
	bad["location"] = ""
	if r := e.call(c, "POST", "/me/listings", bad); r.Status != 422 || r.field("item") == "" || r.field("quantity") == "" || r.field("location") == "" {
		t.Fatalf("validation: %d %v", r.Status, r.Body)
	}
	past := demandBody("Pupuk", 100)
	past["deadline"] = time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if r := e.call(c, "POST", "/me/listings", past); r.Status != 422 || r.field("deadline") == "" {
		t.Fatalf("past deadline: %d %v", r.Status, r.Body)
	}

	// Lists and ownership.
	if r := e.call(c, "GET", "/me/listings?kind=supply", nil); r.Status != 200 {
		t.Fatalf("list: %d", r.Status)
	}
	other, _ := e.signedIn("Orang Lain")
	if r := e.call(other, "GET", "/me/listings/"+supplyID, nil); r.Status != 404 {
		t.Fatalf("other user's listing: %d", r.Status)
	}

	// Detail: history, related markets and opportunities by category.
	market := e.seedMarket("Kopi Garut", "agri", "kg", "active", "auto")
	e.seedMarket("Pupuk Jateng", "agri", "kg", "closed", "manual")
	e.exec(`INSERT INTO opportunities (code, title, kind, category_id, region, status, unit, demand_value, supply_value, potential_value_idr, suggested_mechanism, confidence, mechanism_reason, description, required_contribution)
	        VALUES (next_code('OPP'), 'Pengadaan kolektif kopi', 'collective_demand', 'agri', 'Jawa Barat', 'detected', 'kg', 1000, 600, 5000000, 'reverse_auction', 0.8, 'x', 'x', 'x')`)
	r = e.call(c, "GET", "/me/listings/"+supplyID, nil)
	if r.Status != 200 || r.Body["item"] != "Biji kopi arabika" {
		t.Fatalf("detail: %d %v", r.Status, r.Body)
	}
	hist, _ := r.Body["history"].([]any)
	markets, _ := r.Body["markets"].([]any)
	matches, _ := r.Body["matches"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["note"] != "Dibuat" || len(markets) != 1 || len(matches) != 1 {
		t.Fatalf("detail parts: history %v markets %d matches %d", hist, len(markets), len(matches))
	}

	// Patch: editable fields only, per kind.
	r = e.call(c, "PATCH", "/me/listings/"+supplyID, map[string]any{"kind": "supply", "item": "Biji kopi arabika grade 1", "priceIdr": 85000,
		"quantity": map[string]any{"value": 450, "unit": "kg"}, "availableFrom": time.Now().Format("2006-01-02")})
	if r.Status != 200 || r.Body["item"] != "Biji kopi arabika grade 1" || r.Body["priceIdr"] != float64(85000) || r.Body["quantity"].(map[string]any)["value"] != float64(450) {
		t.Fatalf("patch: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "PATCH", "/me/listings/"+supplyID, map[string]any{"budgetIdr": 10}); r.Status != 422 || r.field("budgetIdr") == "" {
		t.Fatalf("wrong-kind field: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "PATCH", "/me/listings/"+supplyID, map[string]any{"kind": "demand"}); r.Status != 422 || r.field("kind") == "" {
		t.Fatalf("kind change: %d %v", r.Status, r.Body)
	}

	// Into a market: closed and wrong-category markets are refused; auto-approval makes the owner an active participant.
	closed := e.seedMarket("Tutup", "agri", "kg", "closed", "auto")
	if r := e.call(c, "POST", "/me/listings/"+supplyID+"/market", map[string]any{"marketId": closed}); r.Status != 409 || r.code() != "market_closed" {
		t.Fatalf("closed market: %d %v", r.Status, r.Body)
	}
	pack := e.seedMarket("Kemasan", "packaging", "pcs", "active", "auto")
	if r := e.call(c, "POST", "/me/listings/"+supplyID+"/market", map[string]any{"marketId": pack}); r.Status != 422 || r.field("marketId") == "" {
		t.Fatalf("category: %d %v", r.Status, r.Body)
	}
	r = e.call(c, "POST", "/me/listings/"+supplyID+"/market", map[string]any{"marketId": market})
	if r.Status != 200 || r.Body["status"] != "in_market" || r.Body["marketId"] != market {
		t.Fatalf("submit: %d %v", r.Status, r.Body)
	}
	if st := e.scalar(`SELECT mp.role || '/' || mp.status FROM market_participants mp JOIN parties p ON p.id = mp.party_id JOIN users u ON u.id = p.user_id
	                    WHERE mp.market_id = $1 AND u.name = 'Lina'`, market); st != "supplier/active" {
		t.Fatalf("participant: %v", st)
	}

	// Archive: supply expired, demand cancelled; finished listings are read-only.
	if r := e.call(c, "POST", "/me/listings/"+demandID+"/archive", nil); r.Status != 200 || r.Body["status"] != "cancelled" {
		t.Fatalf("archive demand: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "PATCH", "/me/listings/"+demandID, map[string]any{"item": "x"}); r.Status != 409 || r.code() != "not_editable" {
		t.Fatalf("edit finished: %d %v", r.Status, r.Body)
	}

	// Restricted accounts cannot create.
	e.exec(`UPDATE users SET status = 'restricted' WHERE name = 'Lina'`)
	if r := e.call(c, "POST", "/me/listings", supplyBody("Teh", 1000, 1, "kg")); r.Status != 403 || r.code() != "account_restricted" {
		t.Fatalf("restricted: %d %v", r.Status, r.Body)
	}
}

func TestCatalogAndPriceSuggestion(t *testing.T) {
	e := newEnv(t)
	tag := fmt.Sprint(time.Now().UnixNano()) // isolates this test's rows in the shared test database
	seller, _ := e.signedIn("Penjual " + tag)
	hidden, _ := e.signedIn("Dibatasi " + tag)
	unit := "Kg" + tag[len(tag)-6:] // a unit no other test uses keeps the price samples to this test's listings
	for i, price := range []int{80000, 82000, 84000, 86000} {
		e.call(seller, "POST", "/me/listings", supplyBody(fmt.Sprintf("Kopi arabika %s #%d", tag, i), price, 100, unit))
	}
	e.call(seller, "POST", "/me/listings", supplyBody("Pupuk organik "+tag, 2300, 100, unit))
	gone := e.call(seller, "POST", "/me/listings", supplyBody("Kopi lama "+tag, 99999, 100, unit)).Body["id"].(string)
	e.call(seller, "POST", "/me/listings/"+gone+"/archive", nil)
	e.call(hidden, "POST", "/me/listings", supplyBody("Kopi rahasia "+tag, 1, 100, unit))
	e.exec(`UPDATE users SET status = 'restricted' WHERE name = $1`, "Dibatasi "+tag)

	anon := e.client()
	r := e.call(anon, "GET", "/listings?q="+tag+"&pageSize=3", nil)
	meta := r.Body["meta"].(map[string]any)
	if r.Status != 200 || meta["total"] != float64(5) || len(r.Body["data"].([]any)) != 3 || meta["pageSize"] != float64(3) {
		t.Fatalf("catalog: %d total %v data %d", r.Status, meta["total"], len(r.Body["data"].([]any)))
	}
	first := r.Body["data"].([]any)[0].(map[string]any)
	owner := first["owner"].(map[string]any)
	if owner["name"] != "Penjual "+tag || owner["username"] == nil || first["unitPriceIdr"] == nil {
		t.Fatalf("item: %v", first)
	}
	if r := e.call(anon, "GET", "/listings?q="+tag+"&page=2&pageSize=3", nil); len(r.Body["data"].([]any)) != 2 {
		t.Fatalf("page 2: %v", r.Body)
	}
	if r := e.call(anon, "GET", "/listings?q="+tag+"&page=9", nil); r.Body["meta"].(map[string]any)["total"] != float64(5) || len(r.Body["data"].([]any)) != 0 {
		t.Fatalf("out of range page: %v", r.Body)
	}
	if r := e.call(anon, "GET", "/listings?q="+tag+"&kind=demand", nil); r.Body["meta"].(map[string]any)["total"] != float64(0) {
		t.Fatalf("kind filter: %v", r.Body["meta"])
	}
	if r := e.call(anon, "GET", "/listings?q=Garut&region=jawa%20barat&category=agri", nil); r.Status != 200 {
		t.Fatalf("region filter: %d", r.Status)
	}
	// LIKE wildcards in q are literal.
	if r := e.call(anon, "GET", "/listings?q=%25%25"+tag, nil); r.Body["meta"].(map[string]any)["total"] != float64(0) {
		t.Fatalf("wildcards: %v", r.Body["meta"])
	}

	// Price suggestion: narrowed to "kopi arabika" listings (units compared case-insensitively), not the pupuk one.
	r = e.call(anon, "GET", "/listings/price-suggestion?category=agri&unit="+strings.ToUpper(unit)+"&item=Kopi%20arabika", nil)
	if r.Status != 200 || r.Body["sample"] != float64(4) || r.Body["medianIdr"] != float64(83000) || r.Body["unit"] != strings.ToLower(unit) {
		t.Fatalf("suggestion: %d %v", r.Status, r.Body)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+BasePath+"/listings/price-suggestion?category=energy&unit=unit-"+tag, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var b [16]byte
	n, _ := res.Body.Read(b[:])
	res.Body.Close()
	if res.StatusCode != 200 || strings.TrimSpace(string(b[:n])) != "null" {
		t.Fatalf("no data: %d %q", res.StatusCode, b[:n])
	}
}
