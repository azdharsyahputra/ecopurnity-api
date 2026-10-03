package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
)

const sessionCookie = "ecp_session"

// session is the signed-in user of a request.
type session struct {
	UserID      string
	TokenHash   []byte
	Name        string
	Status      string // active | restricted | suspended
	SuspendedAt *time.Time
}

type reqKey struct{}

// reqState is per-request plumbing the typed handlers can't reach otherwise: the ResponseWriter (cookies) and the
// session loaded by the middleware.
type reqState struct {
	w       http.ResponseWriter
	r       *http.Request
	session *session
}

func state(ctx context.Context) *reqState {
	st, _ := ctx.Value(reqKey{}).(*reqState)
	if st == nil {
		return &reqState{}
	}
	return st
}

// withSession loads the session from the cookie (on the primary: a login must be visible to the very next request)
// and slides its expiry at most once an hour.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &reqState{w: w, r: r}
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && s.DB != nil {
			hash := auth.HashToken(c.Value)
			var sess session
			var lastSeen time.Time
			err := s.DB.Primary().QueryRow(r.Context(), `
				SELECT u.id, u.name, u.status, u.suspended_at, s.last_seen_at
				FROM sessions s JOIN users u ON u.id = s.user_id
				WHERE s.token_hash = $1 AND s.expires_at > now()`, hash).
				Scan(&sess.UserID, &sess.Name, &sess.Status, &sess.SuspendedAt, &lastSeen)
			switch {
			case err == nil:
				sess.TokenHash = hash
				st.session = &sess
				if time.Since(lastSeen) > time.Hour {
					_, _ = s.DB.Primary().Exec(r.Context(),
						`UPDATE sessions SET last_seen_at = now(), expires_at = now() + $2 WHERE token_hash = $1`, hash, s.SessionTTL)
				}
			case !errors.Is(err, pgx.ErrNoRows) && s.Log != nil:
				s.Log.Error("session lookup", "err", err)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqKey{}, st)))
	})
}

var (
	errUnauthenticated = &Error{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "Belum login"}
	errSuspended       = &Error{Status: http.StatusForbidden, Code: "account_suspended", Message: suspendedMessage}
	errRestricted      = &Error{Status: http.StatusForbidden, Code: "account_restricted", Message: "Akunmu dibatasi tim governance: belum bisa bid atau membuat auction."}
)

const suspendedMessage = "Akun ini disuspend oleh tim governance."

// requireUser returns the signed-in, non-suspended user, or the 401/403 error to return.
func requireUser(ctx context.Context) (*session, error) {
	sess := state(ctx).session
	if sess == nil {
		return nil, errUnauthenticated
	}
	if sess.Status == "suspended" {
		return nil, errSuspended
	}
	return sess, nil
}

// requireActive additionally rejects restricted accounts (no new bids, listings, auctions).
func requireActive(ctx context.Context) (*session, error) {
	sess, err := requireUser(ctx)
	if err == nil && sess.Status == "restricted" {
		return nil, errRestricted
	}
	return sess, err
}

// startSession creates a session for userID and sets the cookie on the current response.
func (s *Server) startSession(ctx context.Context, q dbtx, userID string) error {
	token, hash := auth.NewToken()
	st := state(ctx)
	var ua, ip *string
	if st.r != nil {
		agent := st.r.UserAgent()
		ua = &agent
		if host := clientIP(st.r); host != "" {
			ip = &host
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at, user_agent, ip) VALUES ($1, $2, now() + $3, $4, $5::inet)`,
		hash, userID, s.SessionTTL, ua, ip); err != nil {
		return err
	}
	s.setCookie(ctx, token, int(s.SessionTTL.Seconds()))
	return nil
}

func (s *Server) setCookie(ctx context.Context, value string, maxAge int) {
	if w := state(ctx).w; w != nil {
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
			HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteLaxMode,
		})
	}
}
