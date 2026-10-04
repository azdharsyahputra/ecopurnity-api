package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
)

const (
	emailCodeTTL      = 10 * time.Minute
	emailCodeAttempts = 5
	emailCodeCooldown = 60 * time.Second
	resetTokenTTL     = time.Hour
)

var (
	errInvalidCredentials = &Error{Status: http.StatusUnauthorized, Code: "invalid_credentials", Message: "Email atau password salah"}
	errPasswordTooShort   = &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Password terlalu pendek",
		Fields: map[string]string{"password": "Minimal 8 karakter"}}
)

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

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
	var code string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := insertUser(ctx, tx, name, email, &hash, nil, false)
		if err != nil {
			return err
		}
		if code, err = s.issueEmailCode(ctx, tx, id); err != nil {
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
	s.send(ctx, verificationMail(email, name, code))
	return api.Register201JSONResponse{Body: out}, nil
}

var errEmailTaken = &Error{Status: http.StatusConflict, Code: "email_taken", Message: "Email sudah terdaftar",
	Fields: map[string]string{"email": "Email ini sudah punya akun. Masuk saja."}}

func insertUser(ctx context.Context, tx pgx.Tx, name, email string, passwordHash, googleSub *string, verified bool) (string, error) {
	base := usernameBase(email)
	for attempt := 0; ; attempt++ {
		username := base
		if attempt > 0 {
			username = fmt.Sprintf("%s%d", base, attempt+1)
		}
		var id string

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

func (s *Server) send(ctx context.Context, m mail.Message) {
	if s.Mail == nil {
		return
	}
	s.mailWG.Add(1)
	go func() {
		defer s.mailWG.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.Mail.Send(ctx, m); err != nil && s.Log != nil {
			s.Log.Error("send email", "to", m.To, "subject", m.Subject, "err", err)
		}
	}()
}

func (s *Server) WaitMail() { s.mailWG.Wait() }

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

func (s *Server) issueEmailCode(ctx context.Context, q dbtx, userID string) (string, error) {
	if _, err := q.Exec(ctx, `UPDATE auth_tokens SET used_at = now() WHERE user_id = $1 AND purpose = 'verify_email' AND used_at IS NULL`, userID); err != nil {
		return "", err
	}
	code := auth.NewOTP()
	_, err := q.Exec(ctx, `INSERT INTO auth_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'verify_email', now() + $3)`,
		auth.OTPHash(s.Keys.OTP, "verify_email", userID, code), userID, emailCodeTTL)
	return code, err
}

func verificationMail(to, name, code string) mail.Message {
	return mail.VerifyCode(to, name, code, emailCodeTTL)
}

func codeError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message, Fields: map[string]string{"code": message}}
}

func (s *Server) VerifyEmail(ctx context.Context, req api.VerifyEmailRequestObject) (api.VerifyEmailResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.User
	var codeErr *Error
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var verified bool
		if err := tx.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, sess.UserID).Scan(&verified); err != nil {
			return err
		}
		if !verified {
			var hash []byte
			var attempts int
			err := tx.QueryRow(ctx, `
				SELECT token_hash, attempts FROM auth_tokens
				WHERE user_id = $1 AND purpose = 'verify_email' AND used_at IS NULL AND expires_at > now()
				ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, sess.UserID).Scan(&hash, &attempts)
			if errors.Is(err, pgx.ErrNoRows) {
				codeErr = codeError(422, "code_expired", "Kode sudah kedaluwarsa. Minta kode baru.")
				return nil
			}
			if err != nil {
				return err
			}
			if !hmac.Equal(hash, auth.OTPHash(s.Keys.OTP, "verify_email", sess.UserID, req.Body.Code)) {
				attempts++

				if attempts >= emailCodeAttempts {
					_, err = tx.Exec(ctx, `UPDATE auth_tokens SET attempts = $2, used_at = now() WHERE token_hash = $1`, hash, attempts)
					codeErr = codeError(429, "too_many_attempts", "Terlalu banyak percobaan. Minta kode baru.")
				} else {
					_, err = tx.Exec(ctx, `UPDATE auth_tokens SET attempts = $2 WHERE token_hash = $1`, hash, attempts)
					codeErr = codeError(422, "invalid_code", fmt.Sprintf("Kode salah. Sisa %d percobaan.", emailCodeAttempts-attempts))
				}
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE auth_tokens SET used_at = now() WHERE token_hash = $1`, hash); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1`, sess.UserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE parties SET verified = true WHERE user_id = $1`, sess.UserID); err != nil {
				return err
			}
		}
		out, err = loadUser(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if codeErr != nil {
		return nil, codeErr
	}
	return api.VerifyEmail200JSONResponse(out), nil
}

func (s *Server) ResendVerificationEmail(ctx context.Context, _ api.ResendVerificationEmailRequestObject) (api.ResendVerificationEmailResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var email, name, code string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var verified bool
		var wait float64
		if err := tx.QueryRow(ctx, `
			SELECT u.email, u.name, u.email_verified_at IS NOT NULL,
			       coalesce((SELECT extract(epoch FROM $2 - (now() - max(t.created_at))) FROM auth_tokens t
			                 WHERE t.user_id = u.id AND t.purpose = 'verify_email'), 0)
			FROM users u WHERE u.id = $1 FOR UPDATE`, sess.UserID, emailCodeCooldown).Scan(&email, &name, &verified, &wait); err != nil {
			return err
		}
		if verified {
			return nil
		}
		if wait > 0 {
			secs := int(wait) + 1
			if w := state(ctx).w; w != nil {
				w.Header().Set("Retry-After", strconv.Itoa(secs))
			}
			return &Error{Status: 429, Code: "resend_cooldown", Message: fmt.Sprintf("Tunggu %d detik sebelum minta kode baru.", secs)}
		}
		code, err = s.issueEmailCode(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if code != "" {
		s.send(ctx, verificationMail(email, name, code))
	}
	return api.ResendVerificationEmail204Response{}, nil
}

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
	s.send(ctx, mail.ResetPassword(email, name, link, resetTokenTTL))
	return api.ForgotPassword204Response{}, nil
}

func (s *Server) ResetPassword(ctx context.Context, req api.ResetPasswordRequestObject) (api.ResetPasswordResponseObject, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {

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
		if uniqueViolation(err, "") {
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
