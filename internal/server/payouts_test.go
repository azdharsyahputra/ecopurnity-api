package server

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNormName(t *testing.T) {
	if normName(" Budi  santoso. ") != normName("BUDI SANTOSO") || normName("Budi Santosa") == normName("BUDI SANTOSO") {
		t.Fatal("normName")
	}
}

func TestManualPayouts(t *testing.T) {
	e := newEnv(t)
	adm, adminID := e.admin("Sari Admin")
	seller, email := e.signedIn("Budi Santoso")
	sellerID := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
	party := e.partyOf(sellerID)

	// 900k released earnings + 100k collected PPN in the wallet; an approved KTP named BUDI SANTOSO.
	if err := e.server.inTx(t0(), func(tx pgx.Tx) error {
		acc, err := accounts(t0(), tx, "bank_clearing", "wallet_available:"+party, "ppn_payable:"+party)
		if err != nil {
			return err
		}
		return post(t0(), tx, journal{Label: "seed", Lines: []ledgerLine{{acc["bank_clearing"], 1_000_000, "payout"},
			{acc["wallet_available:"+party], -900_000, "payout"}, {acc["ppn_payable:"+party], -100_000, "payout"}}})
	}); err != nil {
		t.Fatal(err)
	}
	vr := e.scalar(`INSERT INTO verification_requests (kind, submitted_by, subject_name, status, decided_by, decided_at, decision_note)
		VALUES ('personal', $1, 'BUDI SANTOSO', 'approved', $2, now(), 'Dokumen sesuai') RETURNING id::text`, sellerID, adminID).(string)
	e.exec(`INSERT INTO kyc_submissions (verification_request_id, nik_encrypted, nik_hash, full_name) VALUES ($1, '\x00', $2, 'BUDI SANTOSO')`,
		vr, make([]byte, 32))
	if r := e.call(seller, "PUT", "/me/finance/bank", map[string]any{"bank": "BCA", "accountNo": "1234567890", "holder": "Budi Santoso"}); r.Status != 200 {
		t.Fatal("bank", r.Status, r.Body)
	}
	wd := func(amount int) string {
		r := e.call(seller, "POST", "/me/finance/withdrawals", map[string]any{"amountIdr": amount})
		if r.Status != 201 {
			t.Fatal("withdraw", r.Status, r.Body)
		}
		return r.Body["withdrawals"].([]any)[0].(map[string]any)["id"].(string)
	}
	paidID := wd(950_000)  // 900k wallet + 50k PPN
	rejectID := wd(50_000) // the remaining PPN
	walletPPN := func() (int64, int64) {
		w, _ := balance(t0(), e.db.Primary(), party, "wallet_available")
		p, _ := balance(t0(), e.db.Primary(), party, "ppn_payable")
		return w, p
	}
	if w, p := walletPPN(); w != 0 || p != 0 {
		t.Fatal("drained", w, p)
	}

	// Admins only.
	for _, path := range []string{"/admin/withdrawals", "/admin/withdrawals/" + paidID} {
		if r := e.call(seller, "GET", path, nil); r.Status != 403 {
			t.Fatal("non-admin", path, r.Status)
		}
	}
	if r := e.call(seller, "POST", "/admin/withdrawals/"+paidID+"/actions", map[string]any{"action": "mark_paid", "transferRef": "X"}); r.Status != 403 {
		t.Fatal("non-admin action", r.Status)
	}
	if r := e.call(adm, "GET", "/admin/withdrawals/00000000-0000-0000-0000-000000000000", nil); r.Status != 404 {
		t.Fatal("404", r.Status)
	}

	// Queue: masked number, SLA hint; overview counts it.
	q := e.list(adm, "/admin/withdrawals?status=processing")
	w := find(q, "id", paidID)
	if w == nil || find(q, "id", rejectID) == nil || w["accountLast4"] != "7890" || w["accountNo"] != nil || w["bank"] != "BCA" ||
		w["requester"].(map[string]any)["name"] != "Budi Santoso" || w["party"].(map[string]any)["id"] != party || w["dueAt"] == nil {
		t.Fatalf("queue: %v", w)
	}
	if r := e.call(adm, "GET", "/admin/overview", nil); num(r.Body["queues"].(map[string]any)["withdrawals"]) < 2 || len(r.Body["sla"].([]any)) != 3 {
		t.Fatal("overview", r.Body)
	}

	// Detail: decrypted number (audited), KTP name next to the holder, the party's other withdrawals.
	views := func() int64 {
		return e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action LIKE 'Melihat nomor rekening%'`, sellerID).(int64)
	}
	r := e.call(adm, "GET", "/admin/withdrawals/"+paidID, nil)
	if r.Status != 200 || r.Body["accountNo"] != "1234567890" || r.Body["identityName"] != "BUDI SANTOSO" || r.Body["nameMismatch"] != false ||
		len(r.Body["recent"].([]any)) != 1 || views() != 1 {
		t.Fatal("detail", r.Status, r.Body, views())
	}

	// Validation, then mark paid.
	act := func(id string, body map[string]any) resp {
		return e.call(adm, "POST", "/admin/withdrawals/"+id+"/actions", body)
	}
	if r := act(paidID, map[string]any{"action": "mark_paid", "transferRef": "  "}); r.Status != 422 || r.field("transferRef") == "" {
		t.Fatal("ref required", r.Status, r.Body)
	}
	if r := act(paidID, map[string]any{"action": "mark_paid", "transferRef": "BCA-1", "paidAt": time.Now().Add(time.Hour)}); r.Status != 422 || r.field("paidAt") == "" {
		t.Fatal("future paidAt", r.Status, r.Body)
	}
	if r := act(paidID, map[string]any{"action": "reject", "reason": "pendek"}); r.Status != 422 || r.code() != "reason_required" {
		t.Fatal("reason", r.Status, r.Body)
	}
	r = act(paidID, map[string]any{"action": "mark_paid", "transferRef": " TRF-20261003-001 ", "note": "Transfer manual BCA"})
	if r.Status != 200 || r.Body["status"] != "paid" || r.Body["transferRef"] != "TRF-20261003-001" || r.Body["paidAt"] == nil ||
		r.Body["decidedBy"] != "Sari Admin (Admin)" {
		t.Fatal("mark_paid", r.Status, r.Body)
	}
	if r := act(paidID, map[string]any{"action": "reject", "reason": "Rekening tidak valid sama sekali"}); r.Status != 409 || r.code() != "decided" {
		t.Fatal("double action", r.Status, r.Body)
	}
	if w, p := walletPPN(); w != 0 || p != 0 {
		t.Fatal("paid: wallet unchanged", w, p)
	}
	if v := e.scalar(`SELECT coalesce(sum(e.amount), 0)::bigint FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.withdrawal_id = $1 AND a.kind = 'payout_pending'`, paidID).(int64); v != 0 {
		t.Fatal("payout_pending cleared", v)
	}
	if v := e.scalar(`SELECT coalesce(sum(e.amount), 0)::bigint FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.withdrawal_id = $1 AND a.kind = 'bank_clearing'`, paidID).(int64); v != -950_000 {
		t.Fatal("money left the bank", v)
	}
	if !e.notified(sellerID, "Dana Rp 950.000 sudah ditransfer ke BCA ••7890") {
		t.Fatal("paid notification")
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action LIKE 'Tandai pencairan WDR-% dibayar'`, sellerID).(int64); n != 1 {
		t.Fatal("paid audit", n)
	}
	// No full number once paid (and no new audited view).
	if r := e.call(adm, "GET", "/admin/withdrawals/"+paidID, nil); r.Body["accountNo"] != nil || views() != 1 {
		t.Fatal("paid detail", r.Body, views())
	}

	// Reject: the PPN it took is available again.
	r = act(rejectID, map[string]any{"action": "reject", "reason": "Nama pemilik rekening tidak sesuai KTP"})
	if r.Status != 200 || r.Body["status"] != "rejected" || r.Body["reason"] != "Nama pemilik rekening tidak sesuai KTP" {
		t.Fatal("reject", r.Status, r.Body)
	}
	if w, p := walletPPN(); w != 0 || p != -50_000 {
		t.Fatal("reject restores", w, p)
	}
	if r := act(rejectID, map[string]any{"action": "mark_paid", "transferRef": "X"}); r.Status != 409 {
		t.Fatal("reject then pay", r.Status)
	}
	if !e.notified(sellerID, "Pencairan Rp 50.000 ditolak") {
		t.Fatal("reject notification")
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action LIKE 'Tolak pencairan%' AND reason IS NOT NULL`, sellerID).(int64); n != 1 {
		t.Fatal("reject audit", n)
	}

	// The seller's view.
	f := e.call(seller, "GET", "/me/finance", nil).Body
	if num(f["availableIdr"]) != 50_000 || num(f["withdrawnIdr"]) != 950_000 {
		t.Fatal("finance totals", f)
	}
	ws := map[string]map[string]any{}
	for _, x := range f["withdrawals"].([]any) {
		ws[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	if ws[paidID]["status"] != "paid" || ws[paidID]["transferRef"] != "TRF-20261003-001" ||
		ws[rejectID]["status"] != "rejected" || ws[rejectID]["reason"] != "Nama pemilik rekening tidak sesuai KTP" {
		t.Fatal("finance withdrawals", ws)
	}
	if h := e.list(adm, "/admin/withdrawals?status=paid"); find(h, "id", paidID) == nil || find(h, "id", rejectID) != nil {
		t.Fatal("history filter")
	}
}
