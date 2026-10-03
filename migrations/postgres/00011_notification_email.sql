-- +goose Up
-- Notification emails (PRD §8.11): a background job mails each notification whose type the user has email on for
-- (notification_prefs, missing row = defaults) and marks it here. Up to 3 attempts; after that it is left alone.
ALTER TABLE notifications ADD COLUMN emailed_at timestamptz;
ALTER TABLE notifications ADD COLUMN email_attempts smallint NOT NULL DEFAULT 0 CHECK (email_attempts >= 0);
-- The mail queue: not yet emailed, attempts left (the job also only looks at the last day).
CREATE INDEX notifications_email_pending_idx ON notifications (created_at) WHERE emailed_at IS NULL AND email_attempts < 3;
COMMENT ON TABLE notification_prefs IS 'A user''s channel preference for one notification type (missing row = defaults: in-app on; email on only for outbid, winning_bid, payment, transaction_update).';

-- +goose Down
COMMENT ON TABLE notification_prefs IS 'A user''s channel preference for one notification type (missing row = defaults).';
DROP INDEX notifications_email_pending_idx;
ALTER TABLE notifications DROP COLUMN email_attempts;
ALTER TABLE notifications DROP COLUMN emailed_at;
