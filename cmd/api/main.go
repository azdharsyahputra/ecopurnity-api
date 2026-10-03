// Command api is the Ecopurnity HTTP API: every operation in api/openapi.yaml is routed and validated; operations that
// are not implemented yet answer 501 not_implemented.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
	"github.com/azdharsyahputra/ecopurnity-api/internal/config"
	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
	"github.com/azdharsyahputra/ecopurnity-api/internal/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := db.Open(ctx, cfg.PostgresPrimaryURL, cfg.PostgresReplicaURL, cfg.ReplicaMaxLag)
	if err != nil {
		return err
	}
	defer pg.Close()
	go pg.Watch(ctx, 5*time.Second)

	ch, err := analytics.Open(cfg.ClickHouseAddr, cfg.ClickHouseDatabase, cfg.ClickHouseUser, cfg.ClickHousePassword)
	if err != nil {
		return err
	}
	defer ch.Close()

	h, err := (&server.Server{DB: pg, Analytics: ch, Log: log}).Handler()
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
	return nil
}
