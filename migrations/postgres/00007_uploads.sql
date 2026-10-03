-- +goose Up
-- Files uploaded straight to object storage (S3-compatible / Cloudflare R2) through presigned URLs. A row is created
-- when the client asks for an upload slot; the endpoint that consumes the file verifies the object (exists, size,
-- sniffed type) and marks it uploaded. Consumers reference the row (or copy its object_key).
CREATE TABLE uploads (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose       text NOT NULL CHECK (purpose IN ('kyc_ktp','kyc_selfie')),
  object_key    text NOT NULL UNIQUE,
  file_name     text NOT NULL CHECK (length(btrim(file_name)) BETWEEN 1 AND 200),
  content_type  text NOT NULL,
  size_bytes    bigint NOT NULL CHECK (size_bytes > 0),
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','uploaded')),
  -- Set when a consumer used the file (e.g. a KYC submission); a used upload cannot be attached again.
  consumed_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  uploaded_at   timestamptz,
  CHECK ((status = 'uploaded') = (uploaded_at IS NOT NULL)),
  CHECK (consumed_at IS NULL OR status = 'uploaded')
);
CREATE INDEX uploads_owner_idx ON uploads (owner_user_id, created_at DESC);
-- Janitor: slots never uploaded or never used are deleted (with their objects) after a day.
CREATE INDEX uploads_stale_idx ON uploads (created_at) WHERE consumed_at IS NULL;
COMMENT ON TABLE uploads IS 'A file a user uploaded (or is about to upload) to object storage through a presigned URL.';

-- +goose Down
DROP TABLE uploads;
