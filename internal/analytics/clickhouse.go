// Package analytics is the ClickHouse side: append-only events and read-heavy aggregates (explorer stats,
// market price history, activity feed, audit search). PostgreSQL stays the system of record; ClickHouse is fed by
// the outbox publisher and can be rebuilt from it.
package analytics

import (
	"context"
	"fmt"

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
