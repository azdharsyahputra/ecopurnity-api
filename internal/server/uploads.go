package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/storage"
)

// Uploads go straight from the browser to object storage through presigned URLs; the API only hands out slots and
// verifies what arrived before a feature uses it.

const uploadURLTTL = 10 * time.Minute

type uploadRule struct {
	types    map[string]string // allowed content type -> file extension
	maxBytes int64
}

var imageTypes = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}

var uploadRules = map[api.UploadPurpose]uploadRule{
	api.UploadPurposeKycKtp:    {types: imageTypes, maxBytes: 8 << 20},
	api.UploadPurposeKycSelfie: {types: imageTypes, maxBytes: 8 << 20},
	api.UploadPurposeOrgDocument: {types: map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"},
		maxBytes: 10 << 20},
	api.UploadPurposeListingAttachment: {types: map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"},
		maxBytes: 10 << 20},
	api.UploadPurposeTradeProof:      {types: docTypes, maxBytes: 10 << 20},
	api.UploadPurposeDisputeEvidence: {types: docTypes, maxBytes: 10 << 20},
}

// ponytail: no video evidence; MP4 would need a per-type size limit (50 MB) in uploadRule.
var docTypes = map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}

var errStorageUnavailable = &Error{Status: http.StatusServiceUnavailable, Code: "storage_unavailable", Message: "Penyimpanan file belum tersedia. Coba lagi nanti."}

func (s *Server) CreateUpload(ctx context.Context, req api.CreateUploadRequestObject) (api.CreateUploadResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.Storage == nil {
		return nil, errStorageUnavailable
	}
	in := req.Body
	rule, ok := uploadRules[in.Purpose]
	if !ok {
		return nil, &Error{Status: 422, Code: "validation", Message: "Periksa kembali isian", Fields: map[string]string{"purpose": "Jenis upload tidak dikenal"}}
	}
	fields := map[string]string{}
	contentType := strings.ToLower(strings.TrimSpace(in.ContentType))
	ext, okType := rule.types[contentType]
	if !okType {
		fields["contentType"] = "Format file tidak didukung. Pakai JPG, PNG, atau WebP."
		if _, pdf := rule.types["application/pdf"]; pdf {
			fields["contentType"] = "Format file tidak didukung. Pakai PDF, JPG, PNG, atau WebP."
		}
	}
	if in.SizeBytes <= 0 || int64(in.SizeBytes) > rule.maxBytes {
		fields["sizeBytes"] = fmt.Sprintf("Ukuran file maksimal %d MB.", rule.maxBytes>>20)
	}
	name := strings.TrimSpace(path.Base(strings.ReplaceAll(in.FileName, "\\", "/")))
	if name == "" || name == "." || name == "/" {
		fields["fileName"] = "Nama file wajib diisi"
	}
	if len(fields) > 0 {
		return nil, &Error{Status: 422, Code: "validation", Message: "File tidak bisa diunggah", Fields: fields}
	}

	var id, key string
	if err := s.DB.Primary().QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
		return nil, err
	}
	// The key never contains user input: purpose/user/upload-id.ext.
	key = fmt.Sprintf("%s/%s/%s%s", in.Purpose, sess.UserID, id, ext)
	if _, err := s.DB.Primary().Exec(ctx, `
		INSERT INTO uploads (id, owner_user_id, purpose, object_key, file_name, content_type, size_bytes) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, sess.UserID, in.Purpose, key, name, contentType, in.SizeBytes); err != nil {
		return nil, err
	}
	url, headers, err := s.Storage.PresignPut(ctx, key, contentType, int64(in.SizeBytes), uploadURLTTL)
	if err != nil {
		return nil, err
	}
	h := map[string]string{}
	for k := range headers {
		h[k] = headers.Get(k)
	}
	return api.CreateUpload201JSONResponse{UploadId: id, Method: "PUT", Url: url, Headers: h, ExpiresAt: time.Now().Add(uploadURLTTL).UTC()}, nil
}

// upload is a verified file a feature may attach.
type upload struct {
	ID, ObjectKey, FileName, ContentType string
	SizeBytes                            int64
}

// claimUpload checks that uploadID is the user's, has the purpose, is in storage with the declared size and a sniffed
// type matching the declared one, and marks it consumed (a file is attached once). field names the input for errors.
func (s *Server) claimUpload(ctx context.Context, tx pgx.Tx, userID, uploadID string, purpose api.UploadPurpose, field string) (upload, error) {
	bad := func(msg string) error {
		return &Error{Status: 422, Code: "validation", Message: "Periksa kembali file yang diunggah", Fields: map[string]string{field: msg}}
	}
	if s.Storage == nil {
		return upload{}, errStorageUnavailable
	}
	var u upload
	var owner, gotPurpose string
	var consumed *time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, owner_user_id, purpose, object_key, file_name, content_type, size_bytes, consumed_at
		FROM uploads WHERE id::text = $1 FOR UPDATE`, uploadID).
		Scan(&u.ID, &owner, &gotPurpose, &u.ObjectKey, &u.FileName, &u.ContentType, &u.SizeBytes, &consumed)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != userID):
		return u, bad("File tidak ditemukan. Unggah ulang.")
	case err != nil:
		return u, err
	case gotPurpose != string(purpose):
		return u, bad("File ini diunggah untuk keperluan lain.")
	case consumed != nil:
		return u, bad("File ini sudah dipakai. Unggah ulang.")
	}
	obj, err := s.Storage.Inspect(ctx, u.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return u, bad("File belum selesai diunggah.")
	}
	if err != nil {
		return u, err
	}
	if obj.Size != u.SizeBytes {
		return u, bad("Ukuran file tidak sesuai. Unggah ulang.")
	}
	if sniffed := http.DetectContentType(obj.Head); sniffed != u.ContentType {
		if !strings.HasPrefix(u.ContentType, "image/") {
			return u, bad("Isi file tidak sesuai formatnya.")
		}
		return u, bad("Isi file bukan gambar yang valid.")
	}
	_, err = tx.Exec(ctx, `UPDATE uploads SET status = 'uploaded', uploaded_at = coalesce(uploaded_at, now()), consumed_at = now() WHERE id = $1`, u.ID)
	return u, err
}
