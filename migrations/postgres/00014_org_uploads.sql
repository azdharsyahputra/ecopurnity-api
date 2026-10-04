-- +goose Up
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document'));
ALTER TABLE org_documents ADD COLUMN object_key text;

-- +goose Down
ALTER TABLE org_documents DROP COLUMN object_key;
DELETE FROM uploads WHERE purpose = 'org_document';
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie'));
