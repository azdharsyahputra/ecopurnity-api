-- +goose Up
-- Realtime replay and the outbox publisher (docs/realtime.md, internal/server/publisher.go).
--
-- Two consumers read the outbox, and they fail independently:
--   published_at     the realtime side is done: 'rt' rows were pg_notify'd on ecp_rt (other topics have nothing to
--                    notify and are only marked). Never held up by ClickHouse.
--   ch_published_at  the row is in ClickHouse `events` (README dedupe steps). Stays NULL while ClickHouse is down and
--                    catches up afterwards. 'rt' rows never go to ClickHouse: they are rendered copies of facts that
--                    have their own topics, and they carry chat text and notification bodies, which the analytics
--                    store must not keep (migrations/clickhouse/README.md).
--
-- Retention: the publisher deletes rows older than 2 days once both sides are done. A `subscribe {sinceSeq}` that needs
-- an older frame finds it missing and answers resync_required (the client refetches REST), so 2 days is the longest a
-- client can be away and still resume. Raise it here and in publisher.go together.

-- Replay: the frames of one channel, newest first (subscribe {sinceSeq} reads at most 200 back from the head).
CREATE INDEX outbox_rt_channel_idx ON outbox (aggregate_id, id) WHERE topic = 'rt';

ALTER TABLE outbox ADD COLUMN ch_published_at timestamptz;
COMMENT ON COLUMN outbox.published_at IS 'Realtime side done: rt rows notified on ecp_rt (other topics only marked).';
COMMENT ON COLUMN outbox.ch_published_at IS 'Inserted into ClickHouse events; NULL while ClickHouse is behind. Never set for rt rows.';
-- The analytics queue.
CREATE INDEX outbox_ch_pending_idx ON outbox (id) WHERE ch_published_at IS NULL AND topic <> 'rt';

-- Wakes the publisher (LISTEN outbox_new) when a writing transaction commits, instead of waiting for its 250 ms poll.
-- Statement level and payload-free: Postgres folds identical notifications of one transaction into one.
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
