package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
	"github.com/azdharsyahputra/ecopurnity-api/internal/config"
	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
	"github.com/azdharsyahputra/ecopurnity-api/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed-demo:", err)
		os.Exit(1)
	}
}

func run() error {
	purge := flag.Bool("purge", false, "remove all demo data (and only demo data)")
	scale := flag.String("scale", "normal", "small | normal")
	days := flag.Int("days", 90, "history window in days")
	yes := flag.Bool("yes", false, "required when the database host is not localhost")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: seed-demo [--scale small|normal] [--days 90] [--yes]\n       seed-demo --purge [--yes]")
		flag.PrintDefaults()
	}
	flag.Parse()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	u, err := url.Parse(cfg.PostgresPrimaryURL)
	if err != nil {
		return fmt.Errorf("POSTGRES_PRIMARY_URL: %w", err)
	}
	host, dbName := u.Hostname(), strings.TrimPrefix(u.Path, "/")
	action := fmt.Sprintf("seed (scale %s, %d days)", *scale, *days)
	if *purge {
		action = "purge"
	}
	fmt.Printf("target: postgres %s/%s, clickhouse %s/%s, app %s: %s\n", host, dbName, cfg.ClickHouseAddr, cfg.ClickHouseDatabase, cfg.AppURL, action)
	if host != "localhost" && host != "127.0.0.1" && host != "::1" && !*yes {
		return fmt.Errorf("%s is not localhost: pass --yes to continue", host)
	}

	ctx := context.Background()
	pg, err := db.Open(ctx, cfg.PostgresPrimaryURL, "", time.Second)
	if err != nil {
		return err
	}
	defer pg.Close()
	keys, err := secure.Derive(cfg.Secret)
	if err != nil {
		return err
	}
	s := &server.Server{DB: pg, Keys: keys}
	start := time.Now()

	if !*purge {
		counts, err := s.SeedDemo(ctx, server.DemoOptions{Scale: *scale, Days: *days, Log: os.Stdout})
		if err != nil {
			return err
		}
		printCounts("written", counts)
		fmt.Printf("done in %.1fs; the API's outbox publisher moves the facts to ClickHouse\n", time.Since(start).Seconds())
		return nil
	}

	ids, counts, err := s.PurgeDemo(ctx)
	if err != nil {
		return err
	}
	printCounts("deleted", counts)
	ch, err := analytics.Open(cfg.ClickHouseAddr, cfg.ClickHouseDatabase, cfg.ClickHouseUser, cfg.ClickHousePassword)
	if err == nil {
		defer ch.Close()
		err = ch.PurgeDemo(ctx, ids)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: clickhouse purge failed (run --purge again when it is reachable; tagged demo facts are still found):", err)
	} else {
		fmt.Println("clickhouse: demo facts removed, aggregates rebuilt")
	}
	fmt.Printf("done in %.1fs\n", time.Since(start).Seconds())
	return nil
}

func printCounts(verb string, m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-28s %7d %s\n", k, m[k], verb)
	}
}
