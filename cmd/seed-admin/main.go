package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 || os.Args[1] == "" {
		return fmt.Errorf("usage: seed-admin <email>")
	}
	url := os.Getenv("POSTGRES_PRIMARY_URL")
	if url == "" {
		return fmt.Errorf("POSTGRES_PRIMARY_URL is required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var id, name string
		if err := tx.QueryRow(ctx, `SELECT id, name FROM users WHERE email = $1`, os.Args[1]).Scan(&id, &name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no account with email %s: register it first", os.Args[1])
			}
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO user_capabilities (user_id, capability) VALUES ($1, 'admin') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			fmt.Printf("%s already has the admin capability\n", name)
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_log (actor_label, action, entity_type, entity_id, entity_label, changes)
			VALUES ('Sistem', 'Beri capability admin', 'user', $1, $2, '[{"field":"Capability","after":"admin"}]')`, id, name); err != nil {
			return err
		}
		fmt.Printf("granted admin to %s (%s)\n", name, id)
		return nil
	})
}
