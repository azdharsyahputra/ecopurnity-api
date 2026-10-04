package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DemoOptions struct {
	Scale string
	Days  int
	Log   io.Writer
}

type demoScale struct {
	People, Orgs, MMs, Markets, Listings, RFQs, Contracts, BuyerAuctions, OrgAuctions, DirectTrades int
}

var demoScales = map[string]demoScale{
	"normal": {People: 60, Orgs: 15, MMs: 4, Markets: 10, Listings: 250, RFQs: 20, Contracts: 8, BuyerAuctions: 12, OrgAuctions: 12, DirectTrades: 70},
	"small":  {People: 20, Orgs: 6, MMs: 2, Markets: 4, Listings: 80, RFQs: 6, Contracts: 3, BuyerAuctions: 4, OrgAuctions: 4, DirectTrades: 20},
}

const demoEmailLike = "%@" + DemoEmailDomain

func (s *Server) DemoCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM users WHERE email LIKE $1`, demoEmailLike).Scan(&n)
	return n, err
}

func (s *Server) SeedDemo(ctx context.Context, o DemoOptions) (map[string]int, error) {
	sc, ok := demoScales[o.Scale]
	if !ok {
		return nil, fmt.Errorf("scale must be small or normal")
	}
	if o.Days < 14 || o.Days > 365 {
		return nil, fmt.Errorf("days must be 14..365")
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if n, err := s.DemoCount(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, fmt.Errorf("demo data already present (%d demo accounts); run with --purge first to reseed", n)
	}
	now := time.Now().UTC().Truncate(time.Second)
	g := &demoGen{ctx: ctx, s: s, r: rand.New(rand.NewPCG(20261004, uint64(o.Days))), now: now, from: now.AddDate(0, 0, -o.Days),
		days: o.Days, sc: sc, log: o.Log, b: &pgx.Batch{}, codes: map[string][]string{}, acct: map[string]string{},
		avail: map[string][]demoMove{}, n: map[string]int{}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		g.tx = tx

		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, engineLockKey); err != nil {
			return err
		}
		return g.run()
	})
	if err != nil {
		return nil, err
	}
	return g.n, nil
}

type demoGen struct {
	ctx       context.Context
	s         *Server
	tx        pgx.Tx
	r         *rand.Rand
	now, from time.Time
	days      int
	sc        demoScale
	log       io.Writer

	b      *pgx.Batch
	err    error
	codes  map[string][]string
	acct   map[string]string
	avail  map[string][]demoMove
	n      map[string]int
	hashes []string

	people   []*demoPerson
	orgs     []*demoOrg
	mmOrgs   []*demoOrg
	parties  []*demoParty
	markets  []*demoMarket
	listings []*demoListing
	trades   []*demoTrade
	staff    *demoPerson

	usedNames, takenUsers, takenEmails map[string]bool
}

type demoMove struct {
	at     time.Time
	amount int64
}

func (g *demoGen) logf(format string, args ...any) { fmt.Fprintf(g.log, format+"\n", args...) }

func (g *demoGen) q(sql string, args ...any) {
	g.b.Queue(sql, args...)
	if g.b.Len() >= 2000 {
		g.flush()
	}
}

func (g *demoGen) flush() {
	if g.err != nil || g.b.Len() == 0 {
		return
	}
	g.err = g.tx.SendBatch(g.ctx, g.b).Close()
	g.b = &pgx.Batch{}
}

func (g *demoGen) ins(table, cols string, args ...any) {
	ph := make([]string, len(args))
	for i := range args {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	g.q("INSERT INTO "+table+" ("+cols+") VALUES ("+strings.Join(ph, ", ")+")", args...)
	g.n[table]++
}

func newID() string { return uuid.NewString() }

func (g *demoGen) code(prefix string) string {
	if len(g.codes[prefix]) == 0 && g.err == nil {
		rows, err := g.tx.Query(g.ctx, `SELECT next_code($1) FROM generate_series(1, 256)`, prefix)
		if err == nil {
			g.codes[prefix], err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			g.err = err
		}
	}
	if len(g.codes[prefix]) == 0 {
		return prefix + "-0000"
	}
	c := g.codes[prefix][0]
	g.codes[prefix] = g.codes[prefix][1:]
	return c
}

func (g *demoGen) fact(topic, aggregate string, at time.Time, payload map[string]any) {
	payload["demo"] = true
	b, _ := json.Marshal(payload)
	g.ins("outbox", "created_at, topic, aggregate_id, payload", at, topic, aggregate, b)
}

func (g *demoGen) activity(typ, title string, amount *int64, market *string, at time.Time) {
	p := map[string]any{"type": typ, "title": title}
	if amount != nil {
		p["amountIdr"] = *amount
	}
	if market != nil {
		p["marketId"] = *market
	}
	g.fact("activity", "act-"+strings.ToLower(cryptorand.Text()), at, p)
}

func (g *demoGen) f(a, b float64) float64  { return a + g.r.Float64()*(b-a) }
func (g *demoGen) i(a, b int) int          { return a + g.r.IntN(b-a+1) }
func (g *demoGen) chance(p float64) bool   { return g.r.Float64() < p }
func demoPick[T any](g *demoGen, xs []T) T { return xs[g.r.IntN(len(xs))] }

func (g *demoGen) ago(d float64) time.Time {
	return g.now.Add(-time.Duration(d * float64(24*time.Hour))).Truncate(time.Second)
}

func hrs(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }

func niceQty(v float64) float64 {
	switch {
	case v >= 10_000:
		return math.Round(v/1000) * 1000
	case v >= 1_000:
		return math.Round(v/100) * 100
	case v >= 100:
		return math.Round(v/10) * 10
	case v < 1:
		return 1
	}
	return math.Round(v)
}

func nicePrice(v float64) int64 {
	switch {
	case v >= 1_000_000:
		return int64(math.Round(v/10_000) * 10_000)
	case v >= 100_000:
		return int64(math.Round(v/1_000) * 1_000)
	case v >= 10_000:
		return int64(math.Round(v/500) * 500)
	case v >= 1_000:
		return int64(math.Round(v/50) * 50)
	}
	return int64(math.Max(1, math.Round(v)))
}

func idrPtr(v int64) *int64 { return &v }

func (g *demoGen) workHours(t time.Time) time.Time {
	l := t.In(wib)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, wib)
	open := func(d time.Time) time.Time {
		return d.Add(time.Duration(8*60+g.i(0, 150)) * time.Minute).Add(time.Duration(g.i(0, 59)) * time.Second)
	}
	switch {
	case l.Hour() < 7:
		l = open(day)
	case l.Hour() >= 20:
		l = open(day.AddDate(0, 0, 1))
	}
	switch l.Weekday() {
	case time.Sunday:
		if g.chance(0.85) {
			l = open(time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, wib))
		}
	case time.Saturday:
		if g.chance(0.55) {
			l = open(time.Date(l.Year(), l.Month(), l.Day()+2, 0, 0, 0, 0, wib))
		}
	}
	return l.UTC().Truncate(time.Second)
}

func (g *demoGen) when() time.Time {
	u := g.r.Float64()
	if g.chance(0.6) {
		u = math.Sqrt(u)
	}
	t := g.ago(float64(g.days) * (1 - u))
	if w := g.workHours(t); w.Before(g.now) {
		return w
	}
	return t
}
