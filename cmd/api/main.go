// Command api is the Ecopurnity HTTP API. For now it serves only health endpoints; routes are generated from
// api/openapi.yaml as the backend is built out.
package main

import (
	"context"
	"encoding/json"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		st, err := pg.Status(r.Context())
		chErr := ch.Ping(r.Context())
		body := map[string]any{"postgres": st, "clickhouse": "ok"}
		code := http.StatusOK
		if err != nil {
			code = http.StatusServiceUnavailable
		}
		if chErr != nil {
			// Analytics being down degrades dashboards but must not take the API out of rotation.
			body["clickhouse"] = chErr.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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
