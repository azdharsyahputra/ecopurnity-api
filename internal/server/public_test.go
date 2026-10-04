package server

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

func chSchema(t *testing.T) (*analytics.Client, driver.Conn) {
	addr := os.Getenv("TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		addr = "localhost:9000"
	}
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, DialTimeout: 2 * time.Second, Auth: clickhouse.Auth{Database: "default"}})
	if err != nil || conn.Ping(t0()) != nil {
		return nil, nil
	}
	name := fmt.Sprintf("ecp_test_%d", time.Now().UnixNano())
	raw, err := os.ReadFile("../../migrations/clickhouse/00001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	schema := strings.ReplaceAll(strings.ReplaceAll(sql.String(), "ecopurnity.", name+"."), "DATABASE IF NOT EXISTS ecopurnity", "DATABASE "+name)
	t.Cleanup(func() { _ = conn.Exec(t0(), "DROP DATABASE IF EXISTS "+name); _ = conn.Close() })
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if err := conn.Exec(t0(), stmt); err != nil {
			t.Fatalf("schema: %v\n%s", err, stmt)
		}
	}
	c, err := analytics.Open(addr, name, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	scratch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: name}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scratch.Close() })
	return c, scratch
}

func TestPublicAnalyticsReads(t *testing.T) {
	e := newEnv(t)
	ch, raw := chSchema(t)
	if ch == nil {
		t.Skip("clickhouse not running")
	}
	e.server.Analytics = ch
	now := time.Now().UTC()
	events := []struct {
		topic, agg, payload string
		at                  time.Time
	}{
		{"trade.status", "t1", `{"status":"agreement","via":"direct","item":"Kopi","categoryId":"agri","region":"Jawa Barat","marketId":"m1",
			"buyerPartyId":"b1","supplierPartyId":"s1","quantity":10,"unit":"kg","unitPriceIdr":1000,"valueIdr":10000}`, now.Add(-48 * time.Hour)},
		{"trade.status", "t1", `{"status":"completed","via":"direct","item":"Kopi","categoryId":"agri","region":"Jawa Barat","marketId":"m1",
			"buyerPartyId":"b1","supplierPartyId":"s1","quantity":10,"unit":"kg","unitPriceIdr":1000,"valueIdr":10000}`, now.Add(-time.Hour)},
		{"listing.created", "l1", `{"kind":"demand","categoryId":"agri","region":"Bandung","item":"Kopi","quantity":100,"unit":"kg","valueIdr":500000,"partyId":"b1"}`, now.Add(-time.Hour)},
		{"listing.created", "l2", `{"kind":"supply","categoryId":"agri","region":"Garut","item":"Kopi","quantity":40,"unit":"kg","valueIdr":200000,"partyId":"s2"}`, now.Add(-time.Hour)},
		{"opportunity.detected", "o1", `{"categoryId":"agri","region":"Jawa Barat"}`, now.Add(-time.Hour)},
		{"activity", "act-1", `{"type":"opportunity_detected","title":"Opportunity baru: kopi","amountIdr":123}`, now.Add(-time.Minute)},
	}
	for i, ev := range events {
		if err := raw.Exec(t0(), `INSERT INTO events (outbox_id, topic, aggregate_id, occurred_at, payload) VALUES (?, ?, ?, ?, ?)`,
			uint64(i+1), ev.topic, ev.agg, ev.at, ev.payload); err != nil {
			t.Fatal(err)
		}
	}

	r := e.call(e.client(), "GET", "/public/stats", nil)
	if r.Status != 200 || r.Body["opportunitiesDetected"] != 1.0 || r.Body["transactionVolumeIdr"] != 10000.0 ||
		r.Body["activeMarkets"] != 1.0 || r.Body["activeParticipants"] != 3.0 {
		t.Fatalf("stats: %d %v", r.Status, r.Body)
	}
	acts := e.callList(e.client(), "/public/activity")
	if len(acts) != 1 || acts[0]["id"] != "act-1" || acts[0]["amountIdr"] != 123.0 {
		t.Fatalf("activity: %v", acts)
	}
	r = e.call(e.client(), "GET", "/explorer/overview?range=7d&category=agri", nil)
	vol := r.Body["volume"].([]any)
	ds := r.Body["demandSupply"].([]any)
	idx := r.Body["priceIndex"].([]any)
	if r.Status != 200 || len(vol) != 7 || vol[6].(map[string]any)["volumeIdr"] != 10000.0 || len(ds) != 1 ||
		ds[0].(map[string]any)["demandIdr"] != 500000.0 || idx[6].(map[string]any)["agri"] != 100.0 {
		t.Fatalf("overview: %d %v", r.Status, r.Body)
	}
	agg := e.callList(e.client(), "/explorer/supply")
	if len(agg) != 1 || agg[0]["region"] != "Garut" || agg[0]["listings"] != 1.0 || agg[0]["quantity"].(map[string]any)["value"] != 40.0 {
		t.Fatalf("aggregates: %v", agg)
	}

	down, err := analytics.Open("127.0.0.1:1", "x", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	e.server.Analytics = down
	if r := e.call(e.client(), "GET", "/public/stats", nil); r.Status != 200 || r.Body["transactionVolumeIdr"] != 0.0 {
		t.Fatalf("down stats: %d %v", r.Status, r.Body)
	}
	if r := e.call(e.client(), "GET", "/explorer/overview", nil); r.Status != 200 || len(r.Body["volume"].([]any)) != 30 {
		t.Fatalf("down overview: %d %v", r.Status, r.Body)
	}

	_, buyerID := e.bidder("Pembeli Fallback")
	_, supplierID := e.bidder("Supplier Fallback")
	title := fmt.Sprintf("Gula semut aren %d kg", time.Now().UnixNano()%1000)
	e.completedTrade(buyerID, supplierID, title)
	found := false
	for _, a := range e.callList(e.client(), "/public/activity?limit=100") {
		if a["title"] == "Transaksi selesai: "+title && a["type"] == "transaction_completed" && a["amountIdr"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("with ClickHouse down, the activity feed falls back to Postgres")
	}
}
