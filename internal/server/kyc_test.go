package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func (e *testEnv) signedIn(name string) (*http.Client, string) {
	e.t.Helper()
	c := e.client()
	email := uniqueEmail(e.t, strings.ToLower(strings.ReplaceAll(name, " ", ".")))
	if r := e.call(c, "POST", "/auth/register", map[string]any{"name": name, "email": email, "password": "rahasia123"}); r.Status != 201 {
		e.t.Fatalf("register: %d %v", r.Status, r.Body)
	}
	return c, email
}

func pngBytes(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (e *testEnv) upload(c *http.Client, purpose, contentType string, data []byte) string {
	e.t.Helper()
	r := e.call(c, "POST", "/uploads", map[string]any{"purpose": purpose, "fileName": "foto." + strings.TrimPrefix(contentType, "image/"),
		"contentType": contentType, "sizeBytes": len(data)})
	if r.Status != 201 {
		e.t.Fatalf("create upload: %d %v", r.Status, r.Body)
	}
	req, _ := http.NewRequest(http.MethodPut, r.Body["url"].(string), bytes.NewReader(data))
	for k, v := range r.Body["headers"].(map[string]any) {
		req.Header.Set(k, v.(string))
	}
	req.ContentLength = int64(len(data))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		e.t.Fatalf("PUT to storage: %d", res.StatusCode)
	}
	return r.Body["uploadId"].(string)
}

func TestKycLevels(t *testing.T) {
	e := newEnv(t)
	c, _ := e.signedIn("Pita")

	r := e.call(c, "GET", "/me/kyc", nil)
	v, _ := r.Body["verification"].(map[string]any)
	if r.Status != 200 || r.Body["level"] != float64(0) || r.Body["label"] != "Email" || r.Body["limitIdr"] != float64(10_000_000) ||
		r.Body["next"] != "Verifikasi KTP untuk naik ke Rp 2 M per transaksi." || v["phone"] != nil || r.Body["otpPending"] != nil {
		t.Fatalf("initial kyc: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/me/kyc/phone", map[string]any{"phone": "081234567890"}); r.Status != 404 {
		t.Fatalf("phone endpoint still there: %d %v", r.Status, r.Body)
	}
}

func TestUploadsAndKTP(t *testing.T) {
	e := newEnv(t)
	if e.server.Storage == nil {
		t.Skip("object storage not running (docker compose up seaweedfs)")
	}
	c, email := e.signedIn("Kartika")

	if r := e.call(c, "POST", "/uploads", map[string]any{"purpose": "kyc_ktp", "fileName": "ktp.pdf", "contentType": "application/pdf", "sizeBytes": 100}); r.Status != 422 || r.field("contentType") == "" {
		t.Fatalf("type: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/uploads", map[string]any{"purpose": "kyc_ktp", "fileName": "ktp.jpg", "contentType": "image/jpeg", "sizeBytes": 9 << 20}); r.Status != 422 || r.field("sizeBytes") == "" {
		t.Fatalf("size: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "POST", "/uploads", map[string]any{"purpose": "kyc_ktp", "fileName": "a.jpg", "contentType": "image/jpeg", "sizeBytes": 10}); r.Status != 401 {
		t.Fatalf("anonymous: %d", r.Status)
	}

	ktp := e.upload(c, "kyc_ktp", "image/png", pngBytes(t))
	selfie := e.upload(c, "kyc_selfie", "image/jpeg", jpegBytes(t))

	fake := []byte("<html>bukan gambar</html>")
	disguised := e.upload(c, "kyc_ktp", "image/png", fake)

	ghost := e.call(c, "POST", "/uploads", map[string]any{"purpose": "kyc_ktp", "fileName": "a.png", "contentType": "image/png", "sizeBytes": 10}).Body["uploadId"].(string)

	submit := func(cl *http.Client, ktpID, selfieID, nik string) resp {
		return e.call(cl, "POST", "/me/kyc/identity", map[string]any{"nik": nik, "fullName": "  Kartika Sari ", "ktpUploadId": ktpID, "selfieUploadId": selfieID})
	}
	if r := submit(c, disguised, selfie, "3205010101900001"); r.Status != 422 || r.field("ktpUploadId") != "Isi file bukan gambar yang valid." {
		t.Fatalf("disguised: %d %v", r.Status, r.Body)
	}
	if r := submit(c, ghost, selfie, "3205010101900001"); r.Status != 422 || r.field("ktpUploadId") != "File belum selesai diunggah." {
		t.Fatalf("not uploaded: %d %v", r.Status, r.Body)
	}
	if r := submit(c, selfie, ktp, "3205010101900001"); r.Status != 422 || r.field("ktpUploadId") != "File ini diunggah untuk keperluan lain." {
		t.Fatalf("swapped purposes: %d %v", r.Status, r.Body)
	}
	other, _ := e.signedIn("Penyusup")
	if r := submit(other, ktp, selfie, "3205010101900001"); r.Status != 422 || r.field("ktpUploadId") != "File tidak ditemukan. Unggah ulang." {
		t.Fatalf("someone else's upload: %d %v", r.Status, r.Body)
	}

	r := submit(c, ktp, selfie, "3205010101900001")
	if r.Status != 200 || r.Body["verification"].(map[string]any)["identity"] != "pending" {
		t.Fatalf("submit: %d %v", r.Status, r.Body)
	}
	if r := submit(c, ktp, selfie, "3205010101900001"); r.Status != 409 || r.code() != "already_submitted" {
		t.Fatalf("second submit: %d %v", r.Status, r.Body)
	}

	form := e.scalar(`SELECT form::text FROM verification_requests r JOIN users u ON u.id = r.submitted_by WHERE u.email = $1`, email).(string)
	if !strings.Contains(form, "3205********0001") || strings.Contains(form, "3205010101900001") || !strings.Contains(form, "Kartika Sari") {
		t.Fatalf("form: %s", form)
	}
	if n := e.scalar(`SELECT count(*) FROM kyc_submissions k JOIN verification_requests r ON r.id = k.verification_request_id JOIN users u ON u.id = r.submitted_by
	                  WHERE u.email = $1 AND position(convert_to('3205010101900001', 'UTF8') in k.nik_encrypted) = 0`, email); n != int64(1) {
		t.Fatalf("encrypted nik: %v", n)
	}
	if n := e.scalar(`SELECT count(*) FROM verification_documents d JOIN verification_requests r ON r.id = d.request_id JOIN users u ON u.id = r.submitted_by WHERE u.email = $1`, email); n != int64(2) {
		t.Fatalf("documents: %v", n)
	}

	if n := e.scalar(`SELECT count(*) FROM uploads WHERE id::text = ANY($1) AND consumed_at IS NOT NULL`, []string{ktp, selfie}); n != int64(2) {
		t.Fatalf("consumed: %v", n)
	}

	e.exec(`UPDATE identities SET identity_verified_at = now(), identity_verified_by = user_id, nik_hash = (SELECT k.nik_hash FROM kyc_submissions k JOIN verification_requests r ON r.id = k.verification_request_id
	          WHERE r.submitted_by = identities.user_id) WHERE user_id = (SELECT id FROM users WHERE email = $1)`, email)
	e.exec(`UPDATE verification_requests SET status = 'approved', decided_at = now(), decided_by = submitted_by, decision_note = 'ok' WHERE submitted_by = (SELECT id FROM users WHERE email = $1)`, email)
	if r := e.call(c, "GET", "/me/kyc", nil); r.Body["level"] != float64(1) || r.Body["label"] != "KTP" || r.Body["limitIdr"] != float64(2_000_000_000) || r.Body["next"] != nil {
		t.Fatalf("level 1: %v", r.Body)
	}
	k2 := e.upload(other, "kyc_ktp", "image/png", pngBytes(t))
	s2 := e.upload(other, "kyc_selfie", "image/png", pngBytes(t))
	if r := submit(other, k2, s2, "3205010101900001"); r.Status != 409 || r.code() != "nik_in_use" {
		t.Fatalf("nik reuse: %d %v", r.Status, r.Body)
	}
}

func TestIdentity(t *testing.T) {
	e := newEnv(t)
	c, email := e.signedIn("Ira")
	r := e.call(c, "GET", "/me/identity", nil)
	if r.Status != 200 || r.Body["profile"].(map[string]any)["name"] != "Ira" || r.Body["completeness"] == nil {
		t.Fatalf("get: %d %v", r.Status, r.Body)
	}
	before := r.Body["completeness"].(float64)

	body := map[string]any{
		"profile": map[string]any{"name": " Ira Wijaya ", "username": "hacker", "location": "Jawa Barat", "bio": "Pengolah kopi",

			"verification": map[string]any{"email": true, "identity": "verified"}},
		"items": []any{
			map[string]any{"id": "cap-local-1", "kind": "skill", "name": "Roasting", "detail": "Mahir", "categoryId": "agri"},
			map[string]any{"id": "cap-local-2", "kind": "asset", "name": "Mesin sangrai", "detail": "5 kg"},
		},
		"availability": map[string]any{"days": []int{0, 1, 1}, "from": "07:30", "to": "16:00"},
		"preferences":  map[string]any{"locations": []string{"Jawa Barat"}, "categories": []string{"agri"}, "deliveryRadiusKm": 40, "minPriceIdr": 80000},
		"completeness": 1,
	}
	r = e.call(c, "PUT", "/me/identity", body)
	if r.Status != 200 {
		t.Fatalf("put: %d %v", r.Status, r.Body)
	}
	p := r.Body["profile"].(map[string]any)
	v := p["verification"].(map[string]any)
	if p["name"] != "Ira Wijaya" || p["username"] == "hacker" || v["identity"] != "none" {
		t.Fatalf("profile: %v", p)
	}
	items := r.Body["items"].([]any)
	if len(items) != 2 || r.Body["completeness"].(float64) <= before || r.Body["completeness"].(float64) >= 1 {
		t.Fatalf("items/completeness: %v %v", items, r.Body["completeness"])
	}
	if days := r.Body["availability"].(map[string]any)["days"].([]any); len(days) != 2 {
		t.Fatalf("days normalised: %v", days)
	}
	firstID := items[0].(map[string]any)["id"].(string)
	if me := e.call(c, "GET", "/auth/me", nil); me.Body["name"] != "Ira Wijaya" || me.Body["location"] != "Jawa Barat" {
		t.Fatalf("user not updated: %v", me.Body)
	}

	body["items"] = []any{
		map[string]any{"id": "cap-local-3", "kind": "capacity", "name": "Produksi", "detail": "2 ton/bulan"},
		map[string]any{"id": firstID, "kind": "skill", "name": "Roasting lanjutan", "detail": "Ahli"},
	}
	r = e.call(c, "PUT", "/me/identity", body)
	items = r.Body["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["id"] != firstID || items[1].(map[string]any)["name"] != "Roasting lanjutan" {
		t.Fatalf("upsert: %v", items)
	}
	if n := e.scalar(`SELECT count(*) FROM capacity_items c JOIN users u ON u.id = c.user_id WHERE u.email = $1`, email); n != int64(2) {
		t.Fatalf("rows: %v", n)
	}

	body["profile"].(map[string]any)["name"] = "  "
	if r := e.call(c, "PUT", "/me/identity", body); r.Status != 422 || r.field("name") == "" {
		t.Fatalf("blank name: %d %v", r.Status, r.Body)
	}
	body["profile"].(map[string]any)["name"] = "Ira"
	body["availability"] = map[string]any{"days": []int{1}, "from": "17:00", "to": "08:00"}
	if r := e.call(c, "PUT", "/me/identity", body); r.Status != 422 || r.field("availability") == "" {
		t.Fatalf("hours: %d %v", r.Status, r.Body)
	}
}

func TestCommitGuard(t *testing.T) {
	e := newEnv(t)
	_, email := e.signedIn("Limit")
	id := e.scalar(`SELECT id::text FROM users WHERE email = $1`, email).(string)
	err := e.server.commitGuard(context.Background(), e.db.Primary(), id, 10_000_001)
	apiErr, ok := err.(*Error)
	if !ok || apiErr.Code != "kyc_limit" || apiErr.Message != "Nilai Rp 10.000.001 melebihi batas Rp 10.000.000 untuk level Email. Verifikasi KTP untuk naik ke Rp 2 M per transaksi." {
		t.Fatalf("level 0 over limit: %v", err)
	}
	if err := e.server.commitGuard(context.Background(), e.db.Primary(), id, 10_000_000); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	if got := rupiah(2_000_000_000); got != "Rp 2.000.000.000" {
		t.Fatal(fmt.Sprint(got))
	}
}
