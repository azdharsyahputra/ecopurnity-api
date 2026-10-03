package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

// Outbox publisher: one leader per cluster (Postgres advisory lock) moves committed outbox rows on.
//   - Realtime: 'rt' rows -> pg_notify('ecp_rt', {c: channel, o: outbox id}) in id order, then published_at.
//   - Analytics: every other row -> ClickHouse events (migrations/clickhouse/README.md dedupe), then ch_published_at.
// The two run in separate loops so a ClickHouse outage (or a 30 s dial timeout) never delays a bid frame; analytics
// simply catch up from ch_published_at IS NULL. Rows both sides are done with are deleted after outboxRetention.

const (
	outboxBatch     = 500
	outboxRetention = "2 days" // replay horizon too: see migrations/postgres/00008_outbox_replay.sql
	publisherLock   = `SELECT pg_try_advisory_lock(hashtext('ecopurnity.outbox_publisher'))`
)

// Publish competes for the publisher lock and, while it holds it, publishes until ctx ends. Run it in its own
// goroutine on every instance; all but one wait.
func (s *Server) Publish(ctx context.Context) {
	wait := time.Second
	for {
		err := s.lead(ctx)
		s.leader.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.Log.Warn("outbox publisher", "err", err)
			wait = min(2*wait, 30*time.Second)
		} else {
			wait = time.Second // another instance leads; check again soon so a dead leader is replaced quickly
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// lead takes the lock on a dedicated connection and runs the realtime loop on that same connection: if it dies,
// Postgres releases the lock and the in-flight batch rolls back with it, so two leaders never notify the same rows.
// Returns nil without error when another instance holds the lock.
func (s *Server) lead(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, s.DB.Primary().Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var got bool
	if err := conn.QueryRow(ctx, publisherLock).Scan(&got); err != nil || !got {
		return err
	}
	if _, err := conn.Exec(ctx, `LISTEN outbox_new`); err != nil { // the 00008 trigger notifies on every outbox insert
		return err
	}
	s.leader.Store(true)

	actx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stop()
	if s.Analytics != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.feedAnalytics(actx) }()
	}

	var cleaned time.Time
	for {
		n, err := publishRealtime(ctx, conn)
		if err != nil {
			return err
		}
		if time.Since(cleaned) > time.Hour {
			if err := s.cleanOutbox(ctx, conn); err != nil {
				s.Log.Warn("outbox cleanup", "err", err)
			}
			cleaned = time.Now()
		}
		if n == outboxBatch {
			continue // backlog: no waiting
		}
		// Wake on a commit (outbox_new) or poll every 250 ms as the fallback.
		wctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		_, err = conn.WaitForNotification(wctx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
}

// publishRealtime notifies one batch of unpublished rows and marks them, in one transaction: the NOTIFYs are sent at
// COMMIT, in id order, so per channel they follow seq order (writers of a channel commit in seq order).
func publishRealtime(ctx context.Context, conn *pgx.Conn) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `
			SELECT id, topic, aggregate_id FROM outbox WHERE published_at IS NULL
			ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, outboxBatch)
		type row struct {
			ID           int64
			Topic, Chann string
		}
		batch, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		if err != nil || len(batch) == 0 {
			return err
		}
		n = len(batch)
		var b pgx.Batch
		ids := make([]int64, 0, n)
		for _, r := range batch {
			ids = append(ids, r.ID)
			if r.Topic == "rt" {
				note, _ := json.Marshal(map[string]any{"c": r.Chann, "o": r.ID})
				b.Queue(`SELECT pg_notify('ecp_rt', $1)`, string(note))
			}
		}
		b.Queue(`UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
		return tx.SendBatch(ctx, &b).Close()
	})
	return n, err
}

// feedAnalytics copies non-rt rows to ClickHouse until ctx ends, backing off while ClickHouse is down.
func (s *Server) feedAnalytics(ctx context.Context) {
	wait := time.Second
	for {
		n, err := s.publishAnalytics(ctx)
		if ctx.Err() != nil {
			return
		}
		pause := time.Second
		switch {
		case err != nil:
			s.Log.Warn("outbox -> clickhouse", "err", err)
			pause, wait = wait, min(2*wait, time.Minute)
		case n == outboxBatch:
			pause, wait = 0, time.Second
		default:
			wait = time.Second
		}
		if !sleepCtx(ctx, pause) {
			return
		}
	}
}

// publishAnalytics is migrations/clickhouse/README.md "Dedupe" steps 2-5 for one batch. The row locks (SKIP LOCKED)
// are held across the ClickHouse insert, so a second publisher cannot take the same rows meanwhile.
func (s *Server) publishAnalytics(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.DB.Primary(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `
			SELECT id, topic, aggregate_id, created_at, payload::text FROM outbox
			WHERE ch_published_at IS NULL AND topic <> 'rt'
			ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, outboxBatch)
		evs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[analytics.Event])
		if err != nil || len(evs) == 0 {
			return err
		}
		n = len(evs)
		ids := make([]int64, len(evs))
		for i, e := range evs {
			ids[i] = e.OutboxID
		}
		known, err := s.Analytics.KnownEvents(ctx, ids)
		if err != nil {
			return err
		}
		fresh := evs[:0]
		for _, e := range evs {
			if !known[e.OutboxID] {
				fresh = append(fresh, e)
			}
		}
		if err := s.Analytics.InsertEvents(ctx, fresh); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE outbox SET ch_published_at = now() WHERE id = ANY($1)`, ids)
		return err
	})
	return n, err
}

// cleanOutbox deletes rows both consumers are done with once they are older than the retention. Old rows sit at the
// low end of the primary key, so the bounded scan in id order stays short.
func (s *Server) cleanOutbox(ctx context.Context, conn *pgx.Conn) error {
	for {
		tag, err := conn.Exec(ctx, `
			DELETE FROM outbox WHERE id IN (
				SELECT id FROM outbox
				WHERE created_at < now() - $1::interval AND published_at IS NOT NULL AND (topic = 'rt' OR ch_published_at IS NOT NULL)
				ORDER BY id LIMIT 5000)`, outboxRetention)
		if err != nil || tag.RowsAffected() < 5000 {
			return err
		}
	}
}
