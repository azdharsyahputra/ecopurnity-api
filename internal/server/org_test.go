package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

var testRules = []approvalRule{
	{Label: "> 50 jt", MinAmount: 50_000_000, Approvers: []string{"finance", "owner"}, AppliesTo: []string{"procurement", "auction"}},
	{Label: "Auction > 200 jt", MinAmount: 200_000_000, Approvers: []string{"owner", "procurement"}, AppliesTo: []string{"auction"}},
}

func perms(role string) []string {
	for _, r := range builtInRoles {
		if r.Key == role {
			return r.Permissions
		}
	}
	return nil
}

func TestOrgPermissions(t *testing.T) {
	if !can(perms("finance"), "finance", "procurement", "approve") || can(perms("sales"), "sales", "procurement", "create") {
		t.Fatal("default matrix")
	}
	if !can(nil, "owner", "team", "manage") {
		t.Fatal("owner always allowed")
	}
	qc := []string{"inventory.view"}
	if !can(qc, "custom-qc", "inventory", "view") || can(qc, "custom-qc", "inventory", "manage") {
		t.Fatal("custom role")
	}
	if got := deniedReason("Sales", "procurement", "create"); got != "Peran Sales tidak punya izin buat Procurement" {
		t.Fatal(got)
	}
}

func TestOrgTransactionPermissions(t *testing.T) {
	for _, c := range []struct {
		role, action string
		ok           bool
	}{{"finance", "pay", true}, {"procurement", "pay", false}, {"sales", "issue_invoice", true}, {"operations", "ship", true},
		{"finance", "upload_proof", false}, {"procurement", "confirm_receipt", true}, {"operations", "cancel", false}, {"owner", "dispute", true},
		{"sales", "accept_agreement", true}, {"finance", "review", false}} {
		if canTransact(perms(c.role), c.role, c.action) != c.ok {
			t.Errorf("%s %s", c.role, c.action)
		}
	}
	if !canTransact([]string{"transactions.view", "transactions.manage"}, "custom-ap", "pay") || canTransact([]string{"transactions.view"}, "custom-qc", "pay") {
		t.Fatal("custom roles fall back to transactions.manage")
	}
	if txDeniedReason(perms("finance"), "finance", "Finance", "pay") != "" {
		t.Fatal("finance may pay")
	}
	if got := txDeniedReason(perms("sales"), "sales", "Sales", "pay"); got != "Bayar hanya untuk Owner, Finance; peranmu Sales" {
		t.Fatal(got)
	}
	if got := txDeniedReason(nil, "custom-qc", "QC", "pay"); got != "Peran QC tidak punya izin kelola Transactions" {
		t.Fatal(got)
	}
}

func TestOrgApprovalRules(t *testing.T) {
	eq := func(got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	eq(requiredApprovers(50_000_000, "procurement", testRules), []string{})
	eq(requiredApprovers(60_000_000, "procurement", testRules), []string{"finance", "owner"})
	eq(requiredApprovers(300_000_000, "procurement", testRules), []string{"finance", "owner"})
	eq(requiredApprovers(300_000_000, "auction", testRules), []string{"finance", "owner", "procurement"})

	req := []string{"finance", "owner"}
	ok := func(role string) approval { return approval{role, "approved"} }
	if p, rej, appr := approvalState(req, []approval{ok("finance")}); !slices.Equal(p, []string{"owner"}) || rej || appr {
		t.Fatal("state")
	}
	if statusAfterApproval(req, []approval{ok("finance"), ok("owner")}) != "approved" ||
		statusAfterApproval(req, []approval{{"finance", "rejected"}}) != "rejected" || statusAfterApproval(nil, nil) != "approved" {
		t.Fatal("statusAfterApproval")
	}
	staffed := []string{"finance", "owner", "procurement"}
	if !canApprove("owner", req, []approval{ok("finance")}, staffed) || canApprove("finance", req, []approval{ok("finance")}, staffed) {
		t.Fatal("canApprove")
	}

	eq(signingRoles("owner", req, nil, []string{"owner", "sales"}), []string{"owner", "finance"})
	eq(signingRoles("owner", req, nil, staffed), []string{"owner"})
	eq(signingRoles("owner", req, nil, nil), []string{"owner"})
	eq(signingRoles("owner", []string{"finance"}, nil, []string{"owner"}), []string{"finance"})
	eq(signingRoles("sales", req, nil, []string{"owner", "sales"}), []string{})
	eq(signingRoles("owner", req, []approval{{"owner", "rejected"}}, []string{"owner"}), []string{})
	eq(approverUserIDs([]string{"finance"}, nil, []orgMemberRole{{"ajar", "owner"}, {"fajar", "sales"}}, ""), []string{"ajar"})

	members := []orgMemberRole{{"ajar", "owner"}, {"maya", "finance"}, {"bima", "procurement"}}
	eq(approverUserIDs(req, nil, members, "bima"), []string{"ajar", "maya"})
	eq(approverUserIDs(req, nil, members, "ajar"), []string{"maya"})
	eq(approverUserIDs(req, []approval{ok("finance")}, members, ""), []string{"ajar"})
	eq(approverUserIDs(req, []approval{{"finance", "rejected"}}, members, ""), []string{})

	pending := []approval{ok("finance")}
	eq(procurementActions("pending_approval", req, pending, "owner", nil, staffed), []string{"approve", "reject", "cancel"})
	eq(procurementActions("pending_approval", req, pending, "sales", perms("sales"), staffed), []string{})
	eq(procurementActions("draft", req, pending, "procurement", perms("procurement"), staffed), []string{"submit", "cancel"})

	got := pipelineCounts([]string{"draft", "pending_approval", "approved", "published", "in_auction", "po_issued", "rejected"})
	want := map[string]int{"draft": 1, "approval": 1, "published": 2, "auction": 1, "awarded": 0, "po": 1}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("pipeline %v", got)
		}
	}
}

func TestOrgAuctionStatusAndInitials(t *testing.T) {
	for _, c := range []struct {
		stored  string
		awarded bool
		lots    []string
		want    string
	}{{"pending_approval", false, nil, "pending_approval"}, {"live", true, []string{"closed"}, "awarded"}, {"live", false, []string{"closed", "extended"}, "live"},
		{"live", false, []string{"scheduled", "qualification"}, "scheduled"}, {"live", false, []string{"closed", "scheduled"}, "closed"}} {
		if got := orgAuctionStatus(c.stored, c.awarded, c.lots); got != c.want {
			t.Errorf("%v: %s", c, got)
		}
	}
	if poInitials("PT Solusi Kemasan Nusantara") != "PSKN" || poInitials("koperasi kopi") != "ORG" {
		t.Fatal("initials")
	}
}

func TestOrgAnalyticsCompute(t *testing.T) {
	p := func(month, cat, item string, qty float64, spend, budget, market int64, sup string) purchase {
		return purchase{Month: month, Category: cat, Item: item, Unit: "kg", Supplier: sup, Via: "direct", Qty: qty, Spend: spend, Budget: budget, Market: market, Count: 1}
	}
	aggs := []purchase{
		p("2026-07", "packaging", "Kraft", 10, 1000, 1100, 1050, "s:a"),
		p("2026-08", "packaging", "Kraft", 10, 900, 1100, 1100, "s:a"),
		p("2026-08", "agri", "Pupuk", 5, 500, 500, 500, "s:b"),
		p("2026-09", "packaging", "Kraft", 20, 1600, 2200, 2000, "s:b"),
	}
	sups := map[string]supplierInfo{"s:a": {ID: "a", Name: "A", Score: 90, OnTime: 0.9}, "s:b": {ID: "b", Name: "B", Score: 80, OnTime: 0.8}}
	a := orgAnalytics(aggs, nil, 2, "", []string{"agri", "packaging"}, sups)
	if len(a.Spend) != 2 || a.Spend[0].Month != "2026-08" || a.Categories[0] != "agri" {
		t.Fatalf("months/categories: %+v", a)
	}
	if a.PriceTrend.Item != "Kraft" || len(a.PriceTrend.Points) != 2 || a.PriceTrend.Points[1].Ours != 88.9 {
		t.Fatalf("trend %+v", a.PriceTrend)
	}
	if a.Suppliers[0].Id != "b" || a.Suppliers[0].SpendIdr != 2100 || a.Savings[1].BudgetIdr != 2200 {
		t.Fatalf("suppliers/savings %+v %+v", a.Suppliers, a.Savings)
	}
	if a.Demand.Points[0].Requests != 2 || a.UnitPrices[0].AvgIdr != 83 {
		t.Fatalf("demand/unit %+v %+v", a.Demand, a.UnitPrices)
	}
	if b := orgAnalytics(nil, nil, 12, "", nil, nil); b.Spend == nil || b.History == nil || b.PriceTrend.Points == nil {
		t.Fatal("empty analytics must serialise as []")
	}
}

func (e *testEnv) workspace(name string) (*http.Client, string, string) {
	e.t.Helper()
	c, userID := e.bidder("Owner " + name)
	var orgID string
	err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		var err error
		orgID, err = createOrg(t0(), tx, newOrg{Name: fmt.Sprintf("PT %s %d", name, time.Now().UnixNano()), Industry: "Manufaktur", Location: "Bandung, Jawa Barat",
			Categories: []string{"packaging"}, OwnerUserID: userID})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return c, userID, orgID
}

func (e *testEnv) member(orgID, role string) (*http.Client, string) {
	e.t.Helper()
	c, userID := e.bidder("Anggota " + role)
	e.exec(`INSERT INTO org_members (org_id, user_id, email, name, role, status, joined_at) SELECT $1, id, email, name, $2, 'active', now() FROM users WHERE id = $3`,
		orgID, role, userID)
	return c, userID
}

func (e *testEnv) notifications(userID string) []string {
	e.t.Helper()
	rows, err := e.db.Primary().Query(t0(), `SELECT title FROM notifications WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func want(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.Status != status || (code != "" && r.code() != code) {
		t.Fatalf("want %d %s, got %d %v", status, code, r.Status, r.Body)
	}
}

func TestOrgInviteNewAddress(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Undangan Baru")
	newcomer := fmt.Sprintf("calon-%d@example.test", time.Now().UnixNano())
	want(t, e.call(owner, "POST", "/orgs/"+orgID+"/team/invite", map[string]any{"email": newcomer, "role": "sales"}), 204, "")
	if m, ok := e.lastMail(newcomer); !ok || !strings.Contains(m.Body, "/register?email=") || e.mailCount(newcomer) != 1 {
		t.Fatalf("invite mail to a new address: %v %+v", ok, m)
	}
}

func TestOrgAccessProfileAndTeam(t *testing.T) {
	e := newEnv(t)
	owner, ownerID, orgID := e.workspace("Akses")
	base := "/orgs/" + orgID
	stranger, _ := e.bidder("Orang Luar")
	want(t, e.call(e.client(), "GET", base, nil), 401, "unauthenticated")
	want(t, e.call(stranger, "GET", base, nil), 403, "forbidden")
	want(t, e.call(owner, "GET", "/orgs/00000000-0000-0000-0000-000000000000", nil), 403, "forbidden")

	r := e.call(owner, "GET", base, nil)
	want(t, r, 200, "")
	if rules := r.Body["approvalRules"].([]any); len(rules) != 2 || len(r.Body["roles"].([]any)) != 5 {
		t.Fatalf("settings: %v", r.Body)
	}
	if p := r.Body["permissions"].(map[string]any)["sales"].(map[string]any); p["procurement"].([]any)[0] != "view" {
		t.Fatalf("permissions: %v", p)
	}

	sales, _ := e.member(orgID, "sales")
	profile := r.Body["profile"].(map[string]any)
	profile["legal"] = map[string]any{"nib": "123", "npwp": "01.234", "akta": ""}
	r = e.call(sales, "PUT", base+"/profile", profile)
	want(t, r, 403, "forbidden")
	if r.message() != "Peran Sales tidak punya izin kelola Profil bisnis" {
		t.Fatal(r.message())
	}
	r = e.call(owner, "PUT", base+"/profile", profile)
	want(t, r, 422, "validation")
	if r.field("nib") == "" || r.field("npwp") == "" {
		t.Fatalf("profile fields: %v", r.Body)
	}
	profile["legal"] = map[string]any{"nib": "9120004417263", "npwp": "02.417.556.8-421.000", "akta": "No. 14"}
	profile["description"] = "Produsen box karton"
	r = e.call(owner, "PUT", base+"/profile", profile)
	want(t, r, 200, "")
	if r.Body["legal"].(map[string]any)["nib"] != "9120004417263" {
		t.Fatalf("profile: %v", r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE org_id = $1 AND action = 'Ubah profil bisnis' AND changes @> '[{"field":"NIB"}]'`, orgID); n != int64(1) {
		t.Fatalf("profile audit: %v", n)
	}

	invitee, inviteeEmail := e.signedIn("Calon Anggota")
	_ = invitee
	e.exec(`UPDATE users SET email_verified_at = now() WHERE email = $1`, inviteeEmail)
	inviteeID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, inviteeEmail).(string)
	mailsBefore := e.mailCount(inviteeEmail)
	want(t, e.call(owner, "POST", base+"/team/invite", map[string]any{"email": "bukan-email", "role": "finance"}), 422, "validation")
	want(t, e.call(owner, "POST", base+"/team/invite", map[string]any{"email": strings.ToUpper(inviteeEmail), "role": "nope"}), 422, "validation")
	want(t, e.call(owner, "POST", base+"/team/invite", map[string]any{"email": " " + strings.ToUpper(inviteeEmail), "role": "finance", "department": "Keuangan"}), 204, "")
	want(t, e.call(owner, "POST", base+"/team/invite", map[string]any{"email": inviteeEmail, "role": "finance"}), 409, "exists")
	if n := e.notifications(inviteeID); len(n) != 1 || !strings.HasPrefix(n[0], "Undangan bergabung ke PT Akses") {
		t.Fatalf("invite notification: %v", n)
	}
	if err := e.server.NotificationMailTick(t0()); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.lastMail(inviteeEmail); !ok || !strings.Contains(m.Subject+m.Body, "Undangan") {
		t.Fatalf("invite mail: %v %v", ok, m.Subject)
	}
	if n := e.mailCount(inviteeEmail) - mailsBefore; n != 1 {
		t.Fatalf("invite mails to an existing account: %d, want 1", n)
	}

	r = e.call(owner, "GET", base+"/team", nil)
	want(t, r, 200, "")
	members := r.Body["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("members: %v", members)
	}
	var ownerMember, invited string
	for _, m := range members {
		m := m.(map[string]any)
		if m["role"] == "owner" {
			ownerMember = m["id"].(string)
		}
		if m["status"] == "invited" {
			invited = m["id"].(string)
		}
		if _, leak := m["userId"]; leak {
			t.Fatal("userId exposed")
		}
	}
	want(t, e.call(owner, "PATCH", base+"/team/members/"+ownerMember, map[string]any{"role": "finance"}), 409, "last_owner")
	want(t, e.call(owner, "PATCH", base+"/team/members/"+invited, map[string]any{"role": "ghost"}), 422, "validation")
	want(t, e.call(owner, "PATCH", base+"/team/members/"+invited, map[string]any{"role": "operations", "department": "Gudang"}), 204, "")
	want(t, e.call(owner, "DELETE", base+"/team/members/"+ownerMember, nil), 409, "self")
	want(t, e.call(sales, "DELETE", base+"/team/members/"+invited, nil), 403, "forbidden")
	want(t, e.call(owner, "DELETE", base+"/team/members/"+invited, nil), 204, "")
	want(t, e.call(owner, "DELETE", base+"/team/members/"+invited, nil), 404, "not_found")

	settings := e.call(owner, "GET", base, nil).Body
	roles := append(settings["roles"].([]any), map[string]any{"id": "custom-qc", "label": "Quality Control", "custom": true})
	perms := settings["permissions"].(map[string]any)
	perms["custom-qc"] = map[string]any{"inventory": []string{"view", "manage"}}
	body := map[string]any{"roles": roles, "permissions": perms, "departments": []string{"Direksi", "QC", "QC"},
		"approvalRules": []any{map[string]any{"id": "x", "label": "Semua > 1 jt", "minAmountIdr": 1_000_000, "approvers": []string{"custom-qc"}, "appliesTo": []string{"procurement"}}}}
	r = e.call(owner, "PUT", base+"/team/settings", body)
	want(t, r, 200, "")
	if d := r.Body["departments"].([]any); len(d) != 2 || len(r.Body["approvalRules"].([]any)) != 1 {
		t.Fatalf("settings saved: %v", r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE org_id = $1 AND action = 'Ubah pengaturan tim'`, orgID); n != int64(1) {
		t.Fatal("settings audit")
	}
	qc, _ := e.member(orgID, "custom-qc")
	want(t, e.call(qc, "POST", base+"/inventory/schedules", map[string]any{"item": "Kraft", "quantity": map[string]any{"value": 10, "unit": "ton"}}), 201, "")
	want(t, e.call(qc, "POST", base+"/inventory/items", map[string]any{"name": "Kraft", "categoryId": "packaging", "quantity": map[string]any{"value": 1, "unit": "ton"}}), 403, "forbidden")

	bad := map[string]any{"roles": roles[:4], "permissions": perms, "departments": []string{}, "approvalRules": []any{}}
	r = e.call(owner, "PUT", base+"/team/settings", bad)
	want(t, r, 422, "validation")
	if !strings.Contains(r.field("roles"), "Sales") {
		t.Fatalf("builtin removal: %v", r.Body)
	}
	noQC := map[string]any{"roles": roles[:5], "permissions": map[string]any{}, "departments": []string{}, "approvalRules": []any{}}
	if r = e.call(owner, "PUT", base+"/team/settings", noQC); r.Status != 422 || !strings.Contains(r.field("roles"), "Quality Control") {
		t.Fatalf("held role removal: %v", r.Body)
	}
	_ = ownerID
}

func TestOrgInventoryOverviewAnalytics(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Gudang")
	base := "/orgs/" + orgID
	sales, _ := e.member(orgID, "sales")
	item := map[string]any{"name": "Kraft liner", "categoryId": "packaging", "quantity": map[string]any{"value": 38, "unit": "ton"}, "moq": 5}
	want(t, e.call(sales, "POST", base+"/inventory/items", item), 403, "forbidden")
	want(t, e.call(owner, "POST", base+"/inventory/items", map[string]any{"name": " ", "categoryId": "packaging", "quantity": map[string]any{"value": -1, "unit": "ton"}}), 422, "validation")
	want(t, e.call(owner, "POST", base+"/inventory/items", item), 201, "")
	r := e.call(owner, "POST", base+"/inventory/import", map[string]any{"fileName": "stok.csv", "items": []any{
		map[string]any{"name": "Box", "categoryId": "packaging", "quantity": map[string]any{"value": 10, "unit": "pcs"}},
		map[string]any{"name": "", "categoryId": "packaging", "quantity": map[string]any{"value": 1, "unit": "pcs"}}}})
	want(t, r, 422, "validation")
	if r.message() != "Baris 3: nama kosong" {
		t.Fatal(r.message())
	}
	r = e.call(owner, "POST", base+"/inventory/import", map[string]any{"fileName": "stok.csv", "items": []any{
		map[string]any{"name": "Box", "categoryId": "packaging", "quantity": map[string]any{"value": 10, "unit": ""}},
		map[string]any{"name": "Lem", "categoryId": "manufacturing", "quantity": map[string]any{"value": 2, "unit": "kg"}, "sku": "LEM-1"}}})
	if r.Status != 200 || r.Body["imported"] != float64(2) {
		t.Fatalf("import: %v", r.Body)
	}
	r = e.call(sales, "GET", base+"/inventory", nil)
	items := r.Body["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["sku"] != "SKU-1" || items[0].(map[string]any)["quantity"].(map[string]any)["unit"] != "unit" {
		t.Fatalf("inventory: %v", items)
	}

	month := time.Now().UTC().Format("2006-01") + "-01"
	supID := e.scalar(`INSERT INTO suppliers (name, categories, region, seed_rating, verified) VALUES ('PT Kertas Uji', '{packaging}', 'Jawa Barat', 4.5, true) RETURNING id::text`).(string)
	e.exec(`INSERT INTO supplier_scorecards (supplier_id, month, price, reliability, quality, delivery) VALUES ($1, $2, 80, 90, 85, 95)`, supID, month)
	e.exec(`INSERT INTO org_purchase_history (org_id, code, month, item, category_id, supplier_id, quantity, unit, unit_price_idr, budget_unit_idr, market_unit_idr, via, bidders, opening_idr)
		VALUES ($1, 'HST-1', $2, 'Kraft liner', 'packaging', $3, 10, 'ton', 1000000, 1100000, 1050000, 'auction', 5, 1080000),
		       ($1, 'HST-2', ($2::date - interval '1 month')::date, 'Kraft liner', 'packaging', $3, 5, 'ton', 1000000, 1000000, 1000000, 'direct', NULL, NULL)`,
		orgID, month, supID)
	r = e.call(sales, "GET", base+"/overview", nil)
	want(t, r, 200, "")
	st := r.Body["stats"].(map[string]any)
	if st["spendMonthIdr"] != float64(10_000_000) || st["savingsMonthIdr"] != float64(1_000_000) || st["savingsTargetIdr"] != float64(10_000_000) {
		t.Fatalf("overview stats: %v", st)
	}
	if len(r.Body["activity"].([]any)) == 0 || r.Body["pipeline"].(map[string]any)["draft"] != float64(0) {
		t.Fatalf("overview: %v", r.Body)
	}
	r = e.call(sales, "GET", base+"/analytics?months=12", nil)
	want(t, r, 200, "")
	if h := r.Body["history"].([]any); len(h) != 2 || h[0].(map[string]any)["supplier"] != "PT Kertas Uji" {
		t.Fatalf("analytics history: %v", r.Body["history"])
	}
	if a := r.Body["auctions"].([]any); len(a) != 1 || a[0].(map[string]any)["bidders"] != float64(5) {
		t.Fatalf("analytics auctions: %v", r.Body["auctions"])
	}
	if s := r.Body["suppliers"].([]any); len(s) != 1 || s[0].(map[string]any)["score"] != float64(88) {
		t.Fatalf("analytics suppliers: %v", r.Body["suppliers"])
	}
	if r = e.call(sales, "GET", base+"/analytics?months=1&category=agri", nil); len(r.Body["history"].([]any)) != 0 {
		t.Fatalf("category filter: %v", r.Body)
	}
}

func TestOrgSuppliers(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Pemasok")
	base := "/orgs/" + orgID
	sales, _ := e.member(orgID, "sales")
	procurement, _ := e.member(orgID, "procurement")
	tag := fmt.Sprint(time.Now().UnixNano())
	verified := e.scalar(`INSERT INTO suppliers (name, categories, region, seed_rating, verified, documents) VALUES ($1, '{packaging}', 'Jawa Barat', 4.0, true, '{NIB.pdf,NPWP.pdf}') RETURNING id::text`,
		"PT Verif "+tag).(string)
	thin := e.scalar(`INSERT INTO suppliers (name, categories, region, seed_rating, verified, documents) VALUES ($1, '{agri}', 'Bali', 3.0, false, '{NIB.pdf}') RETURNING id::text`,
		"UD Tipis "+tag).(string)

	list := func(c *http.Client, query string) []map[string]any {
		t.Helper()
		var out []map[string]any
		res, err := c.Get(e.srv.URL + BasePath + base + "/suppliers?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if l := list(sales, "q="+tag); len(l) != 2 {
		t.Fatalf("list: %v", l)
	}
	if l := list(sales, "q="+tag+"&verified=1"); len(l) != 1 || l[0]["id"] != verified {
		t.Fatalf("verified filter: %v", l)
	}
	if l := list(sales, "q="+tag+"&verified=false&region=Bali"); len(l) != 1 || l[0]["id"] != thin {
		t.Fatalf("unverified filter: %v", l)
	}

	act := func(c *http.Client, sid string, body map[string]any) resp {
		return e.call(c, "POST", base+"/suppliers/"+sid+"/actions", body)
	}
	want(t, act(sales, verified, map[string]any{"action": "shortlist"}), 403, "forbidden")
	want(t, act(procurement, verified, map[string]any{"action": "shortlist"}), 200, "")
	want(t, act(owner, "00000000-0000-0000-0000-000000000000", map[string]any{"action": "shortlist"}), 404, "not_found")
	want(t, act(owner, thin, map[string]any{"action": "verify"}), 409, "docs_missing")
	want(t, act(owner, verified, map[string]any{"action": "rate", "rating": 0}), 422, "validation")
	r := act(owner, verified, map[string]any{"action": "rate", "rating": 5})
	if r.Status != 200 || r.Body["myRating"] != float64(5) || r.Body["rating"] != float64(4.1) || r.Body["relation"] != "shortlisted" {
		t.Fatalf("rate: %v", r.Body)
	}
	want(t, act(owner, verified, map[string]any{"action": "unblock"}), 409, "not_blocked")
	want(t, act(owner, verified, map[string]any{"action": "block"}), 422, "validation")
	want(t, act(owner, verified, map[string]any{"action": "block", "reason": "Kualitas buruk"}), 200, "")
	want(t, act(owner, verified, map[string]any{"action": "rate", "rating": 3}), 409, "blocked")
	if l := list(sales, "q="+tag+"&relation=blocked"); len(l) != 1 {
		t.Fatalf("relation filter: %v", l)
	}
	want(t, act(owner, verified, map[string]any{"action": "unblock"}), 200, "")
	r = e.call(sales, "GET", base+"/suppliers/"+verified, nil)
	want(t, r, 200, "")
	if len(r.Body["activity"].([]any)) != 4 || r.Body["history"] == nil || r.Body["purchases"] == nil {
		t.Fatalf("supplier page: %v", r.Body)
	}
}

func TestOrgProcurementAndPools(t *testing.T) {
	e := newEnv(t)
	owner, ownerID, orgID := e.workspace("Pengadaan")
	base := "/orgs/" + orgID
	finance, financeID := e.member(orgID, "finance")
	proc, _ := e.member(orgID, "procurement")
	sales, _ := e.member(orgID, "sales")
	unit := fmt.Sprintf("roll%d", time.Now().UnixNano())
	input := func(budget int, submit bool) map[string]any {
		return map[string]any{"need": "Stretch film", "categoryId": "packaging", "quantity": map[string]any{"value": 100, "unit": unit}, "budgetIdr": budget,
			"deadline": time.Now().Add(240 * time.Hour), "spec": "23 mikron", "deliveryLocation": "Gudang Cimahi", "visibility": "public", "invitedSupplierIds": []string{},
			"submit": submit}
	}
	want(t, e.call(sales, "POST", base+"/procurement", input(10_000_000, true)), 403, "forbidden")
	bad := input(0, true)
	bad["deadline"] = time.Now().Add(-time.Hour)
	bad["visibility"] = "invite"
	r := e.call(proc, "POST", base+"/procurement", bad)
	if r.Status != 422 || r.field("budgetIdr") == "" || r.field("deadline") == "" || r.field("invited") == "" {
		t.Fatalf("validation: %v", r.Body)
	}

	r = e.call(proc, "POST", base+"/procurement", input(60_000_000, true))
	want(t, r, 201, "")
	id := r.Body["id"].(string)
	if r.Body["status"] != "pending_approval" || len(r.Body["requiredApprovers"].([]any)) != 2 || !strings.HasPrefix(r.Body["code"].(string), "PRQ-") {
		t.Fatalf("created: %v", r.Body)
	}
	if n := e.notifications(financeID); len(n) != 1 || !strings.HasPrefix(n[0], "Perlu approval kamu: PRQ-") {
		t.Fatalf("finance notified: %v", n)
	}
	if len(e.notifications(ownerID)) != 1 {
		t.Fatal("owner notified")
	}
	act := func(c *http.Client, body map[string]any) resp {
		return e.call(c, "POST", base+"/procurement/"+id+"/actions", body)
	}
	want(t, act(sales, map[string]any{"action": "approve"}), 409, "invalid_action")
	want(t, act(proc, map[string]any{"action": "publish"}), 409, "invalid_action")
	want(t, act(finance, map[string]any{"action": "reject", "note": " "}), 422, "validation")
	r = act(finance, map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "pending_approval" || len(r.Body["approvals"].([]any)) != 1 {
		t.Fatalf("finance approve: %v", r.Body)
	}
	want(t, act(finance, map[string]any{"action": "approve"}), 409, "invalid_action")
	r = act(owner, map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "approved" {
		t.Fatalf("owner approve: %v", r.Body)
	}
	if by := r.Body["approvals"].([]any)[0].(map[string]any)["by"].(string); !strings.HasSuffix(by, "(Finance)") {
		t.Fatal(by)
	}
	r = e.call(owner, "GET", base+"/overview", nil)
	if r.Body["pipeline"].(map[string]any)["published"] != float64(1) || len(r.Body["waiting"].([]any)) != 0 {
		t.Fatalf("overview: %v", r.Body)
	}

	r = act(proc, map[string]any{"action": "collective", "quantity": 80, "optIn": true})
	if r.Status != 200 || r.Body["status"] != "in_collective" || r.Body["visibility"] != "aggregate" || r.Body["poolId"] == nil {
		t.Fatalf("collective: %v", r.Body)
	}
	poolID := r.Body["poolId"].(string)
	r = e.call(sales, "GET", base+"/procurement/"+id, nil)
	pool := r.Body["pool"].(map[string]any)
	if pool["thresholdQty"] != float64(800) || pool["baseUnitPriceIdr"] != float64(600_000) || pool["region"] != "Jawa Barat" || len(r.Body["activity"].([]any)) < 3 {
		t.Fatalf("detail: %v", r.Body)
	}

	other, _, otherID := e.workspace("Tetangga")
	want(t, e.call(other, "POST", "/orgs/"+otherID+"/collective/"+poolID+"/join", map[string]any{"quantity": 0}), 422, "validation")
	r = e.call(other, "POST", "/orgs/"+otherID+"/collective/"+poolID+"/join", map[string]any{"quantity": 300})
	want(t, r, 200, "")
	if m := r.Body["members"].([]any); m[0].(map[string]any)["name"] != e.scalar(`SELECT name FROM orgs WHERE id = $1`, orgID) || m[1].(map[string]any)["mine"] != true {
		t.Fatalf("other view: %v", m)
	}
	r = e.call(owner, "POST", base+"/collective/"+poolID+"/join", map[string]any{"quantity": 100, "optIn": false})
	if m := r.Body["members"].([]any); m[1].(map[string]any)["name"] != "Bisnis lain #2" || m[0].(map[string]any)["mine"] != true {
		t.Fatalf("own view: %v", m)
	}
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/market", nil), 409, "not_ready")
	want(t, e.call(other, "POST", "/orgs/"+otherID+"/collective/"+poolID+"/join", map[string]any{"quantity": 700}), 200, "")
	want(t, e.call(proc, "POST", base+"/collective/"+poolID+"/market", nil), 200, "")
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/market", nil), 409, "closed")
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/leave", nil), 409, "closed")
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/join", map[string]any{"quantity": 1}), 409, "closed")

	e.exec(`UPDATE collective_pools SET status = 'open', market_requested_at = NULL WHERE id = $1`, poolID)
	want(t, e.call(sales, "POST", base+"/collective/"+poolID+"/leave", nil), 403, "forbidden")
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/leave", nil), 204, "")
	want(t, e.call(owner, "POST", base+"/collective/"+poolID+"/leave", nil), 409, "not_member")
	if r = e.call(owner, "GET", base+"/procurement/"+id, nil); r.Body["request"].(map[string]any)["status"] != "approved" || r.Body["pool"] != nil {
		t.Fatalf("after leave: %v", r.Body)
	}

	r = e.call(proc, "POST", base+"/procurement", input(5_000_000, false))
	draft := r.Body["id"].(string)
	if r.Body["status"] != "draft" {
		t.Fatalf("draft: %v", r.Body)
	}
	if r = e.call(proc, "POST", base+"/procurement/"+draft+"/actions", map[string]any{"action": "submit"}); r.Body["status"] != "approved" {
		t.Fatalf("submit: %v", r.Body)
	}
	if r = e.call(proc, "POST", base+"/procurement/"+draft+"/actions", map[string]any{"action": "cancel"}); r.Body["status"] != "cancelled" {
		t.Fatalf("cancel: %v", r.Body)
	}
	want(t, e.call(owner, "GET", base+"/procurement/00000000-0000-0000-0000-000000000000", nil), 404, "not_found")
	want(t, e.call(other, "GET", base+"/procurement/"+id, nil), 403, "forbidden")

	r = e.call(owner, "POST", base+"/collective", map[string]any{"title": " ", "quantity": 1, "baseUnitPriceIdr": 1, "categoryId": "packaging", "unit": "",
		"deadline": time.Now().Add(-time.Hour)})
	if r.Status != 422 || r.field("unit") == "" || r.field("deadline") == "" || r.field("title") == "" {
		t.Fatalf("pool validation: %v", r.Body)
	}
	r = e.call(owner, "POST", base+"/collective", map[string]any{"title": "Karung goni", "quantity": 100, "baseUnitPriceIdr": 9000, "categoryId": "agri",
		"unit": unit, "deadline": time.Now().Add(48 * time.Hour)})
	if r.Status != 201 || r.Body["thresholdQty"] != float64(800) || r.Body["members"].([]any)[0].(map[string]any)["mine"] != true {
		t.Fatalf("pool create: %v", r.Body)
	}
}

func (e *testEnv) closeAuction(id string) {
	e.t.Helper()
	e.exec(`UPDATE auctions SET ends_at = now() - interval '1 second', starts_at = now() - interval '1 hour' WHERE id = $1`, id)
	if err := e.server.AuctionTick(t0()); err != nil {
		e.t.Fatal(err)
	}
}

func TestOrgAuctionLifecycle(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Lelang")
	base := "/orgs/" + orgID
	finance, _ := e.member(orgID, "finance")
	proc, _ := e.member(orgID, "procurement")
	sales, _ := e.member(orgID, "sales")

	pr := e.call(proc, "POST", base+"/procurement", map[string]any{"need": "Box karton", "categoryId": "packaging", "quantity": map[string]any{"value": 1000, "unit": "pcs"},
		"budgetIdr": 2_000_000, "deadline": time.Now().Add(240 * time.Hour), "spec": "", "deliveryLocation": "", "visibility": "public", "invitedSupplierIds": []string{},
		"submit": true}).Body
	prID := pr["id"].(string)

	auction := func(reserve int, procurementID string) map[string]any {
		return map[string]any{"title": "Box karton RSC", "categoryId": "packaging", "type": "reverse", "objective": "procurement", "multiLot": true,
			"lots": []any{map[string]any{"item": "Box A", "quantity": map[string]any{"value": 1000, "unit": "pcs"}, "spec": "3 ply", "reservePriceIdr": reserve},
				map[string]any{"item": "Box B", "quantity": map[string]any{"value": 500, "unit": "pcs"}, "spec": "5 ply", "reservePriceIdr": reserve}},
			"rules": map[string]any{"minStepIdr": 10, "visibility": "full", "autoExtension": true, "withdraw": "before_last_30", "award": "lowest",
				"weights": map[string]any{"price": 60, "quality": 20, "delivery": 10, "reliability": 10}},
			"qualification": map[string]any{"documents": []string{"NIB"}, "minRating": 4, "regions": []string{}}, "invited": []string{},
			"schedule": map[string]any{"durationMinutes": 60}, "procurementId": procurementID}
	}
	want(t, e.call(sales, "POST", base+"/auctions", auction(1000, "")), 403, "forbidden")
	r := e.call(proc, "POST", base+"/auctions", map[string]any{"title": "", "categoryId": "packaging", "type": "reverse", "objective": "procurement", "multiLot": false,
		"lots": []any{}, "rules": auction(1, "")["rules"], "qualification": auction(1, "")["qualification"], "invited": []string{"00000000-0000-0000-0000-000000000000"},
		"schedule": map[string]any{"durationMinutes": 0}, "procurementId": "00000000-0000-0000-0000-000000000000"})
	if r.Status != 422 || r.field("title") == "" || r.field("lots") == "" || r.field("duration") == "" || r.field("invited") == "" || r.field("procurementId") == "" {
		t.Fatalf("validation: %v", r.Body)
	}

	r = e.call(proc, "POST", base+"/auctions", auction(1000, prID))
	want(t, r, 201, "")
	aid := r.Body["id"].(string)
	live := r.Body["live"].([]any)
	if r.Body["status"] != "live" || len(live) != 2 || r.Body["multiLot"] != true || r.Body["valueIdr"] != float64(1_500_000) {
		t.Fatalf("created: %v", r.Body)
	}
	lot1, lot2 := live[0].(map[string]any)["auctionId"].(string), live[1].(map[string]any)["auctionId"].(string)
	if e.scalar(`SELECT owner_org_id::text FROM auctions WHERE id = $1`, lot1) != orgID {
		t.Fatal("lot auction owner")
	}
	if p := e.call(owner, "GET", base+"/procurement/"+prID, nil).Body["request"].(map[string]any); p["status"] != "in_auction" || p["auctionId"] != aid {
		t.Fatalf("procurement linked: %v", p)
	}
	ar := e.call(e.client(), "GET", "/auctions/"+lot1, nil)
	if ar.Status != 200 || !strings.Contains(ar.Body["title"].(string), "Lot 1: Box A") {
		t.Fatalf("public lot: %v", ar.Body)
	}

	e.qualify(proc, lot1)
	want(t, e.call(proc, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 990}), 403, "owner")
	b1, _ := e.bidder("Supplier Satu")
	b2, b2ID := e.bidder("Supplier Dua")
	for _, b := range []*http.Client{b1, b2} {
		e.qualify(b, lot1)
		e.qualify(b, lot2)
	}
	want(t, e.call(b1, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 950}), 200, "")
	want(t, e.call(b2, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 900}), 200, "")
	want(t, e.call(b1, "POST", "/auctions/"+lot2+"/bids", map[string]any{"priceIdr": 980}), 200, "")

	b2Party := e.scalar(`SELECT id::text FROM parties WHERE user_id = $1`, b2ID).(string)
	supID := e.scalar(`INSERT INTO suppliers (name, categories, region, verified, party_id) VALUES ($1, '{packaging}', 'Jawa Barat', true, $2) RETURNING id::text`,
		fmt.Sprintf("PT Dua %d", time.Now().UnixNano()), b2Party).(string)
	e.exec(`INSERT INTO supplier_scorecards (supplier_id, month, price, reliability, quality, delivery) VALUES ($1, date_trunc('month', now())::date, 80, 80, 90, 70)`, supID)

	want(t, e.call(owner, "POST", base+"/auctions/"+aid+"/award", map[string]any{"lines": []any{}, "reason": "x"}), 409, "not_closed")
	e.closeAuction(lot1)
	e.closeAuction(lot2)
	r = e.call(sales, "GET", base+"/auctions", nil)
	_ = r
	r = e.call(sales, "GET", base+"/auctions/"+aid+"/evaluation", nil)
	want(t, r, 200, "")
	lots := r.Body["lots"].([]any)
	offers1 := lots[0].(map[string]any)["offers"].([]any)
	if r.Body["auction"].(map[string]any)["status"] != "closed" || len(offers1) != 2 || lots[0].(map[string]any)["status"] != "closed" {
		t.Fatalf("evaluation: %v", r.Body)
	}
	best := offers1[0].(map[string]any)
	if best["priceIdr"] != float64(900) || best["supplierId"] != supID || best["quality"] != float64(90) || best["supplier"].(map[string]any)["reputation"] != float64(80) {
		t.Fatalf("best offer: %v", best)
	}
	second := offers1[1].(map[string]any)["id"].(string)
	lot2Offer := lots[1].(map[string]any)["offers"].([]any)[0].(map[string]any)["id"].(string)

	award := func(c *http.Client, lines []any, reason string) resp {
		return e.call(c, "POST", base+"/auctions/"+aid+"/award", map[string]any{"lines": lines, "reason": reason})
	}
	line := func(offer string, qty float64) map[string]any {
		return map[string]any{"offerId": offer, "supplier": "spoofed", "quantity": qty, "priceIdr": 1}
	}
	want(t, award(sales, []any{}, "x"), 403, "forbidden")
	want(t, award(owner, []any{[]any{line(best["id"].(string), 1000)}, []any{line(lot2Offer, 500)}}, " "), 422, "validation")
	r = award(owner, []any{[]any{line(best["id"].(string), 800), line(second, 300)}, []any{line(lot2Offer, 500)}}, "Termurah")
	if r.Status != 422 || r.field("lines.0") == "" {
		t.Fatalf("over-allocation: %v", r.Body)
	}
	r = award(owner, []any{[]any{line(lot2Offer, 100)}, []any{}}, "Termurah")
	if r.Status != 422 || r.field("lines.0.0") == "" || r.field("lines.1") == "" {
		t.Fatalf("foreign offer / empty lot: %v", r.Body)
	}
	r = award(owner, []any{[]any{line(best["id"].(string), 700), line(second, 300)}, []any{line(lot2Offer, 500)}}, "Split termurah")
	want(t, r, 200, "")
	aw := r.Body["award"].(map[string]any)
	if r.Body["status"] != "awarded" || aw["lines"].([]any)[0].([]any)[0].(map[string]any)["priceIdr"] != float64(900) ||
		!strings.HasPrefix(aw["lines"].([]any)[0].([]any)[0].(map[string]any)["supplier"].(string), "PT Dua") {
		t.Fatalf("awarded: %v", r.Body)
	}
	if e.scalar(`SELECT status FROM auctions WHERE id = $1`, lot1) != "awarded" || e.scalar(`SELECT status FROM bids WHERE auction_id = $1 AND bidder_user_id = $2 ORDER BY seq DESC LIMIT 1`, lot1, b2ID) != "won" {
		t.Fatal("lot auction settled")
	}
	want(t, award(owner, []any{[]any{line(best["id"].(string), 1)}, []any{line(lot2Offer, 1)}}, "lagi"), 409, "awarded")
	if p := e.call(owner, "GET", base+"/procurement/"+prID, nil).Body["request"].(map[string]any); p["status"] != "awarded" {
		t.Fatalf("procurement awarded: %v", p)
	}

	want(t, e.call(finance, "POST", base+"/auctions/"+aid+"/po", nil), 403, "forbidden")
	want(t, e.call(owner, "POST", base+"/auctions/00000000-0000-0000-0000-000000000000/po", nil), 404, "not_found")
	r = e.call(owner, "POST", base+"/auctions/"+aid+"/po", nil)
	want(t, r, 200, "")
	ids := r.Body["transactionIds"].([]any)
	if len(ids) != 3 || !strings.HasPrefix(r.Body["poNumber"].(string), "PO-PL-") {
		t.Fatalf("po: %v", r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM trades t JOIN parties p ON p.id = t.buyer_party_id WHERE p.org_id = $1 AND t.status = 'agreement'`, orgID); n != int64(3) {
		t.Fatalf("trades: %v", n)
	}
	if e.scalar(`SELECT total_idr FROM trades WHERE id = $1`, ids[0]) != int64(630_000) {
		t.Fatal("trade amount from the offer")
	}
	if p := e.scalar(`SELECT payload->>'buyerOrgId' FROM outbox WHERE topic = 'trade.status' AND aggregate_id = $1`, ids[0]); p != orgID {
		t.Fatalf("analytics fact: %v", p)
	}
	want(t, e.call(owner, "POST", base+"/auctions/"+aid+"/po", nil), 409, "exists")
	if p := e.call(owner, "GET", base+"/procurement/"+prID, nil).Body["request"].(map[string]any); p["status"] != "po_issued" {
		t.Fatalf("procurement po: %v", p)
	}
	r = e.call(sales, "GET", base+"/suppliers/"+supID, nil)
	if h := r.Body["history"].([]any); len(h) != 1 || h[0].(map[string]any)["role"] != "buyer" || r.Body["transactions"] != float64(1) {
		t.Fatalf("supplier history: %v", r.Body)
	}
	r = e.call(sales, "GET", base+"/analytics", nil)
	if len(r.Body["history"].([]any)) != 3 || len(r.Body["auctions"].([]any)) != 3 {
		t.Fatalf("analytics from awards: %v", r.Body)
	}

	pr2 := e.call(proc, "POST", base+"/procurement", map[string]any{"need": "Kraft", "categoryId": "packaging", "quantity": map[string]any{"value": 10, "unit": "ton"},
		"budgetIdr": 40_000_000, "deadline": time.Now().Add(240 * time.Hour), "spec": "", "deliveryLocation": "", "visibility": "public", "invitedSupplierIds": []string{},
		"submit": true}).Body["id"].(string)
	r = e.call(proc, "POST", base+"/auctions", auction(100_000, pr2))
	want(t, r, 201, "")
	big := r.Body["id"].(string)
	if r.Body["status"] != "pending_approval" || len(r.Body["live"].([]any)) != 0 {
		t.Fatalf("pending: %v", r.Body)
	}
	if w := e.call(finance, "GET", base+"/overview", nil).Body["waiting"].([]any); len(w) != 1 || w[0].(map[string]any)["kind"] != "auction" {
		t.Fatalf("waiting: %v", w)
	}
	decide := func(c *http.Client, body map[string]any) resp {
		return e.call(c, "POST", base+"/auctions/"+big+"/actions", body)
	}
	want(t, decide(sales, map[string]any{"action": "approve"}), 403, "forbidden")
	want(t, decide(finance, map[string]any{"action": "reject"}), 422, "validation")
	want(t, decide(finance, map[string]any{"action": "approve"}), 200, "")
	r = decide(owner, map[string]any{"action": "reject", "note": "Anggaran dipotong"})
	if r.Status != 200 || r.Body["status"] != "rejected" {
		t.Fatalf("rejected: %v", r.Body)
	}
	want(t, decide(owner, map[string]any{"action": "approve"}), 409, "invalid_action")
	if p := e.call(owner, "GET", base+"/procurement/"+pr2, nil).Body["request"].(map[string]any); p["status"] != "approved" || p["auctionId"] != nil {
		t.Fatalf("handed back: %v", p)
	}

	r = e.call(proc, "POST", base+"/auctions", auction(100_000, pr2))
	again := r.Body["id"].(string)
	want(t, e.call(finance, "POST", base+"/auctions/"+again+"/actions", map[string]any{"action": "approve"}), 200, "")
	r = e.call(owner, "POST", base+"/auctions/"+again+"/actions", map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "live" || len(r.Body["live"].([]any)) != 2 {
		t.Fatalf("opened after approval: %v", r.Body)
	}
}

func TestOrgDocumentsAndVerification(t *testing.T) {
	e := newEnv(t)
	if e.server.Storage == nil {
		t.Skip("no object storage")
	}
	owner, _, orgID := e.workspace("Dokumen")
	base := "/orgs/" + orgID
	admin, adminID := e.bidder("Admin Verifikasi")
	_ = admin
	e.exec(`INSERT INTO user_capabilities (user_id, capability) VALUES ($1, 'admin')`, adminID)
	pdf := []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

	want(t, e.call(owner, "POST", base+"/profile/verification", nil), 422, "validation")
	kyc := e.upload(owner, "kyc_ktp", "image/png", pngBytes(t))
	r := e.call(owner, "POST", base+"/profile/documents", map[string]any{"uploadId": kyc, "kind": "nib"})
	if r.Status != 422 || r.field("uploadId") != "File ini diunggah untuk keperluan lain." {
		t.Fatalf("wrong purpose: %v", r.Body)
	}
	sales, _ := e.member(orgID, "sales")
	want(t, e.call(sales, "POST", base+"/profile/documents", map[string]any{"uploadId": kyc, "kind": "nib"}), 403, "forbidden")
	for _, kind := range []string{"nib", "npwp"} {
		want(t, e.call(owner, "POST", base+"/profile/documents", map[string]any{"uploadId": e.upload(owner, "org_document", "application/pdf", pdf), "kind": kind}), 200, "")
	}
	r = e.call(owner, "POST", base+"/profile/verification", nil)
	if r.Status != 422 || r.message() != "Dokumen belum lengkap: AKTA" {
		t.Fatalf("missing akta: %v", r.Body)
	}
	id := e.upload(owner, "org_document", "application/pdf", pdf)
	want(t, e.call(owner, "POST", base+"/profile/documents", map[string]any{"uploadId": id, "kind": "akta"}), 200, "")
	r = e.call(owner, "POST", base+"/profile/documents", map[string]any{"uploadId": id, "kind": "other"})
	if r.Status != 422 || r.field("uploadId") != "File ini sudah dipakai. Unggah ulang." {
		t.Fatalf("reuse: %v", r.Body)
	}
	r = e.call(owner, "POST", base+"/profile/documents", map[string]any{"uploadId": e.upload(owner, "org_document", "image/png", pngBytes(t)), "kind": "nib"})
	if docs := r.Body["documents"].([]any); r.Status != 200 || len(docs) != 3 {
		t.Fatalf("nib replaced, not added: %v", r.Body)
	}
	r = e.call(owner, "POST", base+"/profile/verification", nil)
	if r.Status != 200 || r.Body["verification"] != "pending" {
		t.Fatalf("verification: %v", r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM verification_documents d JOIN verification_requests r ON r.id = d.request_id WHERE r.org_id = $1 AND r.kind = 'business' AND d.object_key LIKE 'org_document/%'`, orgID); n != int64(3) {
		t.Fatalf("request documents: %v", n)
	}
	if n := e.notifications(adminID); len(n) != 1 || n[0] != "Pengajuan verifikasi bisnis baru" {
		t.Fatalf("admin notified: %v", n)
	}
	want(t, e.call(owner, "POST", base+"/profile/verification", nil), 409, "already_submitted")
}

func TestOrgAnalyticsSources(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Analitik")
	supID := e.scalar(`INSERT INTO suppliers (name, categories, region) VALUES ('CV Analitik', '{packaging}', 'Jawa Barat') RETURNING id::text`).(string)
	e.exec(`INSERT INTO org_purchase_history (org_id, code, month, item, category_id, supplier_id, quantity, unit, unit_price_idr, budget_unit_idr, market_unit_idr, via)
		VALUES ($1, 'HST-9', date_trunc('month', now())::date, 'Lem', 'packaging', $2, 3, 'kg', 1000, 1000, 1000, 'direct')`, orgID, supID)

	down, err := analytics.Open("127.0.0.1:1", "ecopurnity", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	e.server.Analytics = down
	r := e.call(owner, "GET", "/orgs/"+orgID+"/analytics", nil)
	if r.Status != 200 || len(r.Body["history"].([]any)) != 1 {
		t.Fatalf("fallback: %v", r.Body)
	}

	ch, _ := chSchema(t)
	if ch == nil {
		t.Skip("no clickhouse")
	}
	r = e.call(owner, "POST", "/orgs/"+orgID+"/auctions", orgAuctionBody("Lem PVAc", "anytime", [3]any{"Lem PVAc", 200, 5000}))
	want(t, r, 201, "")
	aid, lot := r.Body["id"].(string), r.Body["live"].([]any)[0].(map[string]any)["auctionId"].(string)
	b, _ := e.bidder("Supplier Lem")
	e.qualify(b, lot)
	want(t, e.call(b, "POST", "/auctions/"+lot+"/bids", map[string]any{"priceIdr": 4500}), 200, "")
	e.closeAuction(lot)
	offer := e.call(owner, "GET", "/orgs/"+orgID+"/auctions/"+aid+"/evaluation", nil).Body["lots"].([]any)[0].(map[string]any)["offers"].([]any)[0].(map[string]any)["id"]
	want(t, e.call(owner, "POST", "/orgs/"+orgID+"/auctions/"+aid+"/award", map[string]any{"reason": "Termurah",
		"lines": []any{[]any{map[string]any{"offerId": offer, "supplier": "x", "quantity": 200, "priceIdr": 1}}}}), 200, "")
	want(t, e.call(owner, "POST", "/orgs/"+orgID+"/auctions/"+aid+"/po", nil), 200, "")
	e.exec(`INSERT INTO org_purchase_history (org_id, code, month, item, category_id, supplier_id, quantity, unit, unit_price_idr, budget_unit_idr, market_unit_idr, via, bidders, opening_idr)
		VALUES ($1, 'HST-10', (date_trunc('month', now()) - interval '1 month')::date, 'Lem', 'packaging', $2, 4, 'kg', 1000, 1100, 1050, 'auction', 6, 1200)`, orgID, supID)

	rows, err := e.db.Primary().Query(t0(), `SELECT id, topic, aggregate_id, created_at, payload::text FROM outbox
		WHERE topic <> 'rt' AND (payload->>'buyerOrgId' = $1 OR payload->>'orgId' = $1 OR aggregate_id = ANY(SELECT auction_id::text FROM org_auction_lots WHERE org_auction_id = $2))
		ORDER BY id`, orgID, aid)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[analytics.Event])
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.InsertEvents(t0(), evs); err != nil {
		t.Fatal(err)
	}
	e.server.Analytics = ch
	r = e.call(owner, "GET", "/orgs/"+orgID+"/analytics", nil)
	hist := r.Body["history"].([]any)
	codes := map[string]map[string]any{}
	for _, h := range hist {
		codes[h.(map[string]any)["code"].(string)] = h.(map[string]any)
	}
	var po map[string]any
	for c, h := range codes {
		if strings.HasPrefix(c, "TRX-") {
			po = h
		}
	}

	if r.Status != 200 || len(hist) != 3 || po == nil || po["item"] != "Lem PVAc" || po["totalIdr"] != float64(900_000) || po["via"] != "auction" ||
		codes["HST-10"]["supplier"] != "CV Analitik" || codes["HST-10"]["totalIdr"] != float64(4000) || codes["HST-9"] == nil ||
		codes["HST-10"]["month"] != time.Date(time.Now().UTC().Year(), time.Now().UTC().Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01") {
		t.Fatalf("clickhouse history: %v", hist)
	}
	auctions := r.Body["auctions"].([]any)
	var hst map[string]any
	for _, a := range auctions {
		if a.(map[string]any)["code"] == "HST-10" {
			hst = a.(map[string]any)
		}
	}
	if hst == nil || hst["bidders"] != float64(6) || hst["openingIdr"] != float64(1200) || hst["clearingIdr"] != float64(1000) {
		t.Fatalf("clickhouse auctions: %v", auctions)
	}
	var spend float64
	for _, m := range r.Body["savings"].([]any) {
		spend += m.(map[string]any)["spendIdr"].(float64)
	}
	if spend != 900_000+4000+3000 {
		t.Fatalf("clickhouse spend: %v", r.Body["savings"])
	}
}

func orgAuctionBody(title, withdraw string, lots ...[3]any) map[string]any {
	ls := []any{}
	for _, l := range lots {
		ls = append(ls, map[string]any{"item": l[0], "quantity": map[string]any{"value": l[1], "unit": "pcs"}, "spec": "", "reservePriceIdr": l[2]})
	}
	return map[string]any{"title": title, "categoryId": "packaging", "type": "reverse", "objective": "procurement", "multiLot": len(lots) > 1, "lots": ls,
		"rules": map[string]any{"minStepIdr": 10, "visibility": "full", "autoExtension": false, "withdraw": withdraw, "award": "lowest",
			"weights": map[string]any{"price": 60, "quality": 20, "delivery": 10, "reliability": 10}},
		"qualification": map[string]any{"documents": []string{}, "minRating": 0, "regions": []string{}}, "invited": []string{},
		"schedule": map[string]any{"durationMinutes": 120}}
}

func TestWithdrawBlock(t *testing.T) {
	now := time.Now()
	lot := func(rule string, left time.Duration) auctionRow {
		r := auctionRow{Status: "live", EndsAt: now.Add(left)}
		if rule != "" {
			r.WithdrawRule = &rule
		}
		return r
	}
	for _, c := range []struct {
		r      auctionRow
		status string
		want   string
	}{
		{lot("", time.Hour), "outbid", ""},
		{lot("", 20*time.Minute), "outbid", "Bid terdepan atau 30 menit terakhir tidak bisa ditarik"},
		{lot("", time.Hour), "leading", "Bid terdepan atau 30 menit terakhir tidak bisa ditarik"},
		{lot("before_last_30", 20*time.Minute), "submitted", "Bid terdepan atau 30 menit terakhir tidak bisa ditarik"},
		{lot("anytime", time.Minute), "outbid", ""},
		{lot("anytime", time.Hour), "leading", "Bid terdepan tidak bisa ditarik"},
		{lot("never", time.Hour), "outbid", "Bid di auction ini mengikat, tidak bisa ditarik"},
		{lot("anytime", time.Hour), "withdrawn", "Bid sudah ditarik"},
		{auctionRow{Status: "closed", EndsAt: now}, "outbid", "Auction tidak sedang berjalan"},
	} {
		if got := withdrawBlock(c.r, c.status, now); got != c.want {
			t.Errorf("%v %s: %q want %q", deref(c.r.WithdrawRule), c.status, got, c.want)
		}
	}
}

func TestOrgLotRoomWithdrawCapacityAndClose(t *testing.T) {
	e := newEnv(t)
	owner, ownerID, orgID := e.workspace("Kapasitas")
	base := "/orgs/" + orgID
	proc, _ := e.member(orgID, "procurement")
	sales, salesID := e.member(orgID, "sales")
	e.exec(`INSERT INTO org_roles (org_id, key, label, permissions) VALUES ($1, 'custom-gudang', 'Gudang', '{inventory.view}')`, orgID)
	gudang, gudangID := e.member(orgID, "custom-gudang")

	r := e.call(proc, "POST", base+"/auctions", orgAuctionBody("Box mengikat", "never", [3]any{"Box A", 1000, 1000}, [3]any{"Box B", 500, 1000}))
	want(t, r, 201, "")
	aid := r.Body["id"].(string)
	lot1 := r.Body["live"].([]any)[0].(map[string]any)["auctionId"].(string)
	lot2 := r.Body["live"].([]any)[1].(map[string]any)["auctionId"].(string)

	evaluate := "/org/" + orgID + "/auctions/" + aid + "/evaluate"
	for _, c := range []*http.Client{owner, sales} {
		if me := e.call(c, "GET", "/auctions/"+lot1+"/me", nil).Body; me["owner"] != true || me["evaluateHref"] != evaluate {
			t.Fatalf("org member view: %v", me)
		}
	}
	b1, b1ID := e.bidder("Supplier Kapasitas")
	b2, _ := e.bidder("Supplier Penuh")
	for _, c := range []*http.Client{gudang, b1} {
		if me := e.call(c, "GET", "/auctions/"+lot1+"/me", nil).Body; me["owner"] != false || me["evaluateHref"] != nil {
			t.Fatalf("bidder view: %v", me)
		}
	}

	want(t, e.call(owner, "GET", "/auctions/"+lot1+"/evaluation", nil), 404, "not_found")

	rules := e.call(e.client(), "GET", "/auctions/"+lot1, nil).Body["rules"].([]any)
	if !slices.ContainsFunc(rules, func(x any) bool {
		m := x.(map[string]any)
		return m["label"] == "Penarikan bid" && m["value"] == "Bid mengikat, tidak bisa ditarik"
	}) {
		t.Fatalf("rules: %v", rules)
	}

	for _, c := range []*http.Client{b1, b2} {
		e.qualify(c, lot1)
		e.qualify(c, lot2)
	}
	r = e.call(b1, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 950, "quantity": 1500})
	if r.Status != 422 || r.field("quantity") == "" {
		t.Fatalf("capacity above lot: %v", r.Body)
	}
	want(t, e.call(b1, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 950, "quantity": 0}), 422, "validation")
	r = e.call(b1, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 950, "quantity": 600})
	if r.Status != 200 || r.Body["capacity"].(map[string]any)["value"] != float64(600) || r.Body["capacity"].(map[string]any)["unit"] != "pcs" {
		t.Fatalf("bid with capacity: %v", r.Body)
	}
	r = e.call(b2, "POST", "/auctions/"+lot1+"/bids", map[string]any{"priceIdr": 900})
	if r.Status != 200 || r.Body["capacity"].(map[string]any)["value"] != float64(1000) {
		t.Fatalf("default capacity: %v", r.Body)
	}
	want(t, e.call(b1, "POST", "/auctions/"+lot2+"/bids", map[string]any{"priceIdr": 990}), 200, "")
	pub, _ := json.Marshal(e.call(b2, "GET", "/auctions/"+lot1, nil).Body)
	frames, _ := json.Marshal(e.frames("auction:" + lot1))
	if strings.Contains(string(pub), "capacity") || strings.Contains(string(frames), "capacity") || strings.Contains(string(frames), "600") {
		t.Fatalf("capacity leaked to the room: %s %s", pub, frames)
	}

	me := e.call(b1, "GET", "/auctions/"+lot1+"/me", nil).Body["bid"].(map[string]any)
	if me["status"] != "outbid" || me["canWithdraw"] != false {
		t.Fatalf("binding bid: %v", me)
	}
	r = e.call(b1, "DELETE", "/auctions/"+lot1+"/bids/mine", nil)
	if r.Status != 409 || r.code() != "cannot_withdraw" || r.message() != "Bid di auction ini mengikat, tidak bisa ditarik" {
		t.Fatalf("withdraw never: %d %v", r.Status, r.Body)
	}

	title := "Box mengikat ditutup"
	count := func(userID string) int {
		return len(slices.DeleteFunc(e.notifications(userID), func(s string) bool { return s != title }))
	}
	e.closeAuction(lot1)
	if count(ownerID) != 0 {
		t.Fatal("notified before the last lot closed")
	}
	e.closeAuction(lot2)
	if err := e.server.AuctionTick(t0()); err != nil {
		t.Fatal(err)
	}
	if count(ownerID) != 1 || count(salesID) != 1 || count(gudangID) != 0 || count(b1ID) != 0 {
		t.Fatalf("close notifications: owner %d sales %d gudang %d bidder %d", count(ownerID), count(salesID), count(gudangID), count(b1ID))
	}
	var body, href string
	if err := e.db.Primary().QueryRow(t0(), `SELECT body, href FROM notifications WHERE user_id = $1 AND title = $2`, ownerID, title).Scan(&body, &href); err != nil {
		t.Fatal(err)
	}
	if href != evaluate || body != "Lot 1 Box A ditutup, 2 penawaran. Lot 2 Box B ditutup, 1 penawaran. Evaluasi dan tetapkan pemenang." {
		t.Fatalf("notification: %q %q", body, href)
	}

	lots := e.call(owner, "GET", base+"/auctions/"+aid+"/evaluation", nil).Body["lots"].([]any)
	offers := lots[0].(map[string]any)["offers"].([]any)
	var capped, full map[string]any
	for _, o := range offers {
		if m := o.(map[string]any); m["priceIdr"] == float64(950) {
			capped = m
		} else {
			full = m
		}
	}
	if capped["capacity"].(map[string]any)["value"] != float64(600) || full["capacity"].(map[string]any)["value"] != float64(1000) {
		t.Fatalf("offers capacity: %v", offers)
	}
	lot2Offer := lots[1].(map[string]any)["offers"].([]any)[0].(map[string]any)["id"]
	line := func(id any, q float64) map[string]any {
		return map[string]any{"offerId": id, "supplier": "x", "quantity": q, "priceIdr": 1}
	}
	r = e.call(owner, "POST", base+"/auctions/"+aid+"/award", map[string]any{"reason": "Split", "lines": []any{
		[]any{line(full["id"], 300), line(capped["id"], 700)}, []any{line(lot2Offer, 500)}}})
	if r.Status != 422 || r.field("lines.0.1") != "Melebihi kapasitas penawaran" {
		t.Fatalf("award above capacity: %v", r.Body)
	}
	r = e.call(owner, "POST", base+"/auctions/"+aid+"/award", map[string]any{"reason": "Split", "lines": []any{
		[]any{line(full["id"], 400), line(capped["id"], 600)}, []any{line(lot2Offer, 500)}}})
	want(t, r, 200, "")
}

func TestOrgApprovalOwnerSignsForMissingRole(t *testing.T) {
	e := newEnv(t)
	owner, _, orgID := e.workspace("Tanpa Finance")
	base := "/orgs/" + orgID
	if a := e.call(owner, "GET", base, nil).Body["activeRoles"].([]any); len(a) != 1 || a[0] != "owner" {
		t.Fatalf("activeRoles: %v", a)
	}
	r := e.call(owner, "POST", base+"/auctions", orgAuctionBody("Kraft besar", "before_last_30", [3]any{"Kraft", 1000, 100_000}))
	want(t, r, 201, "")
	aid := r.Body["id"].(string)
	if r.Body["status"] != "pending_approval" || fmt.Sprint(r.Body["requiredApprovers"]) != "[finance owner]" {
		t.Fatalf("pending: %v", r.Body)
	}
	if w := e.call(owner, "GET", base+"/overview", nil).Body["waiting"].([]any); len(w) != 1 {
		t.Fatalf("waiting: %v", w)
	}
	r = e.call(owner, "POST", base+"/auctions/"+aid+"/actions", map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "live" {
		t.Fatalf("owner approves for both: %v", r.Body)
	}
	byRole := map[string]string{}
	for _, a := range r.Body["approvals"].([]any) {
		m := a.(map[string]any)
		byRole[m["role"].(string)] = m["by"].(string)
	}
	if !strings.HasSuffix(byRole["finance"], " · Owner (atas nama Finance)") || !strings.HasSuffix(byRole["owner"], " (Owner)") {
		t.Fatalf("approval history: %v", byRole)
	}

	newReq := func() string {
		return e.call(owner, "POST", base+"/procurement", map[string]any{"need": "Kraft", "categoryId": "packaging", "quantity": map[string]any{"value": 10, "unit": "ton"},
			"budgetIdr": 60_000_000, "deadline": time.Now().Add(240 * time.Hour), "spec": "", "deliveryLocation": "", "visibility": "public",
			"invitedSupplierIds": []string{}, "submit": true}).Body["id"].(string)
	}
	pr := newReq()
	r = e.call(owner, "POST", base+"/procurement/"+pr+"/actions", map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "approved" || len(r.Body["approvals"].([]any)) != 2 {
		t.Fatalf("procurement: %v", r.Body)
	}
	pr = newReq()
	r = e.call(owner, "POST", base+"/procurement/"+pr+"/actions", map[string]any{"action": "reject", "note": "Tunda"})
	if r.Status != 200 || r.Body["status"] != "rejected" || len(r.Body["approvals"].([]any)) != 1 || r.Body["approvals"].([]any)[0].(map[string]any)["role"] != "owner" {
		t.Fatalf("reject: %v", r.Body)
	}

	finance, _ := e.member(orgID, "finance")
	pr = newReq()
	r = e.call(owner, "POST", base+"/procurement/"+pr+"/actions", map[string]any{"action": "approve"})
	if r.Status != 200 || r.Body["status"] != "pending_approval" || len(r.Body["approvals"].([]any)) != 1 {
		t.Fatalf("staffed finance: %v", r.Body)
	}
	want(t, e.call(finance, "POST", base+"/procurement/"+pr+"/actions", map[string]any{"action": "approve"}), 200, "")
}
