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

	PostgresPrimaryURL string

	PostgresReplicaURL string

	ClickHouseAddr     string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string

	ReplicaMaxLag time.Duration

	AppURL string

	CookieSecure bool

	SessionTTL time.Duration

	Secret []byte

	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPTLS      string

	S3Endpoint        string
	S3PublicEndpoint  string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3PathStyle       bool
	S3CreateBucket    bool
	S3CORSOrigins     []string

	GoogleDevLogin bool

	SimulateCounterparties bool

	MidtransServerKey  string
	MidtransClientKey  string
	MidtransProduction bool
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
	c.MidtransServerKey = strings.TrimSpace(os.Getenv("MIDTRANS_SERVER_KEY"))
	c.MidtransClientKey = strings.TrimSpace(os.Getenv("MIDTRANS_CLIENT_KEY"))
	switch env("MIDTRANS_ENV", "sandbox") {
	case "sandbox":
	case "production":
		c.MidtransProduction = true
	default:
		return c, fmt.Errorf("MIDTRANS_ENV must be sandbox or production")
	}
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
