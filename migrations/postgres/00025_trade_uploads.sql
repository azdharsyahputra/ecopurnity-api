-- +goose Up
-- Delivery proofs and dispute evidence arrive as verified uploads (POST /uploads, purposes trade_proof and
-- dispute_evidence). The rows already have their key columns from 00004: trade_documents.object_key and
-- dispute_evidence.file_key (null for generated PDFs and for the demo bot's name-only files).
-- Rebuilds the check with every purpose (00024 added listing_attachment); Down restores the 00024 list.
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
  CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document','listing_attachment','trade_proof','dispute_evidence'));

-- +goose Down
DELETE FROM uploads WHERE purpose IN ('trade_proof','dispute_evidence');
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
  CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document','listing_attachment'));
