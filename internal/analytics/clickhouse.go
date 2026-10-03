// Package analytics is the ClickHouse side: append-only events and read-heavy aggregates (explorer stats,
// market price history, activity feed, audit search). PostgreSQL stays the system of record; ClickHouse is fed by
// the outbox publisher and can be rebuilt from it.
package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Client struct{ conn driver.Conn }

func Open(addr, database, user, password string) (*Client, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: database, Username: user, Password: password},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Ping(ctx context.Context) error { return c.conn.Ping(ctx) }
func (c *Client) Close() error                   { return c.conn.Close() }

// WeeklyMedians returns one median deal price per market per week over the last `weeks` weeks (market_price_daily).
func (c *Client) WeeklyMedians(ctx context.Context, marketIDs []string, weeks int) ([]float64, error) {
	if len(marketIDs) == 0 {
		return nil, nil
	}
	rows, err := c.conn.Query(ctx, `
		SELECT quantileMerge(0.5)(median_state)
		FROM market_price_daily
		WHERE market_id IN ? AND day >= today() - ?
		GROUP BY market_id, toStartOfWeek(day)`, marketIDs, weeks*7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// WeekPrice is one week of a market's deal prices (market_price_daily merged per ISO week).
type WeekPrice struct {
	Week      time.Time // Monday
	Median    float64
	Low, High uint64
}

// PriceHistory returns the weekly median/low/high of one market over the last `weeks` weeks (current one included),
// oldest first. Weeks without deals are absent.
func (c *Client) PriceHistory(ctx context.Context, marketID string, weeks int) ([]WeekPrice, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT toMonday(day) AS week, quantileMerge(0.5)(median_state), min(low_idr), max(high_idr)
		FROM market_price_daily
		WHERE market_id = ? AND day >= toMonday(today()) - ?
		GROUP BY week ORDER BY week`, marketID, (weeks-1)*7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WeekPrice
	for rows.Next() {
		var w WeekPrice
		if err := rows.Scan(&w.Week, &w.Median, &w.Low, &w.High); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Activity is one public activity feed row (ActivityEvent).
type Activity struct {
	ID, Type, Title string
	AmountIdr       *uint64
	At              time.Time
}

// MarketActivity returns the newest `limit` activity events of one market, newest first.
func (c *Client) MarketActivity(ctx context.Context, marketID string, limit int) ([]Activity, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT id, type, title, amount_idr, at FROM activity WHERE market_id = ? ORDER BY at DESC LIMIT ?`, marketID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Type, &a.Title, &a.AmountIdr, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Event is one outbox row as stored in `events` (README: dedupe).
type Event struct {
	OutboxID    int64
	Topic       string
	AggregateID string
	OccurredAt  time.Time
	Payload     string
}

// KnownEvents returns which of the outbox ids are already in `events`: the publisher's dedupe check (README step 3),
// covering a crash between the insert and marking the rows, and an insert that timed out but landed.
func (c *Client) KnownEvents(ctx context.Context, ids []int64) (map[int64]bool, error) {
	known := map[int64]bool{}
	if len(ids) == 0 {
		return known, nil
	}
	u := make([]uint64, len(ids))
	for i, id := range ids {
		u[i] = uint64(id)
	}
	rows, err := c.conn.Query(ctx, `SELECT outbox_id FROM events WHERE outbox_id IN ?`, u)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		known[int64(id)] = true
	}
	return known, rows.Err()
}

// InsertEvents appends outbox rows to `events` in one block (README step 4).
func (c *Client) InsertEvents(ctx context.Context, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO events (outbox_id, topic, aggregate_id, occurred_at, payload)`)
	if err != nil {
		return err
	}
	for _, e := range evs {
		if err := b.Append(uint64(e.OutboxID), e.Topic, e.AggregateID, e.OccurredAt, e.Payload); err != nil {
			return err
		}
	}
	return b.Send()
}

// MmEfficiencyWeek is one week of value-weighted matched demand and supply utilization (README /mm/analytics).
type MmEfficiencyWeek struct {
	Week                 time.Time // Monday
	Matched, Utilization float64
}

// MmEfficiency returns the last 8 weeks (current included) of the markets' closed rounds, oldest first; weeks without
// closed rounds are absent.
func (c *Client) MmEfficiency(ctx context.Context, marketIDs []string) ([]MmEfficiencyWeek, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT toMonday(at) AS week,
		       ifNull(sum(matched_idr) / nullIf(sum(demand_idr), 0), 0) AS matched,
		       ifNull(sum(matched_idr) / nullIf(sum(supply_idr), 0), 0) AS utilization
		FROM auction_results
		WHERE market_id IN ? AND at >= toMonday(today()) - 49
		GROUP BY week ORDER BY week`, marketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MmEfficiencyWeek
	for rows.Next() {
		var w MmEfficiencyWeek
		if err := rows.Scan(&w.Week, &w.Matched, &w.Utilization); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MmGrowthWeek is one week of active parties, completed trades, distinct buyer-supplier pairs and repeat trades.
type MmGrowthWeek struct {
	Week                                            time.Time // Monday
	Participants, Transactions, Connections, Repeat uint64
}

// MmGrowth returns the last 8 weeks (current included) of the markets, oldest first; empty weeks are absent.
func (c *Client) MmGrowth(ctx context.Context, marketIDs []string) ([]MmGrowthWeek, error) {
	rows, err := c.conn.Query(ctx, `
		WITH toMonday(today()) - 49 AS since
		SELECT week, participants, transactions, connections, repeat
		FROM
		(
			SELECT toMonday(day) AS week, uniqMerge(parties) AS participants
			FROM participants_daily
			WHERE market_id IN ? AND day >= since
			GROUP BY week
		) AS p
		FULL JOIN
		(
			SELECT week, count() AS transactions, uniqExact(buyer_party_id, supplier_party_id) AS connections, countIf(nth > 1) AS repeat
			FROM
			(
				SELECT toMonday(at) AS week, buyer_party_id, supplier_party_id,
				       row_number() OVER (PARTITION BY buyer_party_id, supplier_party_id ORDER BY at) AS nth
				FROM trades
				WHERE market_id IN ? AND status = 'completed'
			)
			WHERE week >= since
			GROUP BY week
		) AS t USING (week)
		ORDER BY week`, marketIDs, marketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MmGrowthWeek
	for rows.Next() {
		var w MmGrowthWeek
		if err := rows.Scan(&w.Week, &w.Participants, &w.Transactions, &w.Connections, &w.Repeat); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
