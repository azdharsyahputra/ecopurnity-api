package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRegisterSessionLogout(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	email := uniqueEmail(t, "Rina.Wulandari")

	r := e.call(c, "POST", "/auth/register", map[string]any{"name": "  Rina Wulandari ", "email": "  " + strings.ToUpper(email), "password": "rahasia123"})
	if r.Status != http.StatusCreated {
		t.Fatalf("register: %d %v", r.Status, r.Body)
	}
	if r.Body["email"] != strings.ToLower(email) || r.Body["name"] != "Rina Wulandari" || r.Body["emailVerified"] != false || r.Body["onboarded"] != false {
		t.Fatalf("register body: %v", r.Body)
	}
	if !strings.HasPrefix(r.Body["username"].(string), "rina.wulandari") {
		t.Fatalf("username: %v", r.Body["username"])
	}
	cookie := r.Header.Get("Set-Cookie")
	for _, want := range []string{"ecp_session=", "HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Fatalf("cookie %q lacks %s", cookie, want)
		}
	}
	// Only the hash of the session token is stored.
	if n := e.scalar(`SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = $1`, strings.ToLower(email)); n != int64(1) {
		t.Fatalf("sessions: %v", n)
	}

	if r := e.call(c, "GET", "/auth/me", nil); r.Status != 200 || r.Body["email"] != strings.ToLower(email) {
		t.Fatalf("me: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/auth/logout", nil); r.Status != http.StatusNoContent {
		t.Fatalf("logout: %d", r.Status)
	}
	if r := e.call(c, "GET", "/auth/me", nil); r.Status != 401 || r.code() != "unauthenticated" {
		t.Fatalf("me after logout: %d %v", r.Status, r.Body)
	}
	// Logging out without a session still succeeds.
	if r := e.call(e.client(), "POST", "/auth/logout", nil); r.Status != http.StatusNoContent {
		t.Fatalf("anonymous logout: %d", r.Status)
	}

	// Sign back in; email is case-insensitive.
	if r := e.call(c, "POST", "/auth/login", map[string]any{"email": strings.ToUpper(email), "password": "rahasia123"}); r.Status != 200 {
		t.Fatalf("login: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("me after login: %d", r.Status)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	email := uniqueEmail(t, "dup")
	if r := e.call(e.client(), "POST", "/auth/register", map[string]any{"name": "A", "email": email, "password": "rahasia123"}); r.Status != 201 {
		t.Fatalf("first: %d %v", r.Status, r.Body)
	}
	// Taken email is reported before the short password (mock order), with the field message the UI shows.
	r := e.call(e.client(), "POST", "/auth/register", map[string]any{"name": "B", "email": email, "password": "x"})
	if r.Status != 409 || r.code() != "email_taken" || r.field("email") != "Email ini sudah punya akun. Masuk saja." {
		t.Fatalf("dup: %d %v", r.Status, r.Body)
	}
	r = e.call(e.client(), "POST", "/auth/register", map[string]any{"name": "B", "email": uniqueEmail(t, "short"), "password": "1234567"})
	if r.Status != 422 || r.field("password") != "Minimal 8 karakter" {
		t.Fatalf("short password: %d %v", r.Status, r.Body)
	}
	r = e.call(e.client(), "POST", "/auth/register", map[string]any{"name": " ", "email": "bukan-email", "password": "rahasia123"})
	if r.Status != 422 || r.field("name") == "" || r.field("email") == "" {
		t.Fatalf("bad name/email: %d %v", r.Status, r.Body)
	}
	// Same local part: usernames stay unique.
	other := strings.Replace(email, "@example.id", "@lain.id", 1)
	r = e.call(e.client(), "POST", "/auth/register", map[string]any{"name": "C", "email": other, "password": "rahasia123"})
	if r.Status != 201 || !strings.HasSuffix(r.Body["username"].(string), "2") {
		t.Fatalf("username collision: %d %v", r.Status, r.Body)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t)
	email := uniqueEmail(t, "login")
	e.call(e.client(), "POST", "/auth/register", map[string]any{"name": "L", "email": email, "password": "rahasia123"})

	wrong := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": email, "password": "salah-salah"})
	unknown := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": uniqueEmail(t, "nobody"), "password": "salah-salah"})
	for _, r := range []resp{wrong, unknown} {
		if r.Status != 401 || r.code() != "invalid_credentials" || r.message() != "Email atau password salah" {
			t.Fatalf("bad login: %d %v", r.Status, r.Body)
		}
	}
	// Body is validated against the spec before the handler.
	if r := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": email}); r.Status != 422 || r.field("password") == "" {
		t.Fatalf("missing password: %d %v", r.Status, r.Body)
	}
}

func token(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Query().Get("token") == "" {
		t.Fatalf("link %q", link)
	}
	return u.Query().Get("token")
}

func TestEmailVerificationCode(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	email := uniqueEmail(t, "verify")
	e.call(c, "POST", "/auth/register", map[string]any{"name": "Vina", "email": email, "password": "rahasia123"})
	first, ok := e.lastMail(email)
	if !ok || len(first.Code) != 6 || !strings.Contains(first.Subject, first.Code) || !strings.Contains(first.Body, first.Code) {
		t.Fatalf("verification mail: %+v", first)
	}
	// Only an HMAC is stored, never the code.
	if n := e.scalar(`SELECT count(*) FROM auth_tokens WHERE token_hash = convert_to($1, 'UTF8')`, first.Code); n != int64(0) {
		t.Fatal("code stored in clear")
	}

	// Needs the session; malformed codes are rejected by the spec (pattern).
	if r := e.call(e.client(), "POST", "/auth/verify-email", map[string]any{"code": first.Code}); r.Status != 401 {
		t.Fatalf("no session: %d", r.Status)
	}
	if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": "12ab"}); r.Status != 422 || r.field("code") == "" {
		t.Fatalf("malformed: %d %v", r.Status, r.Body)
	}

	// Resend is rate limited to one per minute.
	r := e.call(c, "POST", "/auth/resend-verification", nil)
	if r.Status != 429 || r.code() != "resend_cooldown" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("cooldown: %d %v", r.Status, r.Body)
	}
	e.exec(`UPDATE auth_tokens SET created_at = created_at - interval '2 minutes' WHERE user_id = (SELECT id FROM users WHERE email = $1)`, email)
	if r := e.call(c, "POST", "/auth/resend-verification", nil); r.Status != 204 {
		t.Fatalf("resend: %d %v", r.Status, r.Body)
	}
	second, _ := e.lastMail(email)
	if second.Code == first.Code {
		// Astronomically unlikely; the codes are independent.
		t.Log("same code twice")
	}
	// The old code no longer works once a new one was sent.
	if first.Code != second.Code {
		if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": first.Code}); r.Status != 422 || r.code() != "invalid_code" {
			t.Fatalf("old code: %d %v", r.Status, r.Body)
		}
	}
	r = e.call(c, "POST", "/auth/verify-email", map[string]any{"code": second.Code})
	if r.Status != 200 || r.Body["emailVerified"] != true {
		t.Fatalf("verify: %d %v", r.Status, r.Body)
	}
	// Idempotent once verified; resend does nothing.
	if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": "000000"}); r.Status != 200 {
		t.Fatalf("verified again: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/auth/resend-verification", nil); r.Status != 204 {
		t.Fatalf("resend when verified: %d", r.Status)
	}
}

func TestEmailCodeAttemptsAndExpiry(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	email := uniqueEmail(t, "brute")
	e.call(c, "POST", "/auth/register", map[string]any{"name": "B", "email": email, "password": "rahasia123"})
	m, _ := e.lastMail(email)
	wrong := "000000"
	if m.Code == wrong {
		wrong = "111111"
	}
	for i := 1; i <= 4; i++ {
		r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": wrong})
		if r.Status != 422 || r.code() != "invalid_code" || !strings.Contains(r.message(), "Sisa") {
			t.Fatalf("attempt %d: %d %v", i, r.Status, r.Body)
		}
	}
	if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": wrong}); r.Status != 429 || r.code() != "too_many_attempts" {
		t.Fatalf("5th attempt: %d %v", r.Status, r.Body)
	}
	// Burned: even the right code fails now.
	if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": m.Code}); r.Status != 422 || r.code() != "code_expired" {
		t.Fatalf("after burn: %d %v", r.Status, r.Body)
	}

	// Expiry.
	e.exec(`UPDATE auth_tokens SET created_at = created_at - interval '2 minutes' WHERE user_id = (SELECT id FROM users WHERE email = $1)`, email)
	e.call(c, "POST", "/auth/resend-verification", nil)
	m2, _ := e.lastMail(email)
	e.exec(`UPDATE auth_tokens SET expires_at = now() - interval '1 second' WHERE user_id = (SELECT id FROM users WHERE email = $1) AND used_at IS NULL`, email)
	if r := e.call(c, "POST", "/auth/verify-email", map[string]any{"code": m2.Code}); r.Status != 422 || r.code() != "code_expired" {
		t.Fatalf("expired: %d %v", r.Status, r.Body)
	}
}

func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	email := uniqueEmail(t, "reset")
	other := e.client()
	e.call(other, "POST", "/auth/register", map[string]any{"name": "R", "email": email, "password": "rahasia123"})

	// Unknown email: same 204, no mail.
	ghost := uniqueEmail(t, "ghost")
	if r := e.call(e.client(), "POST", "/auth/forgot-password", map[string]any{"email": ghost}); r.Status != 204 {
		t.Fatalf("forgot unknown: %d", r.Status)
	}
	if _, sent := e.lastMail(ghost); sent {
		t.Fatal("mail sent for unknown email")
	}
	if r := e.call(e.client(), "POST", "/auth/forgot-password", map[string]any{"email": strings.ToUpper(email)}); r.Status != 204 {
		t.Fatalf("forgot: %d", r.Status)
	}
	m, _ := e.lastMail(email)
	if !strings.HasPrefix(m.Link, "http://app.test/reset-password?token=") {
		t.Fatalf("reset mail: %+v", m)
	}
	tok := token(t, m.Link)

	if r := e.call(e.client(), "POST", "/auth/reset-password", map[string]any{"token": "nope", "password": "x"}); r.Status != 400 || r.code() != "invalid_token" {
		t.Fatalf("bad token checked before password: %d %v", r.Status, r.Body)
	}
	// A short password keeps the token usable.
	if r := e.call(e.client(), "POST", "/auth/reset-password", map[string]any{"token": tok, "password": "pendek"}); r.Status != 422 || r.field("password") == "" {
		t.Fatalf("short: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "POST", "/auth/reset-password", map[string]any{"token": tok, "password": "baru-sekali-123"}); r.Status != 204 {
		t.Fatalf("reset: %d %v", r.Status, r.Body)
	}
	// Old sessions are signed out, old password stops working, new one works, token is spent.
	if r := e.call(other, "GET", "/auth/me", nil); r.Status != 401 {
		t.Fatalf("old session survived reset: %d", r.Status)
	}
	if r := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": email, "password": "rahasia123"}); r.Status != 401 {
		t.Fatalf("old password: %d", r.Status)
	}
	if r := e.call(e.client(), "POST", "/auth/login", map[string]any{"email": email, "password": "baru-sekali-123"}); r.Status != 200 {
		t.Fatalf("new password: %d", r.Status)
	}
	if r := e.call(e.client(), "POST", "/auth/reset-password", map[string]any{"token": tok, "password": "lagi-lagi-123"}); r.Status != 400 {
		t.Fatalf("token reuse: %d", r.Status)
	}
}

func TestSuspensionAndAppeal(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	email := uniqueEmail(t, "suspend")
	e.call(c, "POST", "/auth/register", map[string]any{"name": "Bima", "email": email, "password": "rahasia123"})
	creds := map[string]any{"email": email, "password": "rahasia123"}

	if r := e.call(e.client(), "POST", "/auth/appeal", map[string]any{"email": email, "password": "rahasia123", "reason": strings.Repeat("x", 25)}); r.Status != 409 || r.code() != "not_suspended" {
		t.Fatalf("appeal while active: %d %v", r.Status, r.Body)
	}

	e.exec(`UPDATE users SET status = 'suspended', suspended_at = now() WHERE email = $1`, email)

	// An existing session is cut off immediately.
	if r := e.call(c, "GET", "/auth/me", nil); r.Status != 403 || r.code() != "account_suspended" {
		t.Fatalf("me suspended: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "POST", "/auth/login", creds); r.Status != 403 || r.code() != "account_suspended" {
		t.Fatalf("login suspended: %d %v", r.Status, r.Body)
	}

	appeal := map[string]any{"email": email, "password": "salah-salah", "reason": strings.Repeat("x", 25)}
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 401 {
		t.Fatalf("appeal wrong password: %d", r.Status)
	}
	appeal["password"] = "rahasia123"
	appeal["reason"] = "  terlalu pendek  "
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 422 || r.field("reason") != "Minimal 20 karakter" {
		t.Fatalf("short reason: %d %v", r.Status, r.Body)
	}
	appeal["reason"] = "Saya tidak pernah meminta transfer di luar escrow, mohon ditinjau."
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 200 || r.Body["ok"] != true {
		t.Fatalf("appeal: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 409 || r.code() != "already_appealed" {
		t.Fatalf("second appeal: %d %v", r.Status, r.Body)
	}
	r := e.call(e.client(), "POST", "/auth/login", creds)
	if r.Status != 403 || r.code() != "account_suspended_appealed" || !strings.HasSuffix(r.message(), "Bandingmu sedang ditinjau.") {
		t.Fatalf("login with pending appeal: %d %v", r.Status, r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM audit_log WHERE action = 'Ajukan banding suspend' AND entity_label = 'Bima'`); n != int64(1) {
		t.Fatalf("audit rows: %v", n)
	}

	e.exec(`UPDATE suspension_appeals SET status = 'denied', decided_at = now(), decided_by = (SELECT id FROM users WHERE email = $1), decision_note = 'Bukti transfer valid'
	        WHERE user_id = (SELECT id FROM users WHERE email = $1)`, email)
	r = e.call(e.client(), "POST", "/auth/login", creds)
	if r.Status != 403 || !strings.HasSuffix(r.message(), "Banding ditolak: Bukti transfer valid") {
		t.Fatalf("login with denied appeal: %d %v", r.Status, r.Body)
	}

	// A new suspension episode can be appealed again.
	e.exec(`UPDATE users SET status = 'active', suspended_at = NULL WHERE email = $1`, email)
	e.exec(`UPDATE users SET status = 'suspended', suspended_at = now() WHERE email = $1`, email)
	if r := e.call(e.client(), "POST", "/auth/appeal", appeal); r.Status != 200 {
		t.Fatalf("appeal for a new suspension: %d %v", r.Status, r.Body)
	}
}

func TestGoogleDevLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	r := e.call(c, "POST", "/auth/google", nil)
	if r.Status != 200 || r.Body["email"] != "tamu.google@gmail.com" || r.Body["emailVerified"] != true {
		t.Fatalf("google: %d %v", r.Status, r.Body)
	}
	// Second sign-in reuses the account.
	if r2 := e.call(e.client(), "POST", "/auth/google", nil); r2.Body["id"] != r.Body["id"] {
		t.Fatalf("google second: %v vs %v", r2.Body["id"], r.Body["id"])
	}
	if r := e.call(c, "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("me: %d", r.Status)
	}
}

func TestOnboarding(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO users (name, username, email, password_hash) VALUES ('Sari Admin', $1, $2, 'x')`,
		"admin"+strings.Split(uniqueEmail(t, "a"), "@")[0][2:], uniqueEmail(t, "admin"))
	e.exec(`INSERT INTO user_capabilities (user_id, capability) SELECT id, 'admin' FROM users WHERE name = 'Sari Admin' ON CONFLICT DO NOTHING`)

	c := e.client()
	email := uniqueEmail(t, "onboard")
	e.call(c, "POST", "/auth/register", map[string]any{"name": "Ajar", "email": email, "password": "rahasia123"})
	orgName := "PT Solusi " + strings.Split(email, "@")[0]
	body := map[string]any{
		"goals": []string{"sell"}, "location": "Jawa Barat", "categories": []string{"packaging", "agri"},
		"minPriceIdr": 1500, "radiusKm": 50,
		"firstListing":           map[string]any{"kind": "supply", "item": "Box karton", "quantity": map[string]any{"value": 1000, "unit": "pcs"}},
		"organization":           map[string]any{"name": orgName, "industry": "Kemasan", "location": "Bandung"},
		"marketMakerApplication": map[string]any{"organization": orgName, "reason": "Mengagregasi UMKM kemasan Bandung"},
	}
	r := e.call(c, "PATCH", "/me/onboarding", body)
	if r.Status != 200 || r.Body["onboarded"] != true || r.Body["location"] != "Jawa Barat" {
		t.Fatalf("onboarding: %d %v", r.Status, r.Body)
	}
	orgs, _ := r.Body["orgs"].([]any)
	if len(orgs) != 1 || orgs[0].(map[string]any)["role"] != "owner" || orgs[0].(map[string]any)["orgName"] != orgName {
		t.Fatalf("orgs: %v", r.Body["orgs"])
	}
	checks := map[string]any{
		`SELECT count(*) FROM listings l JOIN parties p ON p.id = l.owner_party_id JOIN users u ON u.id = p.user_id
		  WHERE u.email = $1 AND l.kind = 'supply' AND l.price_idr = 1500 AND l.category_id = 'packaging' AND l.code LIKE 'SUP-%'`: int64(1),
		`SELECT count(*) FROM org_roles r JOIN orgs o ON o.id = r.org_id JOIN org_members m ON m.org_id = o.id JOIN users u ON u.id = m.user_id WHERE u.email = $1`:      int64(5),
		`SELECT count(*) FROM mm_applications a JOIN users u ON u.id = a.user_id WHERE u.email = $1 AND a.status = 'pending'`:                                            int64(1),
		`SELECT count(*) FROM identities i JOIN users u ON u.id = i.user_id WHERE u.email = $1 AND i.delivery_radius_km = 50 AND i.pref_categories = '{packaging,agri}'`: int64(1),
	}
	for q, want := range checks {
		if got := e.scalar(q, email); got != want {
			t.Fatalf("%s: got %v want %v", q, got, want)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM notifications n JOIN users u ON u.id = n.user_id WHERE u.name = 'Sari Admin' AND n.href = '/admin/mm-applications'`); n.(int64) < 1 {
		t.Fatalf("admin not notified: %v", n)
	}

	// Repeating onboarding updates preferences but does not duplicate the org, listing or application.
	body["radiusKm"] = 10
	if r := e.call(c, "PATCH", "/me/onboarding", body); r.Status != 200 || len(r.Body["orgs"].([]any)) != 1 {
		t.Fatalf("repeat: %d %v", r.Status, r.Body)
	}
	if n := e.scalar(`SELECT count(*) FROM listings l JOIN parties p ON p.id = l.owner_party_id JOIN users u ON u.id = p.user_id WHERE u.email = $1`, email); n != int64(1) {
		t.Fatalf("listing duplicated: %v", n)
	}
	if n := e.scalar(`SELECT delivery_radius_km::int FROM identities i JOIN users u ON u.id = i.user_id WHERE u.email = $1`, email); n != int32(10) {
		t.Fatalf("radius not updated: %v (%T)", n, n)
	}

	if r := e.call(e.client(), "PATCH", "/me/onboarding", body); r.Status != 401 {
		t.Fatalf("onboarding without session: %d", r.Status)
	}
}

func TestRateLimitOnLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	var last resp
	for i := 0; i < 12; i++ {
		last = e.call(c, "POST", "/auth/login", map[string]any{"email": "x@y.id", "password": "salah-salah"})
	}
	if last.Status != http.StatusTooManyRequests || last.code() != "rate_limited" || last.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %v", last.Status, last.Body)
	}
}
