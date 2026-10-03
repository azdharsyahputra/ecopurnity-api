// Package config reads the service configuration from the environment (12-factor).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr string

	// PostgresPrimaryURL takes every write and any read that must see its own write.
	PostgresPrimaryURL string
	// PostgresReplicaURL serves read-only queries (lists, dashboards, public pages).
	// Empty means "no replica": reads fall back to the primary.
	PostgresReplicaURL string

	// ClickHouseAddr is host:port of the native protocol; ClickHouse holds analytics, activity feeds and audit search,
	// never the system of record.
	ClickHouseAddr     string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string

	// ReplicaMaxLag is how far behind the replica may be before readiness fails and reads fall back to the primary.
	ReplicaMaxLag time.Duration

	// AppURL is the frontend origin used in email links (verification, password reset).
	AppURL string
	// CookieSecure marks the session cookie Secure; true everywhere except plain-http local development.
	CookieSecure bool
	// SessionTTL is the sliding lifetime of a session.
	SessionTTL time.Duration
	// GoogleDevLogin enables the mock-compatible POST /auth/google (fixed test account). Never in production;
	// the real OAuth flow replaces it.
	GoogleDevLogin bool
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:           env("HTTP_ADDR", ":8080"),
		PostgresPrimaryURL: os.Getenv("POSTGRES_PRIMARY_URL"),
		PostgresReplicaURL: os.Getenv("POSTGRES_REPLICA_URL"),
		ClickHouseAddr:     env("CLICKHOUSE_ADDR", "localhost:9000"),
		ClickHouseDatabase: env("CLICKHOUSE_DATABASE", "ecopurnity"),
		ClickHouseUser:     env("CLICKHOUSE_USER", "default"),
		ClickHousePassword: os.Getenv("CLICKHOUSE_PASSWORD"),
	}
	c.AppURL = env("APP_URL", "http://localhost:5173")
	c.CookieSecure = env("COOKIE_SECURE", "true") == "true"
	c.GoogleDevLogin = env("GOOGLE_DEV_LOGIN", "false") == "true"
	days, err := strconv.Atoi(env("SESSION_TTL_DAYS", "30"))
	if err != nil || days < 1 {
		return c, fmt.Errorf("SESSION_TTL_DAYS must be a positive integer")
	}
	c.SessionTTL = time.Duration(days) * 24 * time.Hour
	lag, err := strconv.Atoi(env("REPLICA_MAX_LAG_SECONDS", "5"))
	if err != nil || lag < 0 {
		return c, fmt.Errorf("REPLICA_MAX_LAG_SECONDS must be a non-negative integer")
	}
	c.ReplicaMaxLag = time.Duration(lag) * time.Second
	if c.PostgresPrimaryURL == "" {
		return c, fmt.Errorf("POSTGRES_PRIMARY_URL is required")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
