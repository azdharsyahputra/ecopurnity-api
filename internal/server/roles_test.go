package server

import (
	"strings"
	"testing"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func TestRolesDomain(t *testing.T) {
	ok := api.MmApplicationInput{Organization: "Koperasi Tani Maju", Categories: []api.CategoryId{"agri"},
		Experience: "Lima tahun mengagregasi panen 200 petani kopi Garut."}
	if f := mmApplicationErrors(ok); len(f) != 0 {
		t.Errorf("valid application: %v", f)
	}
	if f := mmApplicationErrors(api.MmApplicationInput{Organization: " ", Experience: "singkat"}); len(f) != 3 {
		t.Errorf("invalid application: %v", f)
	}
	if mmApplyBlocked(true, "") == "" || mmApplyBlocked(false, "pending") == "" || mmApplyBlocked(false, "rejected") != "" || mmApplyBlocked(false, "") != "" {
		t.Error("mmApplyBlocked: market makers and pending applicants are blocked, rejected may reapply")
	}
	org := api.NewOrgInput{Name: "CV Maju Jaya", Type: "CV", CategoryId: "packaging"}
	for npwp, valid := range map[string]bool{"": true, "01.234.567.8-901.000": true, "3201234567890001": true, "1234": false} {
		org.Npwp = &npwp
		if got := newOrgErrors(org, nil)["npwp"] == ""; got != valid {
			t.Errorf("npwp %q valid = %v", npwp, got)
		}
	}
	org.Npwp = nil
	if newOrgErrors(api.NewOrgInput{Name: "ab", Type: "CV", CategoryId: "agri"}, nil)["name"] == "" || newOrgErrors(org, []string{"cv maju jaya"})["name"] == "" {
		t.Error("short or duplicate names are rejected")
	}
}

func TestMarketMakerApplication(t *testing.T) {
	e := newEnv(t)
	a, adminID := e.admin("Sari Admin")
	c, email := e.signedIn("Dimas Calon")
	id := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)

	if r := e.call(c, "GET", "/me/mm-application", nil); r.Status != 200 || r.Body != nil {
		t.Fatalf("no application yet: %d %v", r.Status, r.Body)
	}
	in := map[string]any{"organization": "Koperasi Tani Maju", "categories": []string{"agri"}, "experience": "singkat", "documents": ""}
	if r := e.call(c, "POST", "/me/mm-application", in); r.Status != 422 || r.field("experience") == "" {
		t.Fatalf("short experience: %d %v", r.Status, r.Body)
	}
	in["experience"] = "Lima tahun mengagregasi panen 200 petani kopi Garut."
	r := e.call(c, "POST", "/me/mm-application", in)
	if r.Status != 201 || r.Body["status"] != "pending" || r.Body["applicant"] != "Dimas Calon" {
		t.Fatalf("apply: %d %v", r.Status, r.Body)
	}
	appID := r.Body["id"].(string)
	if !e.notified(adminID, "Pengajuan Market Maker baru") {
		t.Fatal("admins are notified")
	}
	if r := e.call(c, "POST", "/me/mm-application", in); r.Status != 409 || r.message() != "Pengajuanmu masih direview" {
		t.Fatalf("apply twice: %d %v", r.Status, r.Body)
	}
	if l := e.list(a, "/admin/mm-applications"); find(l, "id", appID) == nil || l[0]["status"] != "pending" {
		t.Fatalf("queue: %v", l)
	}
	path := "/admin/mm-applications/" + appID + "/actions"
	if r := e.call(c, "POST", path, map[string]any{"action": "approve"}); r.Status != 403 {
		t.Fatalf("non-admin decides: %d", r.Status)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "reject", "reason": "pendek"}); r.Status != 422 || r.code() != "reason_required" {
		t.Fatalf("reject short: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "reject", "reason": "Pengalaman agregasi belum cukup"}); r.Status != 200 || r.Body["status"] != "rejected" {
		t.Fatalf("reject: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "approve"}); r.Status != 409 {
		t.Fatalf("decide twice: %d", r.Status)
	}
	r = e.call(c, "POST", "/me/mm-application", in)
	if r.Status != 201 {
		t.Fatalf("reapply after rejection: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", "/admin/mm-applications/"+r.Body["id"].(string)+"/actions", map[string]any{"action": "approve"}); r.Status != 200 || r.Body["decision"] == nil {
		t.Fatalf("approve: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT count(*) FROM user_capabilities WHERE user_id = $1 AND capability = 'market_maker'`, id).(int64) != 1 || !e.notified(id, "Kamu sekarang Market Maker") {
		t.Fatal("approve grants market_maker and notifies")
	}
	if r := e.call(c, "GET", "/me/mm-application", nil); r.Body["status"] != "approved" {
		t.Fatalf("latest: %v", r.Body)
	}
	if r := e.call(c, "POST", "/me/mm-application", in); r.Status != 409 || r.message() != "Akunmu sudah Market Maker" {
		t.Fatalf("apply as market maker: %d %v", r.Status, r.Body)
	}
}

func TestOrgCreateAndInvitations(t *testing.T) {
	e := newEnv(t)
	owner, ownerEmail := e.signedIn("Owner Org")
	ownerID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, ownerEmail).(string)
	name := "PT Undangan " + ownerID[:8]

	if r := e.call(owner, "POST", "/orgs", map[string]any{"name": name, "type": "PT", "categoryId": "food", "npwp": "1234"}); r.Status != 422 || r.field("npwp") == "" {
		t.Fatalf("bad npwp: %d %v", r.Status, r.Body)
	}
	r := e.call(owner, "POST", "/orgs", map[string]any{"name": name, "type": "PT", "categoryId": "food", "npwp": "01.234.567.8-901.000"})
	if r.Status != 201 {
		t.Fatalf("create org: %d %v", r.Status, r.Body)
	}
	orgID := r.Body["orgId"].(string)
	orgs := r.Body["user"].(map[string]any)["orgs"].([]any)
	if len(orgs) != 1 || orgs[0].(map[string]any)["role"] != "owner" || orgs[0].(map[string]any)["verified"] != false {
		t.Fatalf("user orgs: %v", orgs)
	}
	if e.scalar(`SELECT industry || '|' || npwp FROM org_profiles WHERE org_id = $1`, orgID) != "PT · Pangan|012345678901000" ||
		e.scalar(`SELECT count(*) FROM org_roles WHERE org_id = $1`, orgID).(int64) != 5 {
		t.Fatal("org profile / built-in roles")
	}
	if r := e.call(owner, "POST", "/orgs", map[string]any{"name": strings.ToUpper(name), "type": "PT", "categoryId": "food"}); r.Status != 422 || r.field("name") == "" {
		t.Fatalf("duplicate name: %d %v", r.Status, r.Body)
	}

	member, memberEmail := e.signedIn("Mira Anggota")
	other, _ := e.signedIn("Orang Lain")
	inv := e.scalar(`INSERT INTO org_members (org_id, email, name, role, department, invited_by) VALUES ($1, $2, 'Mira', 'finance', 'Keuangan', $3) RETURNING id::text`,
		orgID, memberEmail, ownerID).(string)
	invs := e.list(member, "/me/invitations")
	if len(invs) != 1 || invs[0]["roleLabel"] != "Finance" || invs[0]["orgName"] != name || invs[0]["department"] != "Keuangan" {
		t.Fatalf("invitations: %v", invs)
	}
	if r := e.call(other, "POST", "/me/invitations/"+inv, map[string]any{"action": "accept"}); r.Status != 404 {
		t.Fatalf("someone else's invitation: %d", r.Status)
	}
	r = e.call(member, "POST", "/me/invitations/"+inv, map[string]any{"action": "accept"})
	if r.Status != 200 || len(r.Body["orgs"].([]any)) != 1 || r.Body["orgs"].([]any)[0].(map[string]any)["role"] != "finance" {
		t.Fatalf("accept: %d %v", r.Status, r.Body)
	}
	if !e.notified(ownerID, "Mira Anggota bergabung ke "+name) {
		t.Fatal("owners are notified")
	}
	if r := e.call(member, "POST", "/me/invitations/"+inv, map[string]any{"action": "decline"}); r.Status != 404 {
		t.Fatalf("answer twice: %d", r.Status)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE org_id = $1 AND action IN ('Buat organisasi', 'Terima undangan')`, orgID).(int64); n != 2 {
		t.Fatalf("org audit: %d", n)
	}

	inv2 := e.scalar(`INSERT INTO org_members (org_id, email, name, role) VALUES ($1, $2, 'X', 'sales') RETURNING id::text`, orgID, strings.ToUpper(e.scalar(`SELECT email FROM users WHERE name = 'Orang Lain' ORDER BY created_at DESC LIMIT 1`).(string))).(string)
	if r := e.call(other, "POST", "/me/invitations/"+inv2, map[string]any{"action": "decline"}); r.Status != 200 {
		t.Fatalf("decline: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT count(*) FROM org_members WHERE id = $1`, inv2).(int64) != 0 {
		t.Fatal("decline deletes the invitation")
	}
}
