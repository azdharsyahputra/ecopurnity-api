-- +goose Up
-- Email verification by 6-digit code (OTP). A code is an auth_tokens row (purpose 'verify_email') whose token_hash is
-- an HMAC of (user id, code) keyed by the server secret; attempts count wrong guesses (5 burns the code).
ALTER TABLE auth_tokens ADD COLUMN attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5);
-- Latest live code per user, for verification and the resend cooldown.
CREATE INDEX auth_tokens_live_idx ON auth_tokens (user_id, purpose, created_at DESC) WHERE used_at IS NULL;

-- +goose Down
DROP INDEX auth_tokens_live_idx;
ALTER TABLE auth_tokens DROP COLUMN attempts;
