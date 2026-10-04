-- +goose Up
ALTER TABLE notifications ADD COLUMN emailed_at timestamptz;
ALTER TABLE notifications ADD COLUMN email_attempts smallint NOT NULL DEFAULT 0 CHECK (email_attempts >= 0);
CREATE INDEX notifications_email_pending_idx ON notifications (created_at) WHERE emailed_at IS NULL AND email_attempts < 3;
COMMENT ON TABLE notification_prefs IS 'A user''s channel preference for one notification type (missing row = defaults: in-app on; email on only for outbid, winning_bid, payment, transaction_update).';

-- +goose Down
COMMENT ON TABLE notification_prefs IS 'A user''s channel preference for one notification type (missing row = defaults).';
DROP INDEX notifications_email_pending_idx;
ALTER TABLE notifications DROP COLUMN email_attempts;
ALTER TABLE notifications DROP COLUMN emailed_at;
