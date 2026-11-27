package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func Open(ctx context.Context, url string) (*sql.DB, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration: %w", err)
	}

	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = db.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return db, nil
		}
		select {
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	_ = db.Close()
	return nil, fmt.Errorf("connect to database: %w", lastErr)
}

func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return err
	}
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, name := range files {
		err = applyMigration(ctx, db, fsys, name)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, name string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7310001)`); err != nil {
		return err
	}
	var applied bool
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
		name).Scan(&applied)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return err
	}

	return tx.Commit()
}

func BootstrapAdmin(ctx context.Context, db *sql.DB, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return errors.New("bootstrap username and password are required")
	}
	hash, err := authentication.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO users (username, password_hash, is_platform_admin)
		VALUES ($1, $2, true) ON CONFLICT (username) DO NOTHING`, username, hash)
	return err
}

type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
