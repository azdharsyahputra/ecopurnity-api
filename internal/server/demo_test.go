package server

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

func demoTestServer(t *testing.T) *Server {
	t.Helper()
	if testDSN == "" {
		t.Skip("no postgres")
	}
	u, _ := url.Parse(testDSN)
	name := fmt.Sprintf("ecp_demo_%d", time.Now().UnixNano())
	admin := *u
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = conn.Close(context.Background())
	})
	u.Path = "/" + name
	if err := migrate(u.String()); err != nil {
		t.Fatal(err)
	}
	cluster, err := db.Open(ctx, u.String(), "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	keys, _ := secure.Derive([]byte("test-secret-test-secret-test-secret!!"))
	return &Server{DB: cluster, Keys: keys, Log: slog.New(slog.NewTextHandler(testLog{t}, nil))}
}

func demoScalar(t *testing.T, s *Server, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.Primary().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const unbalancedSQL = `SELECT count(*) FROM (SELECT journal_id FROM ledger_entries GROUP BY 1 HAVING sum(amount) <> 0) x`

func TestDemoSeedPulsePurge(t *testing.T) {
	s := demoTestServer(t)
	ctx := context.Background()
	if _, err := s.DB.Primary().Exec(ctx, `
		WITH u AS (INSERT INTO users (name, username, email, password_hash) VALUES ('Real User', 'real.user', 'real@example.id', 'x') RETURNING id),
		     p AS (INSERT INTO parties (kind, user_id, name) SELECT 'user', id, 'Real User' FROM u RETURNING id)
		INSERT INTO listings (code, kind, status, owner_party_id, item, category_id, quantity, unit, location, delivery, price_idr, available_from)
		SELECT 'SUP-REAL1', 'supply', 'available', id, 'Kopi arabika Java Preanger green bean', 'agri', 500, 'kg', 'Garut, Jawa Barat', 'both', 120000, now() FROM p`); err != nil {
		t.Fatal(err)
	}
	nonDemo := func() string {
		var out string
		if err := s.DB.Primary().QueryRow(ctx, `SELECT concat_ws('|', (SELECT count(*) FROM users WHERE email NOT LIKE $1),
			(SELECT string_agg(id::text || status || quantity::text || updated_at::text, ',' ORDER BY id) FROM listings WHERE code = 'SUP-REAL1'),
			(SELECT count(*) FROM parties WHERE user_id IN (SELECT id FROM users WHERE email NOT LIKE $1)))`, demoEmailLike).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := nonDemo()

	counts, err := s.SeedDemo(ctx, DemoOptions{Scale: "small", Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if counts["trades"] < 50 || counts["users"] < 20 || counts["bids"] < 100 {
		t.Fatalf("too little seeded: %v", counts)
	}
	if n := demoScalar(t, s, unbalancedSQL); n != 0 {
		t.Fatalf("%d unbalanced journals after seed", n)
	}
	if n := demoScalar(t, s, `SELECT count(*) FROM auctions WHERE status = 'live'`); n == 0 {
		t.Fatal("no live auction")
	}
	if n := demoScalar(t, s, `SELECT count(*) FROM outbox WHERE topic = 'activity' AND created_at < now() - interval '20 days'`); n == 0 {
		t.Fatal("no activity history")
	}
	if _, err := s.SeedDemo(ctx, DemoOptions{Scale: "small", Days: 30}); err == nil {
		t.Fatal("second seed should refuse")
	}

	bids := demoScalar(t, s, `SELECT count(*) FROM bids`)
	events := demoScalar(t, s, `SELECT count(*) FROM trade_events`)
	activity := demoScalar(t, s, `SELECT count(*) FROM outbox WHERE topic = 'activity'`)
	now := time.Now()
	p := &demoPulse{s: s, ctx: ctx, r: rand.New(rand.NewPCG(1, 2)), now: now, g: &demoGen{r: rand.New(rand.NewPCG(3, 4)), now: now}}
	for range 30 {
		for _, fn := range []func() error{p.maintainRounds, p.bid, p.advanceTrade, p.listing, p.dutchAccept, p.directOrder, p.rfq} {
			if err := fn(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if demoScalar(t, s, `SELECT count(*) FROM bids`) == bids {
		t.Fatal("pulse placed no bid")
	}
	if demoScalar(t, s, `SELECT count(*) FROM trade_events`) == events {
		t.Fatal("pulse advanced no trade")
	}
	if demoScalar(t, s, `SELECT count(*) FROM outbox WHERE topic = 'activity'`) == activity {
		t.Fatal("pulse produced no activity")
	}
	if n := demoScalar(t, s, unbalancedSQL); n != 0 {
		t.Fatalf("%d unbalanced journals after pulse", n)
	}
	if got := nonDemo(); got != before {
		t.Fatalf("pulse touched non-demo rows: %s -> %s", before, got)
	}

	if _, _, err := s.PurgeDemo(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"trades", "bids", "auctions", "markets", "orgs", "ledger_entries", "rfqs", "conversations", "opportunities"} {
		if n := demoScalar(t, s, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%d %s left after purge", n, table)
		}
	}
	if n := demoScalar(t, s, `SELECT count(*) FROM users WHERE email LIKE $1`, demoEmailLike); n != 0 {
		t.Fatalf("%d demo users left", n)
	}
	if got := nonDemo(); got != before {
		t.Fatalf("purge touched non-demo rows: %s -> %s", before, got)
	}
}

func TestDemoPulseOff(t *testing.T) {
	s := &Server{}
	done := make(chan struct{})
	go func() { s.RunDemoPulse(context.Background(), time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunDemoPulse ran with DemoPulse off")
	}
}
