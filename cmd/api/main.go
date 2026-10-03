// Command api is the Ecopurnity HTTP API: every operation in api/openapi.yaml is routed and validated; operations that
// are not implemented yet answer 501 not_implemented.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
	"github.com/azdharsyahputra/ecopurnity-api/internal/config"
	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
	"github.com/azdharsyahputra/ecopurnity-api/internal/server"
	"github.com/azdharsyahputra/ecopurnity-api/internal/sms"
	"github.com/azdharsyahputra/ecopurnity-api/internal/storage"
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

	var mailer mail.Mailer = mail.Log{Logger: log}
	if cfg.SMTPHost != "" {
		mailer = mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom, TLS: cfg.SMTPTLS}
		log.Info("smtp", "host", cfg.SMTPHost, "port", cfg.SMTPPort, "tls", cfg.SMTPTLS)
	}
	keys, err := secure.Derive(cfg.Secret)
	if err != nil {
		return err
	}
	var store *storage.Store
	if cfg.S3Bucket != "" {
		store = storage.New(storage.Config{Endpoint: cfg.S3Endpoint, PublicEndpoint: cfg.S3PublicEndpoint, Region: cfg.S3Region,
			Bucket: cfg.S3Bucket, AccessKeyID: cfg.S3AccessKeyID, SecretAccessKey: cfg.S3SecretAccessKey, PathStyle: cfg.S3PathStyle})
		if cfg.S3CreateBucket {
			if err := store.EnsureBucket(ctx); err != nil {
				return fmt.Errorf("create bucket: %w", err)
			}
		}
		if err := store.Ping(ctx); err != nil {
			log.Warn("object storage unreachable; uploads will fail", "err", err)
		}
		if len(cfg.S3CORSOrigins) > 0 {
			if err := store.SetCORS(ctx, cfg.S3CORSOrigins); err != nil {
				log.Warn("could not set bucket CORS; set it at the provider (see README)", "err", err)
			}
		}
	}
	api := &server.Server{
		DB: pg, Analytics: ch, Log: log, Mail: mailer, Keys: keys, Storage: store, SMS: sms.Log{Logger: log},
		AppURL: cfg.AppURL, CookieSecure: cfg.CookieSecure, SessionTTL: cfg.SessionTTL, GoogleDevLogin: cfg.GoogleDevLogin,
		SimulateCounterparties: cfg.SimulateCounterparties,
	}
	defer api.WaitMail() // let queued emails go out on shutdown
	go api.RunAuctionClock(ctx, time.Second)
	go api.RunTradeClock(ctx, 2*time.Second)
	if cfg.SimulateCounterparties {
		log.Warn("SIMULATE_COUNTERPARTIES is on: a bot plays external trade counterparties (demo only)")
	}
	h, err := api.Handler()
	if err != nil {
		return err
	}

	// Realtime: every instance listens for frames (LISTEN ecp_rt); one of them (advisory lock) publishes the outbox.
	// Both stop with rtCtx; Listen closes this instance's sockets with 1001 on the way out.
	rtCtx, stopRT := context.WithCancel(context.Background())
	var rt sync.WaitGroup
	rt.Add(2)
	go func() { defer rt.Done(); api.Listen(rtCtx) }()
	go func() { defer rt.Done(); api.Publish(rtCtx) }()
	defer func() { stopRT(); rt.Wait() }()

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
		// Shutdown does not wait for hijacked (WebSocket) connections: close them first so clients reconnect elsewhere.
		stopRT()
		return srv.Shutdown(shutdown)
	}
	return nil
}
