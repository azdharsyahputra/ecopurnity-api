package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// admin registers a user with the admin capability.
func (e *testEnv) admin(name string) (*http.Client, string) {
	e.t.Helper()
	c, email := e.signedIn(name)
	id := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
	e.exec(`INSERT INTO user_capabilities (user_id, capability) VALUES ($1, 'admin')`, id)
	return c, id
}

// list calls an endpoint that answers a JSON array.
func (e *testEnv) list(c *http.Client, path string) []map[string]any {
	e.t.Helper()
	res, err := c.Get(e.srv.URL + BasePath + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out []map[string]any
	if res.StatusCode != 200 || json.Unmarshal(raw, &out) != nil {
		e.t.Fatalf("GET %s: %d %s", path, res.StatusCode, raw)
	}
	return out
}

func find(items []map[string]any, key, value string) map[string]any {
	for _, it := range items {
		if it[key] == value {
			return it
		}
	}
	return nil
}

func (e *testEnv) notified(userID, title string) bool {
	return e.scalar(`SELECT count(*) FROM notifications WHERE user_id = $1 AND title = $2`, userID, title).(int64) > 0
}

const punitive = "Pelanggaran aturan escrow berulang"

func TestAdminGuard(t *testing.T) {
	e := newEnv(t)
	if r := e.call(e.client(), "GET", "/admin/overview", nil); r.Status != 401 {
		t.Fatalf("signed out: %d", r.Status)
	}
	c, _ := e.signedIn("Biasa")
	if r := e.call(c, "GET", "/admin/overview", nil); r.Status != 403 || r.code() != "forbidden" {
		t.Fatalf("non-admin: %d %v", r.Status, r.Body)
	}
	a, _ := e.admin("Sari Admin")
	if r := e.call(a, "GET", "/admin/overview", nil); r.Status != 200 || len(r.Body["sla"].([]any)) != 3 {
		t.Fatalf("overview: %d %v", r.Status, r.Body)
	}
}

func TestAdminUserGovernance(t *testing.T) {
	e := newEnv(t)
	a, _ := e.admin("Sari Admin")
	target, email := e.signedIn("Tono Target")
	id := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
	path := "/admin/users/" + id + "/actions"

	if u := e.list(a, "/admin/users?q="+strings.Split(email, "@")[0]); len(u) != 1 || u[0]["status"] != "active" || u[0]["kind"] != "person" {
		t.Fatalf("list: %v", u)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "suspend", "reason": "  pendek "}); r.Status != 422 || r.code() != "reason_required" || r.field("reason") == "" {
		t.Fatalf("short reason: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "explode", "reason": punitive}); r.Status != 422 || r.code() != "validation" {
		t.Fatalf("unknown action: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", "/admin/users/00000000-0000-0000-0000-000000000000/actions", map[string]any{"action": "suspend"}); r.Status != 404 {
		t.Fatalf("missing user (404 before reason): %d", r.Status)
	}

	// Suspend: episode starts, sessions die, login refused.
	if r := e.call(a, "POST", path, map[string]any{"action": "suspend", "reason": punitive}); r.Status != 200 || r.Body["status"] != "suspended" {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT suspended_at IS NOT NULL FROM users WHERE id = $1`, id) != true || e.scalar(`SELECT count(*) FROM sessions WHERE user_id = $1`, id).(int64) != 0 {
		t.Fatal("suspend must set suspended_at and delete sessions")
	}
	if r := e.call(target, "GET", "/me/kyc", nil); r.Status != 401 {
		t.Fatalf("old session after suspend: %d", r.Status)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "suspend", "reason": punitive}); r.Status != 409 || r.code() != "no_change" {
		t.Fatalf("suspend twice: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "deny_appeal", "reason": punitive}); r.Status != 409 || r.code() != "no_appeal" {
		t.Fatalf("deny without appeal: %d %v", r.Status, r.Body)
	}
	appeal := map[string]any{"email": email, "password": "rahasia123", "reason": "Saya tidak pernah meminta transfer di luar escrow, mohon ditinjau."}
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 200 {
		t.Fatalf("appeal: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "deny_appeal", "reason": punitive}); r.Status != 200 || r.Body["status"] != "suspended" {
		t.Fatalf("deny appeal: %d %v", r.Status, r.Body)
	}
	d := e.call(a, "GET", "/admin/users/"+id, nil)
	if ap := d.Body["appeal"].(map[string]any); ap["status"] != "denied" || ap["decision"].(map[string]any)["note"] != punitive {
		t.Fatalf("appeal after deny: %v", ap)
	}

	// Restore, suspend again (new episode), appeal, restore grants it.
	if r := e.call(a, "POST", path, map[string]any{"action": "restore", "reason": punitive}); r.Status != 200 || r.Body["status"] != "active" {
		t.Fatalf("restore: %d %v", r.Status, r.Body)
	}
	e.call(a, "POST", path, map[string]any{"action": "suspend", "reason": punitive})
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 200 {
		t.Fatalf("appeal second episode: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "restore", "reason": punitive}); r.Status != 200 {
		t.Fatalf("restore with appeal: %d %v", r.Status, r.Body)
	}
	if d := e.call(a, "GET", "/admin/users/"+id, nil); d.Body["appeal"].(map[string]any)["status"] != "granted" {
		t.Fatalf("appeal after restore: %v", d.Body["appeal"])
	}
	if r := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": email, "password": "rahasia123"}); r.Status != 200 {
		t.Fatalf("login after restore: %d %v", r.Status, r.Body)
	}

	// Verify.
	if r := e.call(a, "POST", path, map[string]any{"action": "verify", "reason": "Dokumen identitas dicek manual"}); r.Status != 200 || r.Body["verified"] != true {
		t.Fatalf("verify: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "verify", "reason": "Dokumen identitas dicek manual"}); r.Status != 409 || r.code() != "already_verified" {
		t.Fatalf("verify twice: %d %v", r.Status, r.Body)
	}
	d = e.call(a, "GET", "/admin/users/"+id, nil)
	audit := d.Body["audit"].([]any)
	if len(audit) < 6 || audit[0].(map[string]any)["action"] != "Verifikasi identitas" || audit[0].(map[string]any)["actor"] != "Sari Admin (Admin)" {
		t.Fatalf("user audit: %v", audit)
	}
	if hist := d.Body["history"].([]any); len(hist) != 0 {
		t.Fatalf("history: %v", hist)
	}
	if entries := e.list(a, "/admin/audit?entityType=user&entityId="+id+"&q=Suspend%20akun"); len(entries) != 2 {
		t.Fatalf("audit search: %v", entries)
	}
}

func TestAdminVerifications(t *testing.T) {
	e := newEnv(t)
	a, _ := e.admin("Sari Admin")

	// Business request (an org asks for review).
	owner, email := e.signedIn("Pemilik CV")
	ownerID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
	r := e.call(owner, "POST", "/orgs", map[string]any{"name": "CV Verif " + ownerID[:6], "type": "CV", "categoryId": "packaging"})
	if r.Status != 201 {
		t.Fatalf("create org: %d %v", r.Status, r.Body)
	}
	orgID := r.Body["orgId"].(string)
	reqID := e.scalar(`INSERT INTO verification_requests (kind, org_id, submitted_by, subject_name, form) VALUES ('business', $1, $2, 'CV Verif', '[{"label":"NIB","value":"1234567890123"}]') RETURNING id::text`,
		orgID, ownerID).(string)
	e.exec(`INSERT INTO verification_documents (request_id, kind, file_name, object_key, fields) VALUES ($1, 'nib', 'nib.pdf', 'docs/nib.pdf', '[{"label":"NIB","value":"1234567890123"}]')`, reqID)

	v := find(e.list(a, "/admin/verifications"), "id", reqID)
	if v == nil || v["status"] != "pending" || v["owner"] != "Pemilik CV" || len(v["documents"].([]any)) != 1 || v["nik"] != nil {
		t.Fatalf("list: %v", v)
	}
	path := "/admin/verifications/" + reqID + "/actions"
	if r := e.call(a, "POST", path, map[string]any{"action": "reject"}); r.Status != 422 || r.code() != "reason_required" {
		t.Fatalf("reject without reason: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "approve"}); r.Status != 200 || r.Body["status"] != "approved" ||
		r.Body["decision"].(map[string]any)["note"] != "Dokumen sesuai" {
		t.Fatalf("approve: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT verification FROM org_profiles WHERE org_id = $1`, orgID) != "verified" || !e.notified(ownerID, "Verifikasi CV Verif: disetujui") {
		t.Fatal("approve must verify the org and notify the submitter")
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "reupload", "reason": "Foto dokumen buram sekali"}); r.Status != 409 || r.code() != "decided" {
		t.Fatalf("decide twice: %d %v", r.Status, r.Body)
	}

	// Personal request (KTP + selfie through presigned uploads).
	if testStorage() == nil {
		t.Skip("object storage not running")
	}
	user, uemail := e.signedIn("Rina KTP")
	userID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, uemail).(string)
	ktp := e.upload(user, "kyc_ktp", "image/png", pngBytes(t))
	selfie := e.upload(user, "kyc_selfie", "image/jpeg", jpegBytes(t))
	nik := fmt.Sprintf("3205%012d", time.Now().UnixNano()%1_000_000_000_000)
	if r := e.call(user, "POST", "/me/kyc/identity", map[string]any{"nik": nik, "fullName": "Rina Wulandari", "ktpUploadId": ktp, "selfieUploadId": selfie}); r.Status != 200 {
		t.Fatalf("submit ktp: %d %v", r.Status, r.Body)
	}
	pid := e.scalar(`SELECT id::text FROM verification_requests WHERE submitted_by = $1`, userID).(string)
	if v := find(e.list(a, "/admin/verifications"), "id", pid); v == nil || v["nik"] != nil {
		t.Fatalf("lists never carry the NIK: %v", v)
	}
	d := e.call(a, "GET", "/admin/verifications/"+pid, nil)
	docs := d.Body["documents"].([]any)
	if d.Body["nik"] != nik || d.Body["kind"] != "personal" || len(docs) != 2 || !strings.HasPrefix(docs[0].(map[string]any)["url"].(string), "http") {
		t.Fatalf("personal detail: %v", d.Body)
	}
	if e.scalar(`SELECT count(*) FROM audit_log WHERE action = 'Melihat NIK' AND entity_id = $1`, userID) != int64(1) {
		t.Fatal("viewing a decrypted NIK must be audited")
	}
	res, err := http.Get(docs[0].(map[string]any)["url"].(string))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("presigned GET: %v %v", err, res)
	}
	res.Body.Close()
	if r := e.call(a, "POST", "/admin/verifications/"+pid+"/actions", map[string]any{"action": "approve", "reason": "KTP dan selfie cocok"}); r.Status != 200 {
		t.Fatalf("approve personal: %d %v", r.Status, r.Body)
	}
	if k := e.call(user, "GET", "/me/kyc", nil); k.Body["level"] != float64(1) {
		t.Fatalf("kyc after approve: %v", k.Body)
	}
	if e.scalar(`SELECT nik_hash IS NOT NULL AND identity_verified_by IS NOT NULL FROM identities WHERE user_id = $1`, userID) != true {
		t.Fatal("approve must store the NIK hash and the verifier")
	}
	if u := e.call(a, "GET", "/admin/users/"+userID, nil); u.Body["verified"] != true || u.Body["audit"].([]any)[0].(map[string]any)["action"] != "Setujui verifikasi KTP" {
		t.Fatalf("user after KTP approve: %v", u.Body)
	}
}

func TestAdminMarketsAndAuctions(t *testing.T) {
	e := newEnv(t)
	a, _ := e.admin("Sari Admin")
	m := e.seedMarket("Moderasi "+time.Now().Format("150405.000"), "agri", "kg", "active", "auto")
	path := "/admin/markets/" + m + "/actions"

	if r := e.call(a, "POST", path, map[string]any{"action": "flag", "reason": "Harga tidak wajar di market ini"}); r.Status != 200 || r.Body["ok"] != true {
		t.Fatalf("flag: %d %v", r.Status, r.Body)
	}
	if mk := find(e.list(a, "/admin/markets"), "id", m); mk == nil || len(mk["flags"].([]any)) != 1 || mk["flags"].([]any)[0].(map[string]any)["by"] != "Sari Admin (Admin)" {
		t.Fatalf("flags: %v", mk)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "review"}); r.Status != 200 {
		t.Fatalf("review: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "restore", "reason": punitive}); r.Status != 409 || r.code() != "no_change" {
		t.Fatalf("restore active: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "suspend", "reason": punitive}); r.Status != 200 {
		t.Fatalf("suspend market: %d %v", r.Status, r.Body)
	}
	d := e.call(a, "GET", "/admin/markets/"+m, nil)
	if d.Body["status"] != "suspended" || d.Body["reviewedAt"] == nil || len(d.Body["audit"].([]any)) != 3 {
		t.Fatalf("market detail: %v", d.Body)
	}

	// Auctions: freeze holds a live buyer auction, unfreeze restores it.
	_, auc, _ := e.buyerAuction("full", "reverse")
	buyerID := e.scalar(`SELECT owner_user_id::text FROM auctions WHERE id = $1`, auc).(string)
	s1, _ := e.bidder("Supplier Satu")
	s2, _ := e.bidder("Supplier Dua")
	e.qualify(s1, auc)
	e.qualify(s2, auc)
	for _, p := range []int{9500, 9000, 8500} { // one bidder three times in a row: "Bid beruntun"
		if r := e.call(s1, "POST", "/auctions/"+auc+"/bids", map[string]any{"priceIdr": p}); r.Status != 200 {
			t.Fatalf("bid %d: %d %v", p, r.Status, r.Body)
		}
	}
	ad := e.call(a, "GET", "/admin/auctions/"+auc, nil)
	if bids := ad.Body["bids"].([]any); len(bids) != 3 || !strings.Contains(bids[0].(map[string]any)["bidder"].(string), "Supplier Satu") ||
		bids[0].(map[string]any)["masked"] != "Supplier 1" {
		t.Fatalf("ledger: %v", ad.Body["bids"])
	}
	if f := ad.Body["findings"].([]any); len(f) != 1 || f[0].(map[string]any)["rule"] != "Bid beruntun" {
		t.Fatalf("findings: %v", f)
	}
	act := "/admin/auctions/" + auc + "/actions"
	if r := e.call(a, "POST", act, map[string]any{"action": "unfreeze", "reason": punitive}); r.Status != 409 || r.code() != "not_frozen" {
		t.Fatalf("unfreeze live: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", act, map[string]any{"action": "freeze", "reason": punitive}); r.Status != 200 || r.Body["ok"] != true {
		t.Fatalf("freeze: %d %v", r.Status, r.Body)
	}
	title := e.scalar(`SELECT title FROM auctions WHERE id = $1`, auc).(string)
	if e.scalar(`SELECT status || '/' || frozen_from FROM auctions WHERE id = $1`, auc) != "frozen/live" || !e.notified(buyerID, title+" dibekukan Admin") {
		t.Fatal("freeze must keep frozen_from and notify the owner")
	}
	frames := e.frames("auction:" + auc)
	if last := frames[len(frames)-1]; last["type"] != "auction.state" || last["payload"].(map[string]any)["status"] != "frozen" {
		t.Fatalf("freeze frame: %v", last)
	}
	if r := e.call(s2, "POST", "/auctions/"+auc+"/bids", map[string]any{"priceIdr": 8000}); r.Status < 400 {
		t.Fatalf("bid on frozen auction accepted: %d", r.Status)
	}
	if r := e.call(a, "POST", act, map[string]any{"action": "freeze", "reason": punitive}); r.Status != 409 || r.code() != "not_active" {
		t.Fatalf("freeze twice: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", act, map[string]any{"action": "unfreeze", "reason": punitive}); r.Status != 200 {
		t.Fatalf("unfreeze: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT status FROM auctions WHERE id = $1`, auc) != "live" {
		t.Fatal("unfreeze must restore live")
	}

	// Open a case, then work the alert: note, escalate to a freeze.
	r := e.call(a, "POST", act, map[string]any{"action": "open_case", "reason": "Pola bid beruntun mencurigakan", "type": "abnormal_bidding"})
	caseID, _ := r.Body["caseId"].(string)
	if r.Status != 200 || caseID == "" {
		t.Fatalf("open case: %d %v", r.Status, r.Body)
	}
	if ad := e.call(a, "GET", "/admin/auctions/"+auc, nil); ad.Body["caseId"] != caseID || len(ad.Body["findings"].([]any)) != 2 {
		t.Fatalf("auction with case: %v", ad.Body)
	}
	al := e.call(a, "GET", "/admin/alerts/"+caseID, nil)
	inv := al.Body["investigation"].(map[string]any)
	if al.Body["status"] != "investigating" || al.Body["source"] != "manual" || len(inv["notes"].([]any)) != 1 || len(al.Body["evidence"].([]any)) != 1 {
		t.Fatalf("case: %v", al.Body)
	}
	alertPath := "/admin/alerts/" + caseID + "/actions"
	if r := e.call(a, "POST", alertPath, map[string]any{"action": "investigate"}); r.Status != 409 || r.code() != "invalid_state" {
		t.Fatalf("investigate twice: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", alertPath, map[string]any{"action": "note", "text": "  "}); r.Status != 422 || r.field("text") == "" {
		t.Fatalf("empty note: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", alertPath, map[string]any{"action": "note", "text": "Cek IP bidder"}); r.Status != 200 {
		t.Fatalf("note: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", alertPath, map[string]any{"action": "escalate", "subjectId": auc, "escalation": "suspend_user", "reason": punitive}); r.Status != 422 || r.field("subjectId") == "" {
		t.Fatalf("escalation mismatch: %d %v", r.Status, r.Body)
	}
	r = e.call(a, "POST", alertPath, map[string]any{"action": "escalate", "subjectId": auc, "escalation": "freeze_auction", "reason": punitive})
	if r.Status != 200 || r.Body["status"] != "escalated" || !strings.HasPrefix(r.Body["resolution"].(map[string]any)["outcome"].(string), "Freeze ") {
		t.Fatalf("escalate: %d %v", r.Status, r.Body)
	}
	if e.scalar(`SELECT status FROM auctions WHERE id = $1`, auc) != "frozen" {
		t.Fatal("escalation must freeze the auction")
	}
	al = e.call(a, "GET", "/admin/alerts/"+caseID, nil)
	var actions []string
	for _, x := range al.Body["audit"].([]any) {
		actions = append(actions, x.(map[string]any)["action"].(string))
	}
	joined := strings.Join(actions, "|")
	if !strings.Contains(joined, "Buka kasus") || !strings.Contains(joined, "Freeze auction (eskalasi ") || !strings.Contains(joined, "Catatan ") {
		t.Fatalf("case history: %v", actions)
	}
	if r := e.call(a, "POST", alertPath, map[string]any{"action": "close", "reason": punitive}); r.Status != 409 || r.code() != "no_case" {
		t.Fatalf("close escalated: %d %v", r.Status, r.Body)
	}
}

func TestAdminDisputes(t *testing.T) {
	e := newEnv(t)
	a, _ := e.admin("Sari Admin")
	buyer, buyerID := e.bidder("Budi Pembeli")
	supplier, supplierID := e.bidder("Sinta Supplier")
	var bp, sp string
	var err error
	if bp, err = userParty(t0(), e.db.Primary(), buyerID); err != nil {
		t.Fatal(err)
	}
	if sp, err = userParty(t0(), e.db.Primary(), supplierID); err != nil {
		t.Fatal(err)
	}
	tr, err := createTrade(t0(), e.db.Primary(), newTrade{Title: "Beras 1 ton", BuyerParty: bp, SupplierParty: sp, Quantity: 1000, Unit: "kg",
		UnitPriceIdr: 10000, DeliveryAddress: "Bandung", Via: "direct", Category: "food", Region: "Jawa Barat"})
	if err != nil {
		t.Fatal(err)
	}
	// Through the real flow: paid into escrow, then the buyer opens the dispute (its reason is the first evidence).
	e.mustAct(buyer, tr.ID, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, tr.ID, map[string]any{"action": "accept_agreement"}, "agreement")
	e.mustAct(supplier, tr.ID, map[string]any{"action": "issue_invoice"}, "invoiced")
	e.payViaGateway(buyer, "/me/transactions/"+tr.ID, "paid")
	e.mustAct(buyer, tr.ID, map[string]any{"action": "dispute", "note": "Barang tidak sesuai"}, "disputed")
	dsp := e.scalar(`SELECT id::text FROM disputes WHERE trade_id = $1`, tr.ID).(string)

	if s := find(e.list(a, "/admin/disputes"), "id", dsp); s == nil || s["totalIdr"] != float64(10_000_000) || s["openedBy"] != "Budi Pembeli" || s["title"] != "Beras 1 ton" {
		t.Fatalf("summary: %v", s)
	}
	path := "/admin/disputes/" + dsp + "/actions"
	resolve := map[string]any{"action": "resolve", "resolution": map[string]any{"kind": "refund"}, "reason": punitive}
	if r := e.call(a, "POST", path, resolve); r.Status != 409 || r.code() != "invalid_transition" {
		t.Fatalf("resolve before review: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "request_evidence", "from": "supplier", "reason": "pendek"}); r.Status != 422 || r.code() != "reason_required" {
		t.Fatalf("short reason: %d %v", r.Status, r.Body)
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "request_evidence", "from": "supplier", "reason": "Kirim foto barang saat dikirim"}); r.Status != 200 || r.Body["status"] != "evidence" {
		t.Fatalf("request evidence: %d %v", r.Status, r.Body)
	}
	code := e.scalar(`SELECT code FROM disputes WHERE id = $1`, dsp).(string)
	if !e.notified(supplierID, code+": Admin meminta bukti") || e.notified(buyerID, code+": Admin meminta bukti") {
		t.Fatal("request_evidence must notify only the asked side")
	}
	if r := e.call(a, "POST", path, map[string]any{"action": "start_review"}); r.Status != 200 || r.Body["status"] != "review" {
		t.Fatalf("start review: %d %v", r.Status, r.Body)
	}
	partial := map[string]any{"action": "resolve", "resolution": map[string]any{"kind": "partial", "refundIdr": 10_000_000}, "reason": punitive}
	if r := e.call(a, "POST", path, partial); r.Status != 422 || r.field("refundIdr") == "" {
		t.Fatalf("partial = total: %d %v", r.Status, r.Body)
	}
	partial["resolution"] = map[string]any{"kind": "partial", "refundIdr": 2_500_000}
	r := e.call(a, "POST", path, partial)
	if r.Status != 200 || r.Body["status"] != "resolved" {
		t.Fatalf("resolve: %d %v", r.Status, r.Body)
	}
	res := r.Body["resolution"].(map[string]any)
	if res["kind"] != "partial" || res["refundIdr"] != float64(2_500_000) || res["releaseIdr"] != float64(7_500_000) || res["by"] != "Sari Admin (Admin)" {
		t.Fatalf("resolution: %v", res)
	}
	tl := r.Body["timeline"].([]any)
	if len(tl) != 4 || tl[0].(map[string]any)["label"] != "Dispute dibuka" || !strings.HasPrefix(tl[3].(map[string]any)["label"].(string), "Diputuskan: Rp 2.500.000 dikembalikan") {
		t.Fatalf("timeline: %v", tl)
	}
	tx := r.Body["transaction"].(map[string]any)
	if tx["role"] != "buyer" || tx["payment"].(map[string]any)["status"] != "released" || len(tx["dispute"].(map[string]any)["evidence"].([]any)) != 1 {
		t.Fatalf("transaction: %v", tx)
	}
	if !e.notified(buyerID, code+" diputuskan") || !e.notified(supplierID, code+" diputuskan") {
		t.Fatal("both parties are notified of the decision")
	}
	if e.scalar(`SELECT action FROM audit_log WHERE entity_type = 'dispute' AND entity_id = $1 ORDER BY id DESC LIMIT 1`, dsp) != "Putuskan dispute (refund sebagian)" {
		t.Fatal("resolve audit")
	}
	if r := e.call(a, "POST", path, resolve); r.Status != 409 {
		t.Fatalf("act on resolved: %d", r.Status)
	}
}

func TestDisputeDomain(t *testing.T) {
	for _, c := range []struct{ status, action, want string }{
		{"open", "request_evidence", "evidence"}, {"evidence", "start_review", "review"}, {"review", "resolve", "resolved"},
		{"evidence", "request_evidence", "evidence"}, {"open", "resolve", ""}, {"resolved", "start_review", ""},
	} {
		if got := disputeTransition(c.status, c.action); got != c.want {
			t.Errorf("%s + %s = %q, want %q", c.status, c.action, got, c.want)
		}
	}
	if o := resolveOutcome(1_000_000, resolution{Kind: "refund"}); o.Status != "cancelled" || o.Payment != "refunded" || o.RefundIdr != 1_000_000 || o.ReleaseIdr != 0 {
		t.Errorf("refund: %+v", o)
	}
	if o := resolveOutcome(1_000_000, resolution{Kind: "release"}); o.Status != "completed" || o.Payment != "released" || o.ReleaseIdr != 1_000_000 {
		t.Errorf("release: %+v", o)
	}
	if o := resolveOutcome(1_000_000, resolution{Kind: "partial", RefundIdr: 250_000}); o.Status != "completed" || o.ReleaseIdr != 750_000 || !strings.Contains(o.Note, "250.000") {
		t.Errorf("partial: %+v", o)
	}
	for amount, ok := range map[int64]bool{0: false, 1_000_000: false, 400_000: true} {
		if got := validateResolution(1_000_000, resolution{Kind: "partial", RefundIdr: amount}) == ""; got != ok {
			t.Errorf("partial %d valid = %v", amount, got)
		}
	}
	if validateResolution(1_000_000, resolution{Kind: "refund"}) != "" {
		t.Error("refund is always valid")
	}
}
