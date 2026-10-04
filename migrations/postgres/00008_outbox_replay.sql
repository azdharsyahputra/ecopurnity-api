-- +goose Up

CREATE INDEX outbox_rt_channel_idx ON outbox (aggregate_id, id) WHERE topic = 'rt';

ALTER TABLE outbox ADD COLUMN ch_published_at timestamptz;
COMMENT ON COLUMN outbox.published_at IS 'Realtime side done: rt rows notified on ecp_rt (other topics only marked).';
COMMENT ON COLUMN outbox.ch_published_at IS 'Inserted into ClickHouse events; NULL while ClickHouse is behind. Never set for rt rows.';
CREATE INDEX outbox_ch_pending_idx ON outbox (id) WHERE ch_published_at IS NULL AND topic <> 'rt';

-- +goose StatementBegin
CREATE FUNCTION outbox_wake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('outbox_new', '');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER outbox_wake AFTER INSERT ON outbox FOR EACH STATEMENT EXECUTE FUNCTION outbox_wake();

-- +goose Down
DROP TRIGGER outbox_wake ON outbox;
DROP FUNCTION outbox_wake();
DROP INDEX outbox_ch_pending_idx;
COMMENT ON COLUMN outbox.published_at IS NULL;
ALTER TABLE outbox DROP COLUMN ch_published_at;
DROP INDEX outbox_rt_channel_idx;
