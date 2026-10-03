// Command migrate applies the PostgreSQL migrations to the primary (never the replica: it follows via WAL).
//
//	go run ./cmd/migrate up | down | status | redo | reset | version
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/azdharsyahputra/ecopurnity-api/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	url := os.Getenv("POSTGRES_PRIMARY_URL")
	if url == "" {
		return fmt.Errorf("POSTGRES_PRIMARY_URL is required")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetBaseFS(migrations.Postgres)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.RunContext(context.Background(), cmd, db, "postgres", os.Args[2:]...)
}
