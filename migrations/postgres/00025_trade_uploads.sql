-- +goose Up
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
  CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document','listing_attachment','trade_proof','dispute_evidence'));

-- +goose Down
DELETE FROM uploads WHERE purpose IN ('trade_proof','dispute_evidence');
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
  CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document','listing_attachment'));
