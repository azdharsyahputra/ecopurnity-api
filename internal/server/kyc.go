package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

// KYC (spec tag Verification): commitment limits by verification level, phone OTP, KTP + selfie review.

// kycLevels mirrors the frontend's src/domain/kyc.ts KYC_LEVELS.
var kycLevels = [3]struct {
	Label string
	Limit int
	Next  string
}{
	{"Email", 10_000_000, "Verifikasi nomor HP untuk naik ke Rp 100 jt per transaksi."},
	{"Email + HP", 100_000_000, "Verifikasi KTP untuk naik ke Rp 2 M per transaksi."},
	{"KTP terverifikasi", 2_000_000_000, ""},
}

const (
	phoneCodeTTL      = 5 * time.Minute
	phoneCodeAttempts = 5
	phoneCodeCooldown = 60 * time.Second
)

// verification is the user's current verification state (Identity.profile.verification).
type verification struct {
	Email, Phone bool
	Identity     string // none | pending | verified
	OTPPending   bool
}

func loadVerification(ctx context.Context, q dbtx, userID string) (verification, error) {
	var v verification
	err := q.QueryRow(ctx, `
		SELECT u.email_verified_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM phone_verifications p WHERE p.user_id = u.id AND p.verified_at IS NOT NULL),
		       CASE WHEN i.identity_verified_at IS NOT NULL THEN 'verified'
		            WHEN EXISTS (SELECT 1 FROM verification_requests r WHERE r.submitted_by = u.id AND r.kind = 'personal' AND r.status = 'pending') THEN 'pending'
		            ELSE 'none' END,
		       EXISTS (SELECT 1 FROM phone_verifications p WHERE p.user_id = u.id AND p.verified_at IS NULL AND p.expires_at > now() AND p.attempts < $2)
		FROM users u LEFT JOIN identities i ON i.user_id = u.id WHERE u.id = $1`, userID, phoneCodeAttempts).
		Scan(&v.Email, &v.Phone, &v.Identity, &v.OTPPending)
	return v, err
}

// kycLevel: 2 when the KTP is verified, 1 with a verified phone, else 0 (frontend kycLevel()).
func (v verification) level() int {
	switch {
	case v.Identity == "verified":
		return 2
	case v.Phone:
		return 1
	default:
		return 0
	}
}

func (s *Server) kycStatus(ctx context.Context, q dbtx, userID string) (api.Kyc, error) {
	v, err := loadVerification(ctx, q, userID)
	if err != nil {
		return api.Kyc{}, err
	}
	l := v.level()
	k := api.Kyc{Level: api.KycLevel(l), Label: kycLevels[l].Label, LimitIdr: kycLevels[l].Limit, OtpPending: v.OTPPending}
	if kycLevels[l].Next != "" {
		k.Next = ptr(kycLevels[l].Next)
	}
	k.Verification.Email, k.Verification.Phone = v.Email, v.Phone
	k.Verification.Identity = api.KycVerificationIdentity(v.Identity)
	return k, nil
}

// commitGuard is the per-commitment limit check every bid / accept / auction / quote / direct order runs:
// 403 kyc_limit when valueIdr is above the user's level (frontend limitError()).
func (s *Server) commitGuard(ctx context.Context, q dbtx, userID string, valueIdr int64) error {
	v, err := loadVerification(ctx, q, userID)
	if err != nil {
		return err
	}
	l := kycLevels[v.level()]
	if valueIdr <= int64(l.Limit) {
		return nil
	}
	next := l.Next
	if next == "" {
		next = "Gunakan akun organisasi untuk transaksi sebesar ini."
	}
	return &Error{Status: http.StatusForbidden, Code: "kyc_limit",
		Message: fmt.Sprintf("Nilai %s melebihi batas %s untuk level %s. %s", rupiah(valueIdr), rupiah(int64(l.Limit)), l.Label, next)}
}

func (s *Server) GetMyKyc(ctx context.Context, _ api.GetMyKycRequestObject) (api.GetMyKycResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	k, err := s.kycStatus(ctx, s.DB.Primary(), sess.UserID)
	if err != nil {
		return nil, err
	}
	return api.GetMyKyc200JSONResponse(k), nil
}

var phonePattern = regexp.MustCompile(`^(62|0)8\d{7,11}$`)

// normalizePhone returns the number as 628…, or "" when it is not an Indonesian mobile number.
func normalizePhone(in string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, in)
	if !phonePattern.MatchString(digits) {
		return ""
	}
	if strings.HasPrefix(digits, "0") {
		return "62" + digits[1:]
	}
	return digits
}

func (s *Server) RequestPhoneOtp(ctx context.Context, req api.RequestPhoneOtpRequestObject) (api.RequestPhoneOtpResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	phone := normalizePhone(req.Body.Phone)
	if phone == "" {
		return nil, &Error{Status: 422, Code: "validation", Message: "Nomor HP tidak valid", Fields: map[string]string{"phone": "Contoh: 0812xxxxxxxx"}}
	}
	code := auth.NewOTP()
	var out api.Kyc
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Serialise per user so two quick requests can't both pass the cooldown.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, sess.UserID); err != nil {
			return err
		}
		var wait float64
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(extract(epoch FROM $2 - (now() - max(created_at))), 0) FROM phone_verifications WHERE user_id = $1`,
			sess.UserID, phoneCodeCooldown).Scan(&wait); err != nil {
			return err
		}
		if wait > 0 {
			secs := int(wait) + 1
			if w := state(ctx).w; w != nil {
				w.Header().Set("Retry-After", strconv.Itoa(secs))
			}
			return &Error{Status: 429, Code: "otp_cooldown", Message: fmt.Sprintf("Tunggu %d detik sebelum minta kode baru.", secs)}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM phone_verifications WHERE user_id = $1 AND verified_at IS NULL`, sess.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO phone_verifications (user_id, phone, code_hash, expires_at) VALUES ($1, $2, $3, now() + $4)`,
			sess.UserID, phone, auth.OTPHash(s.Keys.OTP, "phone", sess.UserID, code), phoneCodeTTL); err != nil {
			return err
		}
		out, err = s.kycStatus(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if s.SMS != nil {
		text := fmt.Sprintf("Kode verifikasi Ecopurnity: %s. Berlaku 5 menit. Jangan bagikan kode ini ke siapa pun.", code)
		if err := s.SMS.Send(ctx, phone, text); err != nil {
			return nil, err
		}
	}
	return api.RequestPhoneOtp200JSONResponse(out), nil
}

func (s *Server) VerifyPhoneOtp(ctx context.Context, req api.VerifyPhoneOtpRequestObject) (api.VerifyPhoneOtpResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Kyc
	var codeErr *Error
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		var hash []byte
		var attempts int
		err := tx.QueryRow(ctx, `
			SELECT id, code_hash, attempts FROM phone_verifications
			WHERE user_id = $1 AND verified_at IS NULL AND expires_at > now() AND attempts < $2 FOR UPDATE`,
			sess.UserID, phoneCodeAttempts).Scan(&id, &hash, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Status: http.StatusConflict, Code: "otp_expired", Message: "Kode kedaluwarsa, kirim ulang"}
		}
		if err != nil {
			return err
		}
		if !hmac.Equal(hash, auth.OTPHash(s.Keys.OTP, "phone", sess.UserID, strings.TrimSpace(req.Body.Code))) {
			attempts++
			if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
				return err
			}
			// Committed so the attempt counts.
			if attempts >= phoneCodeAttempts {
				codeErr = codeError(429, "too_many_attempts", "Terlalu banyak percobaan. Minta kode baru.")
			} else {
				codeErr = codeError(422, "invalid_code", fmt.Sprintf("Kode salah. Sisa %d percobaan.", phoneCodeAttempts-attempts))
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET verified_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		out, err = s.kycStatus(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if codeErr != nil {
		return nil, codeErr
	}
	return api.VerifyPhoneOtp200JSONResponse(out), nil
}

// maskNIK keeps the region code and the last 4 digits: 3205********0001.
func maskNIK(nik string) string { return nik[:4] + strings.Repeat("*", 8) + nik[12:] }

var nikPattern = regexp.MustCompile(`^\d{16}$`)

func (s *Server) SubmitKtpVerification(ctx context.Context, req api.SubmitKtpVerificationRequestObject) (api.SubmitKtpVerificationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Body
	nik := strings.TrimSpace(in.Nik)
	fullName := strings.TrimSpace(in.FullName)
	fields := map[string]string{}
	if !nikPattern.MatchString(nik) {
		fields["nik"] = "NIK 16 digit"
	}
	if fullName == "" {
		fields["fullName"] = "Sesuai KTP"
	}
	if strings.TrimSpace(in.KtpUploadId) == "" {
		fields["ktpUploadId"] = "Unggah foto KTP"
	}
	if strings.TrimSpace(in.SelfieUploadId) == "" {
		fields["selfieUploadId"] = "Unggah selfie dengan KTP"
	}
	if len(fields) > 0 {
		return nil, &Error{Status: 422, Code: "validation", Message: "Data belum lengkap", Fields: fields}
	}
	nikHash := secure.MAC(s.Keys.NIKHash, nik)

	var out api.Kyc
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		v, err := loadVerification(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		if v.Identity != "none" {
			msg := "Pengajuan sedang ditinjau"
			if v.Identity == "verified" {
				msg = "Identitas sudah terverifikasi"
			}
			return &Error{Status: http.StatusConflict, Code: "already_submitted", Message: msg}
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities WHERE nik_hash = $1 AND user_id <> $2)`, nikHash, sess.UserID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return &Error{Status: http.StatusConflict, Code: "nik_in_use", Message: "NIK ini sudah dipakai akun lain yang terverifikasi."}
		}
		ktp, err := s.claimUpload(ctx, tx, sess.UserID, in.KtpUploadId, api.UploadPurposeKycKtp, "ktpUploadId")
		if err != nil {
			return err
		}
		selfie, err := s.claimUpload(ctx, tx, sess.UserID, in.SelfieUploadId, api.UploadPurposeKycSelfie, "selfieUploadId")
		if err != nil {
			return err
		}
		sealed, err := secure.Encrypt(s.Keys.NIKCipher, []byte(nik), []byte(sess.UserID))
		if err != nil {
			return err
		}
		// The NIK never goes into the jsonb form/OCR fields in clear; reviewers see it masked and decrypt on demand.
		form := fmt.Sprintf(`[{"label":"Nama lengkap","value":%q},{"label":"NIK","value":%q}]`, fullName, maskNIK(nik))
		var requestID string
		err = tx.QueryRow(ctx, `
			INSERT INTO verification_requests (kind, submitted_by, subject_name, form) VALUES ('personal', $1, $2, $3::jsonb) RETURNING id`,
			sess.UserID, fullName, form).Scan(&requestID)
		if uniqueViolation(err, "verification_requests_pending_user_key") {
			return &Error{Status: http.StatusConflict, Code: "already_submitted", Message: "Pengajuan sedang ditinjau"}
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_submissions (verification_request_id, nik_encrypted, nik_hash, full_name) VALUES ($1, $2, $3, $4)`,
			requestID, sealed, nikHash, fullName); err != nil {
			return err
		}
		for _, d := range []struct {
			kind string
			u    upload
		}{{"ktp", ktp}, {"selfie", selfie}} {
			if _, err := tx.Exec(ctx, `INSERT INTO verification_documents (request_id, kind, file_name, object_key) VALUES ($1, $2, $3, $4)`,
				requestID, d.kind, d.u.FileName, d.u.ObjectKey); err != nil {
				return err
			}
		}
		if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: sess.Name, Action: "Ajukan verifikasi KTP",
			EntityType: "user", EntityID: sess.UserID, EntityLabel: sess.Name}); err != nil {
			return err
		}
		out, err = s.kycStatus(ctx, tx, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.SubmitKtpVerification200JSONResponse(out), nil
}

// rupiah formats like the frontend's formatIdr: "Rp 10.000.000".
func rupiah(v int64) string {
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return "Rp " + b.String()
}
