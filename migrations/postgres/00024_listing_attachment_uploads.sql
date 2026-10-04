-- +goose Up
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document','listing_attachment'));

ALTER TABLE listing_attachments
  ADD COLUMN object_key   text UNIQUE,
  ADD COLUMN content_type text,
  ADD COLUMN size_bytes   bigint CHECK (size_bytes > 0),
  ADD COLUMN position     integer CHECK (position >= 0);

UPDATE listing_attachments a SET
  position = n.pos,
  content_type = CASE
    WHEN a.file_name ILIKE '%.pdf' THEN 'application/pdf'
    WHEN a.file_name ILIKE '%.png' THEN 'image/png'
    WHEN a.file_name ILIKE '%.webp' THEN 'image/webp'
    WHEN a.file_name ILIKE '%.jpg' OR a.file_name ILIKE '%.jpeg' THEN 'image/jpeg'
    ELSE 'application/octet-stream' END
FROM (SELECT id, row_number() OVER (PARTITION BY listing_id ORDER BY created_at, id) - 1 AS pos FROM listing_attachments) n
WHERE n.id = a.id;

ALTER TABLE listing_attachments ALTER COLUMN content_type SET NOT NULL, ALTER COLUMN position SET NOT NULL;
ALTER TABLE listing_attachments ADD CONSTRAINT listing_attachments_position_key UNIQUE (listing_id, position) DEFERRABLE INITIALLY DEFERRED;
DROP INDEX listing_attachments_listing_idx;

-- +goose Down
CREATE INDEX listing_attachments_listing_idx ON listing_attachments (listing_id, created_at);
ALTER TABLE listing_attachments DROP CONSTRAINT listing_attachments_position_key;
ALTER TABLE listing_attachments DROP COLUMN position, DROP COLUMN size_bytes, DROP COLUMN content_type, DROP COLUMN object_key;
DELETE FROM uploads WHERE purpose = 'listing_attachment';
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('kyc_ktp','kyc_selfie','org_document'));
