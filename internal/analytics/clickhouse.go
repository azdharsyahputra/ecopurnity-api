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
func (c *Client) Close() error                  { return c.conn.Close() }
