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

// Public stats, activity and explorer reads (queries from migrations/clickhouse/README.md). `days` is a range the
// caller validated; `category` "" means every category.

// Stats is PublicStats: parties and markets active in the last 30 days, opportunities and volume since launch.
type Stats struct {
	ActiveParticipants, ActiveMarkets, OpportunitiesDetected, TransactionVolumeIdr uint64
}

func (c *Client) PublicStats(ctx context.Context) (Stats, error) {
	var s Stats
	err := c.conn.QueryRow(ctx, `
		SELECT
		    (SELECT uniqMerge(parties) FROM participants_daily WHERE day > today() - 30),
		    (SELECT uniqExact(market_id) FROM participants_daily WHERE day > today() - 30 AND market_id != ''),
		    (SELECT count() FROM events WHERE topic = 'opportunity.detected'),
		    (SELECT sum(volume_idr) FROM trade_daily)`).
		Scan(&s.ActiveParticipants, &s.ActiveMarkets, &s.OpportunitiesDetected, &s.TransactionVolumeIdr)
	return s, err
}

// PublicActivity returns the newest `limit` activity events, newest first.
func (c *Client) PublicActivity(ctx context.Context, limit int) ([]Activity, error) {
	rows, err := c.conn.Query(ctx, `SELECT id, type, title, amount_idr, at FROM activity ORDER BY at DESC LIMIT ?`, limit)
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

// Deltas are this period vs the previous one of the same length, as fractions; nil = nothing in the previous period.
type Deltas struct{ Participants, Markets, Opportunities, Volume *float64 }

func (c *Client) ExplorerDeltas(ctx context.Context, days int, category string) (Deltas, error) {
	var d Deltas
	err := c.conn.QueryRow(ctx, fmt.Sprintf(`
		WITH %[1]d AS n
		SELECT
		    (SELECT uniqMergeIf(parties, day > today() - n) / nullIf(uniqMergeIf(parties, day <= today() - n), 0) - 1
		       FROM participants_daily WHERE day > today() - 2 * n AND (? = '' OR category_id = ?)),
		    (SELECT uniqExactIf(market_id, day > today() - n) / nullIf(uniqExactIf(market_id, day <= today() - n), 0) - 1
		       FROM participants_daily WHERE day > today() - 2 * n AND market_id != '' AND (? = '' OR category_id = ?)),
		    (SELECT countIf(occurred_at > now() - toIntervalDay(n)) / nullIf(countIf(occurred_at <= now() - toIntervalDay(n)), 0) - 1
		       FROM events WHERE topic = 'opportunity.detected' AND occurred_at > now() - toIntervalDay(2 * n)
		        AND (? = '' OR JSONExtractString(payload, 'categoryId') = ?)),
		    (SELECT sumIf(volume_idr, day > today() - n) / nullIf(sumIf(volume_idr, day <= today() - n), 0) - 1
		       FROM trade_daily WHERE day > today() - 2 * n AND (? = '' OR category_id = ?))`, days),
		category, category, category, category, category, category, category, category).
		Scan(&d.Participants, &d.Markets, &d.Opportunities, &d.Volume)
	return d, err
}

// DayVolume is completed-trade volume on one day.
type DayVolume struct {
	Day    time.Time
	Volume uint64
}

// VolumeSeries returns one row per day of the range, oldest first, gaps filled with 0.
func (c *Client) VolumeSeries(ctx context.Context, days int, category string) ([]DayVolume, error) {
	rows, err := c.conn.Query(ctx, fmt.Sprintf(`
		SELECT day, sum(volume_idr) FROM trade_daily
		WHERE day > today() - %[1]d AND (? = '' OR category_id = ?)
		GROUP BY day ORDER BY day WITH FILL FROM today() - %[1]d + 1 TO today() + 1`, days), category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayVolume
	for rows.Next() {
		var v DayVolume
		if err := rows.Scan(&v.Day, &v.Volume); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// IndexPoint is one category's price index on one day (each market's median relative to its first day in range,
// averaged per category; start = 100).
type IndexPoint struct {
	Day      time.Time
	Category string
	Index    float64
}

func (c *Client) PriceIndex(ctx context.Context, days int, category string) ([]IndexPoint, error) {
	rows, err := c.conn.Query(ctx, fmt.Sprintf(`
		SELECT day, category_id, round(100 * avg(median / base), 1)
		FROM
		(
		    SELECT category_id, market_id, day, median, first_value(median) OVER (PARTITION BY market_id ORDER BY day) AS base
		    FROM
		    (
		        SELECT category_id, market_id, day, quantileMerge(0.5)(median_state) AS median
		        FROM market_price_daily
		        WHERE day > today() - %d AND (? = '' OR category_id = ?)
		        GROUP BY category_id, market_id, day
		    )
		)
		GROUP BY day, category_id
		ORDER BY day, category_id`, days), category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexPoint
	for rows.Next() {
		var p IndexPoint
		if err := rows.Scan(&p.Day, &p.Category, &p.Index); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CategoryValue is the listed demand and supply value of one category.
type CategoryValue struct {
	Category       string
	Demand, Supply uint64
}

func (c *Client) DemandSupply(ctx context.Context, days int, category string) ([]CategoryValue, error) {
	rows, err := c.conn.Query(ctx, fmt.Sprintf(`
		SELECT category_id, sumIf(value_idr, kind = 'demand') AS demand, sumIf(value_idr, kind = 'supply')
		FROM listings
		WHERE at > now() - toIntervalDay(%d) AND (? = '' OR category_id = ?)
		GROUP BY category_id ORDER BY demand DESC, category_id`, days), category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryValue
	for rows.Next() {
		var v CategoryValue
		if err := rows.Scan(&v.Category, &v.Demand, &v.Supply); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Aggregate is one explorer row: listed quantity of an item in a region over the last 60 days, listings in the last
// 30, and the trend (last 30 days vs the 30 before; nil = nothing before).
type Aggregate struct {
	Category, Item, Region, Unit string
	Quantity                     float64
	Listings                     uint64
	Trend                        *float64
}

// ponytail: top 200 rows; page when the explorer table pages.
func (c *Client) Aggregates(ctx context.Context, side, category string) ([]Aggregate, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT category_id, item, region, sum(quantity), unit,
		       countIf(at > now() - INTERVAL 30 DAY) AS recent,
		       round(recent / nullIf(countIf(at <= now() - INTERVAL 30 DAY), 0) - 1, 2)
		FROM listings
		WHERE kind = ? AND at > now() - INTERVAL 60 DAY AND (? = '' OR category_id = ?)
		GROUP BY category_id, item, region, unit
		ORDER BY recent DESC, category_id, item, region
		LIMIT 200`, side, category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Aggregate
	for rows.Next() {
		var a Aggregate
		if err := rows.Scan(&a.Category, &a.Item, &a.Region, &a.Quantity, &a.Unit, &a.Listings, &a.Trend); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
