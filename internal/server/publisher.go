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

const (
	outboxBatch     = 500
	outboxRetention = "2 days"
	publisherLock   = `SELECT pg_try_advisory_lock(hashtext('ecopurnity.outbox_publisher'))`
)

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
			wait = time.Second
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

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
	if _, err := conn.Exec(ctx, `LISTEN outbox_new`); err != nil {
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
			continue
		}

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
