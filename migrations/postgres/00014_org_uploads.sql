-- +goose Up
-- Org verification documents arrive as verified uploads (POST /uploads, purpose org_document) like KYC photos: the
-- document row keeps the object key so the business verification request can point the admin at the file.
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document'));
ALTER TABLE org_documents ADD COLUMN object_key text;  -- null only for documents registered before uploads existed

-- +goose Down
ALTER TABLE org_documents DROP COLUMN object_key;
DELETE FROM uploads WHERE purpose = 'org_document';
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie'));
