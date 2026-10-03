package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
	"github.com/azdharsyahputra/ecopurnity-api/internal/sms"
	"github.com/azdharsyahputra/ecopurnity-api/internal/storage"
	"github.com/azdharsyahputra/ecopurnity-api/migrations"
)

// Integration tests run against a real PostgreSQL: a fresh database per test binary, migrated with the real
// migrations, dropped at the end. TEST_POSTGRES_URL points at a server where the user may create databases
// (default: the docker-compose primary). Tests are skipped when it is unreachable.

var testDSN string

func TestMain(m *testing.M) {
	admin := os.Getenv("TEST_POSTGRES_URL")
	if admin == "" {
		admin = "postgres://ecopurnity:ecopurnity@localhost:5432/postgres?sslmode=disable"
	}
	name := fmt.Sprintf("ecp_test_%d", time.Now().UnixNano())
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err == nil {
		_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration tests skipped: no postgres:", err)
		os.Exit(m.Run())
	}
	// Point the DSN at the new database. (pgx.Config.ConnString() returns the original string, so rewrite the URL.)
	u, err := url.Parse(admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "TEST_POSTGRES_URL:", err)
		os.Exit(1)
	}
	u.Path = "/" + name
	testDSN = u.String()
	if err := migrate(testDSN); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	code := m.Run()
	_, _ = conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	_ = conn.Close(ctx)
	os.Exit(code)
}

func migrate(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	goose.SetBaseFS(migrations.Postgres)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, "postgres")
}

type testEnv struct {
	server *Server
	t      *testing.T
	srv    *httptest.Server
	mail   *mail.Memory
	db     *db.Cluster
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	if testDSN == "" {
		t.Skip("no postgres")
	}
	cluster, err := db.Open(context.Background(), testDSN, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	mem := &mail.Memory{}
	keys, err := secure.Derive([]byte("test-secret-test-secret-test-secret!!"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: cluster, Mail: mem, SMS: &sms.Memory{}, Keys: keys, Storage: testStorage(),
		Log: slog.New(slog.NewTextHandler(testLog{t}, nil)), AppURL: "http://app.test", SessionTTL: 24 * time.Hour, GoogleDevLogin: true}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv, mail: mem, db: cluster, server: s}
}

// client is a browser: it keeps cookies.
func (e *testEnv) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

type resp struct {
	Status int
	Body   map[string]any
	Header http.Header
}

func (r resp) code() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r resp) field(name string) string {
	e, _ := r.Body["error"].(map[string]any)
	f, _ := e["fields"].(map[string]any)
	v, _ := f[name].(string)
	return v
}

func (r resp) message() string {
	e, _ := r.Body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func (e *testEnv) call(c *http.Client, method, path string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+BasePath+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{Status: res.StatusCode, Header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *testEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Primary().Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) scalar(sql string, args ...any) any {
	e.t.Helper()
	var v any
	if err := e.db.Primary().QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// uniqueEmail keeps tests independent inside the shared test database.
func uniqueEmail(t *testing.T, prefix string) string {
	return fmt.Sprintf("%s.%d@example.id", prefix, time.Now().UnixNano())
}

// lastMail waits for queued emails and returns the last one sent to `to`.
func (e *testEnv) lastMail(to string) (mail.Message, bool) {
	e.server.WaitMail()
	return e.mail.Last(to)
}

var (
	storeOnce sync.Once
	store     *storage.Store
)

// testStorage is the docker-compose SeaweedFS (TEST_S3_ENDPOINT to override) with a test bucket, or nil when it is not
// running (upload tests then skip).
func testStorage() *storage.Store {
	storeOnce.Do(func() {
		endpoint := os.Getenv("TEST_S3_ENDPOINT")
		if endpoint == "" {
			endpoint = "http://localhost:8333"
		}
		st := storage.New(storage.Config{Endpoint: endpoint, Region: "auto", Bucket: "ecopurnity-test",
			AccessKeyID: "dev-access-key", SecretAccessKey: "dev-secret-key", PathStyle: true})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if st.EnsureBucket(ctx) == nil {
			store = st
		}
	})
	return store
}

func (e *testEnv) sms() *sms.Memory { return e.server.SMS.(*sms.Memory) }

func t0() context.Context { return context.Background() }

// testLog sends server logs to the test's log, shown only when the test fails.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Helper()
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
