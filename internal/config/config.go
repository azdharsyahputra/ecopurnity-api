// Package config reads the service configuration from the environment (12-factor).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// Secret keys HMACs of one-time codes (and later app-level encryption). At least 32 bytes; never commit it.
	Secret []byte

	// SMTP for transactional email. Empty SMTPHost logs messages instead of sending them.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPTLS      string // tls | starttls | none (default from the port: 465 tls, 25/1025 none, else starttls)

	// Object storage (S3-compatible; Cloudflare R2 in production). Empty S3Bucket disables uploads (503).
	S3Endpoint        string
	S3PublicEndpoint  string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3PathStyle       bool
	S3CreateBucket    bool     // create the bucket at startup if missing (local only)
	S3CORSOrigins     []string // when set, apply a CORS rule for these browser origins at startup

	// GoogleDevLogin enables the mock-compatible POST /auth/google (fixed test account). Never in production;
	// the real OAuth flow replaces it.
	GoogleDevLogin bool

	// SimulateCounterparties lets a bot play external (off-platform) counterparties of trades and contracts so demo
	// flows can be completed. Dev/demo only; never in production.
	SimulateCounterparties bool
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
	c.Secret = []byte(os.Getenv("APP_SECRET"))
	if len(c.Secret) < 32 {
		return c, fmt.Errorf("APP_SECRET must be at least 32 characters")
	}
	c.SMTPHost = os.Getenv("SMTP_HOST")
	c.SMTPUsername = os.Getenv("SMTP_USERNAME")
	c.SMTPPassword = os.Getenv("SMTP_PASSWORD")
	c.SMTPFrom = env("SMTP_FROM", "Ecopurnity <no-reply@ecopurnity.local>")
	if c.SMTPHost != "" {
		port, err := strconv.Atoi(env("SMTP_PORT", "587"))
		if err != nil {
			return c, fmt.Errorf("SMTP_PORT must be a number")
		}
		c.SMTPPort = port
		def := "starttls"
		switch port {
		case 465:
			def = "tls"
		case 25, 1025:
			def = "none"
		}
		c.SMTPTLS = env("SMTP_TLS", def)
		if c.SMTPTLS != "tls" && c.SMTPTLS != "starttls" && c.SMTPTLS != "none" {
			return c, fmt.Errorf("SMTP_TLS must be tls, starttls or none")
		}
	}
	c.S3Endpoint = os.Getenv("S3_ENDPOINT")
	c.S3PublicEndpoint = os.Getenv("S3_PUBLIC_ENDPOINT")
	c.S3Region = env("S3_REGION", "auto")
	c.S3Bucket = os.Getenv("S3_BUCKET")
	c.S3AccessKeyID = os.Getenv("S3_ACCESS_KEY_ID")
	c.S3SecretAccessKey = os.Getenv("S3_SECRET_ACCESS_KEY")
	c.S3PathStyle = env("S3_PATH_STYLE", "false") == "true"
	c.S3CreateBucket = env("S3_CREATE_BUCKET", "false") == "true"
	for _, o := range strings.Split(os.Getenv("S3_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.S3CORSOrigins = append(c.S3CORSOrigins, o)
		}
	}
	if c.S3Bucket != "" && (c.S3Endpoint == "" || c.S3AccessKeyID == "" || c.S3SecretAccessKey == "") {
		return c, fmt.Errorf("S3_BUCKET is set: S3_ENDPOINT, S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required")
	}
	c.AppURL = env("APP_URL", "http://localhost:5173")
	c.CookieSecure = env("COOKIE_SECURE", "true") == "true"
	c.GoogleDevLogin = env("GOOGLE_DEV_LOGIN", "false") == "true"
	c.SimulateCounterparties = env("SIMULATE_COUNTERPARTIES", "false") == "true"
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
