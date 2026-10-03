package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
)

// Auth & session (spec tag Auth). Messages match the frontend mock (src/mocks/handlers.ts) word for word.

const (
	verifyTokenTTL = 7 * 24 * time.Hour
	resetTokenTTL  = time.Hour
)

var (
	errInvalidCredentials = &Error{Status: http.StatusUnauthorized, Code: "invalid_credentials", Message: "Email atau password salah"}
	errPasswordTooShort   = &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Password terlalu pendek",
		Fields: map[string]string{"password": "Minimal 8 karakter"}}
)

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// GetCurrentUser is the frontend's source of truth for the session.
func (s *Server) GetCurrentUser(ctx context.Context, _ api.GetCurrentUserRequestObject) (api.GetCurrentUserResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	u, err := loadUser(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	return api.GetCurrentUser200JSONResponse(u), nil
}

type credentialUser struct {
	ID          string
	Hash        *string
	Status      string
	SuspendedAt *time.Time
}

// checkCredentials returns the account for email+password, or errInvalidCredentials. Unknown email and wrong password
// cost the same time and give the same answer.
func (s *Server) checkCredentials(ctx context.Context, email, password string) (credentialUser, error) {
	var u credentialUser
	err := s.DB.Primary().QueryRow(ctx,
		`SELECT id, password_hash, status, suspended_at FROM users WHERE email = $1`, normalizeEmail(email)).
		Scan(&u.ID, &u.Hash, &u.Status, &u.SuspendedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Hash == nil) {
		auth.BurnTime(password)
		return u, errInvalidCredentials
	}
	if err != nil {
		return u, err
	}
	ok, err := auth.VerifyPassword(password, *u.Hash)
	if err != nil {
		return u, err
	}
	if !ok {
		return u, errInvalidCredentials
	}
	return u, nil
}

func (s *Server) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	u, err := s.checkCredentials(ctx, req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, err
	}
	if u.Status == "suspended" {
		return nil, s.suspendedLoginError(ctx, u)
	}
	var out api.User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.startSession(ctx, tx, u.ID); err != nil {
			return err
		}
		out, err = loadUser(ctx, tx, u.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.Login200JSONResponse{Body: out}, nil
}

// suspendedLoginError tells a suspended user where their appeal stands (the login page offers the appeal form only
// when there is none for the current suspension).
func (s *Server) suspendedLoginError(ctx context.Context, u credentialUser) error {
	var status string
	var note *string
	err := s.DB.Primary().QueryRow(ctx, `
		SELECT status, decision_note FROM suspension_appeals WHERE user_id = $1 AND suspended_at = $2`, u.ID, u.SuspendedAt).
		Scan(&status, &note)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errSuspended
	case err != nil:
		return err
	}
	msg := suspendedMessage + " Bandingmu sedang ditinjau."
	if status == "denied" {
		msg = suspendedMessage + " Banding ditolak: " + deref(note)
	}
	return &Error{Status: http.StatusForbidden, Code: "account_suspended_appealed", Message: msg}
}

func (s *Server) Logout(ctx context.Context, _ api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	if sess := state(ctx).session; sess != nil {
		if _, err := s.DB.Primary().Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, sess.TokenHash); err != nil {
			return nil, err
		}
	}
	s.setCookie(ctx, "", -1)
	return api.Logout204Response{}, nil
}

func (s *Server) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	name := strings.TrimSpace(req.Body.Name)
	email := normalizeEmail(req.Body.Email)
	fields := map[string]string{}
	if name == "" {
		fields["name"] = "Nama wajib diisi"
	}
	if addr, err := netmail.ParseAddress(email); err != nil || addr.Address != email || !strings.Contains(email[strings.LastIndex(email, "@"):], ".") {
		fields["email"] = "Format email tidak valid"
	}
	if len(fields) > 0 {
		return nil, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Periksa kembali isian", Fields: fields}
	}
	// Same order as the mock: a taken email is reported before a short password.
	var taken bool
	if err := s.DB.Primary().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, errEmailTaken
	}
	if len(req.Body.Password) < auth.MinPasswordLength {
		return nil, errPasswordTooShort
	}
	hash, err := auth.HashPassword(req.Body.Password)
	if err != nil {
		return nil, err
	}

	var out api.User
	var link string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := insertUser(ctx, tx, name, email, &hash, nil, false)
		if err != nil {
			return err
		}
		if link, err = s.issueToken(ctx, tx, id, "verify_email", verifyTokenTTL, "/verify-email"); err != nil {
			return err
		}
		if err := s.startSession(ctx, tx, id); err != nil {
			return err
		}
		out, err = loadUser(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.send(ctx, mail.Message{To: email, Subject: "Verifikasi email Ecopurnity", Link: link,
		Body: fmt.Sprintf("Halo %s, klik link ini untuk memverifikasi email kamu: %s", name, link)})
	return api.Register201JSONResponse{Body: out}, nil
}

var errEmailTaken = &Error{Status: http.StatusConflict, Code: "email_taken", Message: "Email sudah terdaftar",
	Fields: map[string]string{"email": "Email ini sudah punya akun. Masuk saja."}}

// insertUser creates the account, its identity row and a unique username derived from the email.
func insertUser(ctx context.Context, tx pgx.Tx, name, email string, passwordHash, googleSub *string, verified bool) (string, error) {
	base := usernameBase(email)
	for attempt := 0; ; attempt++ {
		username := base
		if attempt > 0 {
			username = fmt.Sprintf("%s%d", base, attempt+1)
		}
		var id string
		// A savepoint per attempt: a unique violation aborts only the attempt, not the transaction.
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, `
				INSERT INTO users (name, username, email, password_hash, google_sub, email_verified_at)
				VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN now() END) RETURNING id`,
				name, username, email, passwordHash, googleSub, verified).Scan(&id)
		})
		switch {
		case err == nil:
			_, err = tx.Exec(ctx, `INSERT INTO identities (user_id) VALUES ($1)`, id)
			return id, err
		case uniqueViolation(err, "users_email_key"):
			return "", errEmailTaken
		case uniqueViolation(err, "users_username_key") && attempt < 50:
			continue
		default:
			return "", err
		}
	}
}

// issueToken replaces any unused token of the same purpose and returns the frontend link carrying the new one.
func (s *Server) issueToken(ctx context.Context, q dbtx, userID, purpose string, ttl time.Duration, path string) (string, error) {
	if _, err := q.Exec(ctx, `UPDATE auth_tokens SET used_at = now() WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose); err != nil {
		return "", err
	}
	token, hash := auth.NewToken()
	if _, err := q.Exec(ctx, `INSERT INTO auth_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, $3, now() + $4)`,
		hash, userID, purpose, ttl); err != nil {
		return "", err
	}
	return strings.TrimRight(s.AppURL, "/") + path + "?token=" + token, nil
}

// send delivers email after the transaction committed; a failed send is logged, not surfaced (the user can resend).
func (s *Server) send(ctx context.Context, m mail.Message) {
	if s.Mail == nil {
		return
	}
	if err := s.Mail.Send(ctx, m); err != nil && s.Log != nil {
		s.Log.Error("send email", "to", m.To, "subject", m.Subject, "err", err)
	}
}

// useToken consumes a valid token and returns its user.
func useToken(ctx context.Context, tx pgx.Tx, token, purpose string) (string, bool, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		UPDATE auth_tokens SET used_at = now()
		WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`, auth.HashToken(token), purpose).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return userID, err == nil, err
}

func (s *Server) VerifyEmail(ctx context.Context, req api.VerifyEmailRequestObject) (api.VerifyEmailResponseObject, error) {
	var out api.User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		userID, ok, err := useToken(ctx, tx, req.Body.Token, "verify_email")
		if err != nil {
			return err
		}
		if !ok {
			return &Error{Status: http.StatusBadRequest, Code: "invalid_token", Message: "Link verifikasi tidak valid atau sudah kedaluwarsa"}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = coalesce(email_verified_at, now()) WHERE id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE parties SET verified = true WHERE user_id = $1`, userID); err != nil {
			return err
		}
		out, err = loadUser(ctx, tx, userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.VerifyEmail200JSONResponse(out), nil
}

func (s *Server) ResendVerificationEmail(ctx context.Context, _ api.ResendVerificationEmailRequestObject) (api.ResendVerificationEmailResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var email, name string
	var verified bool
	if err := s.DB.Primary().QueryRow(ctx, `SELECT email, name, email_verified_at IS NOT NULL FROM users WHERE id = $1`, sess.UserID).
		Scan(&email, &name, &verified); err != nil {
		return nil, err
	}
	if verified {
		return api.ResendVerificationEmail204Response{}, nil
	}
	link, err := s.issueToken(ctx, s.DB.Primary(), sess.UserID, "verify_email", verifyTokenTTL, "/verify-email")
	if err != nil {
		return nil, err
	}
	s.send(ctx, mail.Message{To: email, Subject: "Verifikasi email Ecopurnity", Link: link,
		Body: fmt.Sprintf("Halo %s, klik link ini untuk memverifikasi email kamu: %s", name, link)})
	return api.ResendVerificationEmail204Response{}, nil
}

// ForgotPassword always answers 204 so it never reveals whether an email has an account.
func (s *Server) ForgotPassword(ctx context.Context, req api.ForgotPasswordRequestObject) (api.ForgotPasswordResponseObject, error) {
	email := normalizeEmail(req.Body.Email)
	var userID, name string
	err := s.DB.Primary().QueryRow(ctx, `SELECT id, name FROM users WHERE email = $1 AND status <> 'suspended'`, email).Scan(&userID, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ForgotPassword204Response{}, nil
	}
	if err != nil {
		return nil, err
	}
	link, err := s.issueToken(ctx, s.DB.Primary(), userID, "reset_password", resetTokenTTL, "/reset-password")
	if err != nil {
		return nil, err
	}
	s.send(ctx, mail.Message{To: email, Subject: "Reset password Ecopurnity", Link: link,
		Body: fmt.Sprintf("Halo %s, klik link ini untuk membuat password baru (berlaku 1 jam): %s", name, link)})
	return api.ForgotPassword204Response{}, nil
}

// ResetPassword replaces the password and signs out every session of the account (a reset usually means the old
// password is compromised). It does not sign the user in.
func (s *Server) ResetPassword(ctx context.Context, req api.ResetPasswordRequestObject) (api.ResetPasswordResponseObject, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The token is checked first; a short password leaves the token usable (the user just retries).
		var userID string
		err := tx.QueryRow(ctx, `
			SELECT user_id FROM auth_tokens WHERE token_hash = $1 AND purpose = 'reset_password' AND used_at IS NULL AND expires_at > now()
			FOR UPDATE`, auth.HashToken(req.Body.Token)).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusBadRequest, Code: "invalid_token", Message: "Link reset tidak valid atau sudah kedaluwarsa"}
		}
		if err != nil {
			return err
		}
		if len(req.Body.Password) < auth.MinPasswordLength {
			return errPasswordTooShort
		}
		hash, err := auth.HashPassword(req.Body.Password)
		if err != nil {
			return err
		}
		if _, _, err := useToken(ctx, tx, req.Body.Token, "reset_password"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ResetPassword204Response{}, nil
}

// AppealSuspension lets a suspended account (which cannot sign in) appeal; credentials are re-checked. One appeal per
// suspension episode (users.suspended_at).
func (s *Server) AppealSuspension(ctx context.Context, req api.AppealSuspensionRequestObject) (api.AppealSuspensionResponseObject, error) {
	u, err := s.checkCredentials(ctx, req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, err
	}
	if u.Status != "suspended" || u.SuspendedAt == nil {
		return nil, &Error{Status: http.StatusConflict, Code: "not_suspended", Message: "Akun ini tidak disuspend"}
	}
	reason := strings.TrimSpace(req.Body.Reason)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM suspension_appeals WHERE user_id = $1 AND suspended_at = $2)`,
			u.ID, u.SuspendedAt).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return &Error{Status: http.StatusConflict, Code: "already_appealed", Message: "Banding sudah pernah diajukan"}
		}
		if len([]rune(reason)) < 20 {
			return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Jelaskan bandingmu",
				Fields: map[string]string{"reason": "Minimal 20 karakter"}}
		}
		_, err := tx.Exec(ctx, `INSERT INTO suspension_appeals (user_id, suspended_at, reason) VALUES ($1, $2, $3)`, u.ID, u.SuspendedAt, reason)
		if uniqueViolation(err, "") { // a concurrent appeal won the race
			return &Error{Status: http.StatusConflict, Code: "already_appealed", Message: "Banding sudah pernah diajukan"}
		}
		if err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, u.ID).Scan(&name); err != nil {
			return err
		}
		return writeAudit(ctx, tx, audit{ActorUserID: &u.ID, ActorLabel: name, Action: "Ajukan banding suspend",
			EntityType: "user", EntityID: u.ID, EntityLabel: name, Reason: &reason})
	})
	if err != nil {
		return nil, err
	}
	return api.AppealSuspension200JSONResponse{Ok: true}, nil
}

// LoginWithGoogle is the mock-compatible stand-in (fixed test account) behind GOOGLE_DEV_LOGIN; the real OAuth code
// exchange replaces it and changes the request contract.
func (s *Server) LoginWithGoogle(ctx context.Context, _ api.LoginWithGoogleRequestObject) (api.LoginWithGoogleResponseObject, error) {
	if !s.GoogleDevLogin {
		return nil, api.ErrNotImplemented
	}
	const email, sub = "tamu.google@gmail.com", "dev-google-account"
	var out api.User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id, status string
		err := tx.QueryRow(ctx, `SELECT id, status FROM users WHERE google_sub = $1 OR email = $2`, sub, email).Scan(&id, &status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if id, err = insertUser(ctx, tx, "Tamu Google", email, nil, ptr(sub), true); err != nil {
				return err
			}
		case err != nil:
			return err
		case status == "suspended":
			return errSuspended
		}
		if err := s.startSession(ctx, tx, id); err != nil {
			return err
		}
		out, err = loadUser(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.LoginWithGoogle200JSONResponse{Body: out}, nil
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
