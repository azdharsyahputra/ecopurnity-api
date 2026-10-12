package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
)

const (
	googleStateCookie = "ecp_oauth_state"
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
)

var (
	errGoogleFailed = &Error{Status: http.StatusBadRequest, Code: "google_failed", Message: "Gagal masuk dengan Google. Coba lagi."}
	googleHTTP      = &http.Client{Timeout: 10 * time.Second}
)

func (s *Server) googleRedirectURI() string {
	return strings.TrimRight(s.AppURL, "/") + "/auth/google/callback"
}

// googleStart sends the browser to Google's consent screen; the state cookie is checked again in LoginWithGoogle.
func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	if s.GoogleClientID == "" { // dev stub: straight back to the app, which posts to /auth/google
		http.Redirect(w, r, s.googleRedirectURI()+"?code=dev", http.StatusFound)
		return
	}
	state, _ := auth.NewToken()
	http.SetCookie(w, &http.Cookie{
		Name: googleStateCookie, Value: state, Path: BasePath + "/auth/google", MaxAge: 600,
		HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, googleAuthURL+"?"+url.Values{
		"client_id": {s.GoogleClientID}, "redirect_uri": {s.googleRedirectURI()}, "response_type": {"code"},
		"scope": {"openid email profile"}, "state": {state}, "prompt": {"select_account"},
	}.Encode(), http.StatusFound)
}

type googleIdentity struct {
	Aud           string `json:"aud"`
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// googleExchange trades the authorization code for the user's identity.
func (s *Server) googleExchange(ctx context.Context, code string) (googleIdentity, error) {
	var id googleIdentity
	tokenURL := s.GoogleTokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(url.Values{
		"code": {code}, "client_id": {s.GoogleClientID}, "client_secret": {s.GoogleClientSecret},
		"redirect_uri": {s.googleRedirectURI()}, "grant_type": {"authorization_code"},
	}.Encode()))
	if err != nil {
		return id, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := googleHTTP.Do(req)
	if err != nil {
		return id, err
	}
	defer res.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil {
		return id, err
	}
	// The ID token comes straight from Google over TLS, so its signature needs no separate check (OIDC Core 3.1.3.7).
	parts := strings.Split(tok.IDToken, ".")
	if res.StatusCode != http.StatusOK || len(parts) != 3 {
		if s.Log != nil {
			s.Log.Warn("google token exchange", "status", res.StatusCode, "error", tok.Error)
		}
		return id, errGoogleFailed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &id) != nil || id.Aud != s.GoogleClientID || id.Sub == "" || id.Email == "" {
		return id, errGoogleFailed
	}
	if !id.EmailVerified {
		return id, &Error{Status: http.StatusBadRequest, Code: "google_failed", Message: "Email akun Google ini belum terverifikasi."}
	}
	id.Email = normalizeEmail(id.Email)
	if id.Name = strings.TrimSpace(id.Name); id.Name == "" {
		id.Name = usernameBase(id.Email)
	}
	return id, nil
}

func (s *Server) LoginWithGoogle(ctx context.Context, req api.LoginWithGoogleRequestObject) (api.LoginWithGoogleResponseObject, error) {
	id := googleIdentity{Sub: "dev-google-account", Email: "tamu.google@gmail.com", Name: "Tamu Google"}
	switch {
	case s.GoogleClientID != "":
		st := state(ctx)
		if req.Body == nil || req.Body.Code == nil || req.Body.State == nil || st.r == nil {
			return nil, errGoogleFailed
		}
		c, err := st.r.Cookie(googleStateCookie)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(*req.Body.State)) != 1 {
			return nil, errGoogleFailed
		}
		http.SetCookie(st.w, &http.Cookie{Name: googleStateCookie, Path: BasePath + "/auth/google", MaxAge: -1})
		if id, err = s.googleExchange(ctx, *req.Body.Code); err != nil {
			return nil, err
		}
	case !s.GoogleDevLogin:
		return nil, api.ErrNotImplemented
	}

	var out api.User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var uid, status string
		var linked *string
		var verified bool
		err := tx.QueryRow(ctx, `
			SELECT id, status, google_sub, email_verified_at IS NOT NULL FROM users
			WHERE google_sub = $1 OR email = $2 ORDER BY google_sub IS NOT DISTINCT FROM $1 DESC LIMIT 1`,
			id.Sub, id.Email).Scan(&uid, &status, &linked, &verified)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if uid, err = insertUser(ctx, tx, id.Name, id.Email, nil, &id.Sub, true); err != nil {
				return err
			}
		case err != nil:
			return err
		case status == "suspended":
			return errSuspended
		case linked == nil && verified:
			if _, err := tx.Exec(ctx, `UPDATE users SET google_sub = $1 WHERE id = $2`, id.Sub, uid); err != nil {
				return err
			}
		case linked == nil:
			// Nobody proved they own this email yet, so whoever registered it may not be this person: Google's
			// verified email wins, and the old password and its sessions go.
			if _, err := tx.Exec(ctx, `UPDATE users SET google_sub = $1, email_verified_at = now(), password_hash = NULL WHERE id = $2`, id.Sub, uid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, uid); err != nil {
				return err
			}
		case *linked != id.Sub:
			return errEmailTaken
		}
		if err := s.startSession(ctx, tx, uid); err != nil {
			return err
		}
		out, err = loadUser(ctx, tx, uid)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.LoginWithGoogle200JSONResponse{Body: out}, nil
}
