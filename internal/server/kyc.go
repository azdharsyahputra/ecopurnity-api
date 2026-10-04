package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

var kycLevels = [2]struct {
	Label string
	Limit int
	Next  string
}{
	{"Email", 10_000_000, "Verifikasi KTP untuk naik ke Rp 2 M per transaksi."},
	{"KTP", 2_000_000_000, ""},
}

type verification struct {
	Email    bool
	Identity string
}

func loadVerification(ctx context.Context, q dbtx, userID string) (verification, error) {
	var v verification
	err := q.QueryRow(ctx, `
		SELECT u.email_verified_at IS NOT NULL,
		       CASE WHEN i.identity_verified_at IS NOT NULL THEN 'verified'
		            WHEN EXISTS (SELECT 1 FROM verification_requests r WHERE r.submitted_by = u.id AND r.kind = 'personal' AND r.status = 'pending') THEN 'pending'
		            ELSE 'none' END
		FROM users u LEFT JOIN identities i ON i.user_id = u.id WHERE u.id = $1`, userID).
		Scan(&v.Email, &v.Identity)
	return v, err
}

func (v verification) level() int {
	if v.Identity == "verified" {
		return 1
	}
	return 0
}

func (s *Server) kycStatus(ctx context.Context, q dbtx, userID string) (api.Kyc, error) {
	v, err := loadVerification(ctx, q, userID)
	if err != nil {
		return api.Kyc{}, err
	}
	l := v.level()
	k := api.Kyc{Level: api.KycLevel(l), Label: kycLevels[l].Label, LimitIdr: kycLevels[l].Limit}
	if kycLevels[l].Next != "" {
		k.Next = ptr(kycLevels[l].Next)
	}
	k.Verification.Email = v.Email
	k.Verification.Identity = api.KycVerificationIdentity(v.Identity)
	return k, nil
}

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
