// Package db owns the PostgreSQL primary/replica pair.
//
// Rules of use:
//   - Writes and read-your-writes go through Primary().
//   - Everything else (lists, dashboards, public pages) goes through Reader(), which prefers the replica and
//     falls back to the primary when the replica is missing, down, or lagging past the configured limit.
//   - A request that wrote and then reads (e.g. POST then return the resource) must read from Primary(): the
//     replica applies WAL asynchronously.
package db

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Cluster struct {
	primary *pgxpool.Pool
	replica *pgxpool.Pool // nil when no replica is configured

	maxLag time.Duration
	// replicaOK is refreshed by Watch; Reader() only trusts the replica while it is true.
	replicaOK atomic.Bool
}

func Open(ctx context.Context, primaryURL, replicaURL string, maxLag time.Duration) (*Cluster, error) {
	p, err := pgxpool.New(ctx, primaryURL)
	if err != nil {
		return nil, fmt.Errorf("primary: %w", err)
	}
	c := &Cluster{primary: p, maxLag: maxLag}
	if replicaURL != "" {
		r, err := pgxpool.New(ctx, replicaURL)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("replica: %w", err)
		}
		c.replica = r
	}
	return c, nil
}

func (c *Cluster) Primary() *pgxpool.Pool { return c.primary }

// Reader returns the pool for read-only queries.
func (c *Cluster) Reader() *pgxpool.Pool {
	if c.replica != nil && c.replicaOK.Load() {
		return c.replica
	}
	return c.primary
}

// Watch re-checks the replica's health and lag every interval until ctx ends. Run it in its own goroutine.
func (c *Cluster) Watch(ctx context.Context, every time.Duration) {
	if c.replica == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.replicaOK.Store(c.replicaHealthy(ctx))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Cluster) replicaHealthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var lag float64
	// Seconds since the last replayed transaction; 0 when the replica is fully caught up and idle.
	err := c.replica.QueryRow(ctx, `
		SELECT CASE WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
		       ELSE COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()), 0) END`).Scan(&lag)
	return err == nil && time.Duration(lag*float64(time.Second)) <= c.maxLag
}

// Status is what /readyz reports.
type Status struct {
	Primary    string `json:"primary"`
	Replica    string `json:"replica"`
	ReplicaOK  bool   `json:"replicaServing"`
	ReplicaLag string `json:"replicaLag,omitempty"`
}

func (c *Cluster) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s := Status{Primary: "ok", Replica: "not configured", ReplicaOK: false}
	if err := c.primary.Ping(ctx); err != nil {
		s.Primary = err.Error()
		return s, err
	}
	if c.replica != nil {
		s.Replica = "ok"
		if err := c.replica.Ping(ctx); err != nil {
			s.Replica = err.Error()
		}
		s.ReplicaOK = c.replicaOK.Load()
	}
	return s, nil
}

func (c *Cluster) Close() {
	c.primary.Close()
	if c.replica != nil {
		c.replica.Close()
	}
}
