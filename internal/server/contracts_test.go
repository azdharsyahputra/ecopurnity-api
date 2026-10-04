package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestContractsWithExternalSupplier(t *testing.T) {
	e := newEnv(t)
	e.server.SimulateCounterparties = true
	buyer, buyerID := e.bidder("Citra Kontrak")
	ext := e.externalParty("UD Beras Makmur")
	src := e.newTestTrade(newTrade{Title: "Beras premium · 200 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: ext, Quantity: 200, UnitPriceIdr: 12_000, Terms: "net30"})
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format(time.DateOnly)
	body := map[string]any{"fromTx": src, "every": "weekly", "runs": 2, "startAt": tomorrow}

	if r := e.call(buyer, "POST", "/me/contracts", body); r.Status != 409 || r.code() != "not_completed" {
		t.Fatal("not completed", r.Status, r.Body)
	}
	e.exec(`UPDATE trades SET status = 'completed' WHERE id = $1`, src)
	if r := e.call(buyer, "POST", "/me/contracts", map[string]any{"fromTx": src, "every": "weekly", "runs": 2, "startAt": "2020-01-01"}); r.Status != 422 || r.field("startAt") == "" {
		t.Fatal("past start", r.Status, r.Body)
	}
	if r := e.call(buyer, "POST", "/me/contracts", map[string]any{"fromTx": "x", "every": "weekly", "runs": 2, "startAt": tomorrow}); r.Status != 404 {
		t.Fatal("unknown trade", r.Status)
	}
	r := e.call(buyer, "POST", "/me/contracts", body)
	if r.Status != 201 || r.Body["status"] != "proposed" || r.Body["item"] != "Beras premium" || r.Body["side"] != "buyer" || r.Body["terms"] != "net30" ||
		!strings.HasPrefix(r.Body["code"].(string), "CTR-") || r.Body["sourceTxId"] != src {
		t.Fatal("create", r.Status, r.Body)
	}
	if acts := r.Body["actions"].([]any); len(acts) != 1 || acts[0] != "end" {
		t.Fatal("proposer can only end", acts)
	}
	id := r.Body["id"].(string)
	if r := e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "accept"}); r.Status != 409 {
		t.Fatal("proposer accepts", r.Status)
	}

	if err := e.server.ContractTick(t0(), 0); err != nil {
		t.Fatal(err)
	}
	r = e.call(buyer, "GET", "/me/contracts/"+id, nil)
	if r.Body["status"] != "active" || !slices.Contains(r.Body["actions"].([]any), any("run_now")) {
		t.Fatal("accepted by the bot", r.Body)
	}
	if len(r.Body["orders"].([]any)) != 0 {
		t.Fatal("not due yet", r.Body["orders"])
	}

	r = e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "run_now"})
	if r.Status != 200 || r.Body["status"] != "active" || len(r.Body["orders"].([]any)) != 1 {
		t.Fatal("run_now", r.Status, r.Body)
	}
	trade := r.Body["orders"].([]any)[0].(map[string]any)["buyerTxId"].(string)
	d := e.call(buyer, "GET", "/me/transactions/"+trade, nil)
	if d.Status != 200 || !strings.HasSuffix(d.Body["title"].(string), "· order 1/2 ("+r.Body["code"].(string)+")") || d.Body["terms"] != "net30" {
		t.Fatal("order trade", d.Status, d.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE 'Order kontrak %'`, buyerID).(int64); n != 1 {
		t.Fatal("order notification", n)
	}

	e.exec(`UPDATE supply_contracts SET next_at = now() - interval '1 minute' WHERE id = $1`, id)
	if err := e.server.ContractTick(t0(), 0); err != nil {
		t.Fatal(err)
	}
	r = e.call(buyer, "GET", "/me/contracts/"+id, nil)
	if r.Body["status"] != "ended" || len(r.Body["orders"].([]any)) != 2 || len(r.Body["actions"].([]any)) != 0 {
		t.Fatal("ended after the last run", r.Body)
	}
	if r := e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "pause"}); r.Status != 409 {
		t.Fatal("ended is final", r.Status)
	}
}

func TestContractsBetweenUsers(t *testing.T) {
	e := newEnv(t)
	buyer, buyerID := e.bidder("Fani Pembeli")
	supplier, supplierID := e.bidder("Gita Supplier")
	src := e.newTestTrade(newTrade{Title: "Telur · 50 kg", BuyerParty: e.partyOf(buyerID), SupplierParty: e.partyOf(supplierID), Quantity: 50, UnitPriceIdr: 28_000})
	e.exec(`UPDATE trades SET status = 'completed' WHERE id = $1`, src)
	start := time.Now().UTC().Add(48 * time.Hour).Format(time.DateOnly)
	r := e.call(supplier, "POST", "/me/contracts", map[string]any{"fromTx": src, "every": "monthly", "runs": 6, "startAt": start})
	if r.Status != 201 || r.Body["proposedBy"] != "supplier" {
		t.Fatal(r.Status, r.Body)
	}
	id := r.Body["id"].(string)
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title = 'Gita Supplier mengusulkan kontrak rutin'`, buyerID).(int64); n != 1 {
		t.Fatal("proposal notification", n)
	}
	if next, _ := time.Parse(time.RFC3339, r.Body["nextAt"].(string)); next.In(wib).Hour() != 8 || next.In(wib).Format(time.DateOnly) != start {
		t.Fatal("first run 08:00 WIB", r.Body["nextAt"])
	}
	outsider, _ := e.bidder("Hadi Luar")
	if r := e.call(outsider, "GET", "/me/contracts/"+id, nil); r.Status != 404 {
		t.Fatal("outsider", r.Status)
	}
	if r := e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "run_now"}); r.Status != 409 {
		t.Fatal("run_now on a proposal", r.Status)
	}
	r = e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "accept"})
	if r.Status != 200 || r.Body["status"] != "active" || r.Body["side"] != "buyer" {
		t.Fatal("accept", r.Status, r.Body)
	}
	if r := e.call(supplier, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "run_now"}); r.Status != 409 {
		t.Fatal("only the buyer runs now", r.Status)
	}
	if r := e.call(supplier, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "pause"}); r.Status != 200 || r.Body["status"] != "paused" {
		t.Fatal("pause", r.Status, r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title LIKE 'Kontrak % dijeda'`, buyerID).(int64); n != 1 {
		t.Fatal("pause notification", n)
	}
	e.exec(`UPDATE supply_contracts SET next_at = now() - interval '3 days' WHERE id = $1`, id)
	r = e.call(buyer, "POST", "/me/contracts/"+id+"/actions", map[string]any{"action": "resume"})
	if next, _ := time.Parse(time.RFC3339, r.Body["nextAt"].(string)); r.Status != 200 || time.Since(next) > time.Minute {
		t.Fatal("resume moves an overdue run to now", r.Status, r.Body)
	}
	lst := e.callList(supplier, "/me/contracts")
	if !slices.ContainsFunc(lst, func(x map[string]any) bool { return x["id"] == id && x["side"] == "supplier" }) {
		t.Fatal("list", lst)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1`, id).(int64); n != 3 {
		t.Fatal("audit", n)
	}
}
