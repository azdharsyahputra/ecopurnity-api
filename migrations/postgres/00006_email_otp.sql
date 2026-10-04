-- +goose Up
ALTER TABLE auth_tokens ADD COLUMN attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5);
CREATE INDEX auth_tokens_live_idx ON auth_tokens (user_id, purpose, created_at DESC) WHERE used_at IS NULL;

-- +goose Down
DROP INDEX auth_tokens_live_idx;
ALTER TABLE auth_tokens DROP COLUMN attempts;
