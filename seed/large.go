package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
)

const (
	largeTable        = "public.large_orders"
	defaultLargeRows  = 3_000_000
	largeInsertChunk  = 500_000
	largeSeedTimeout  = 20 * time.Minute
	unbatchedUpdateUp = "UPDATE public.large_orders SET amount_cents = round(amount * 100)::bigint"
	batchUpSQL        = "UPDATE public.large_orders SET amount_cents = round(amount * 100)::bigint WHERE id > $1 AND id <= $2"
	batchDownSQL      = "UPDATE public.large_orders SET amount_cents = NULL WHERE id > $1 AND id <= $2"
)

type largeSeedResult struct {
	rows     int64
	inserted int64
	updated  int64
	duration time.Duration
}

func largeRowsFromEnv() (int64, error) {
	raw := getenv("SEED_LARGE_ROWS", strconv.Itoa(defaultLargeRows))
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("SEED_LARGE_ROWS must be a non-negative integer, got %q", raw)
	}
	return n, nil
}

func seedLargeOrders(dsn string, target int64) (largeSeedResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), largeSeedTimeout)
	defer cancel()
	db, err := database.Open(ctx, dsn)
	if err != nil {
		return largeSeedResult{}, err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS public.large_orders (
			id           BIGINT PRIMARY KEY,
			customer_id  INTEGER       NOT NULL,
			status       TEXT          NOT NULL,
			amount       NUMERIC(12,2) NOT NULL,
			amount_cents BIGINT,
			created_at   TIMESTAMPTZ   NOT NULL
		)`); err != nil {
		return largeSeedResult{}, err
	}
	inserted, err := fillLargeOrders(ctx, db, target)
	if err != nil {
		return largeSeedResult{}, err
	}
	if inserted > 0 {
		if _, err := db.ExecContext(ctx, `ANALYZE public.large_orders`); err != nil {
			return largeSeedResult{}, err
		}
	}
	result := largeSeedResult{inserted: inserted}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.large_orders`).Scan(&result.rows); err != nil {
		return largeSeedResult{}, err
	}
	result.updated, result.duration, err = timeUnbatchedUpdate(ctx, db)
	if err != nil {
		return largeSeedResult{}, err
	}
	return result, nil
}

func fillLargeOrders(ctx context.Context, db *sql.DB, target int64) (int64, error) {
	var next int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) + 1 FROM public.large_orders`).Scan(&next); err != nil {
		return 0, err
	}
	var inserted int64
	for from := next; from <= target; from += largeInsertChunk {
		to := min(from+largeInsertChunk-1, target)
		res, err := db.ExecContext(ctx, `
			INSERT INTO public.large_orders (id, customer_id, status, amount, created_at)
			SELECT g,
			       (g % 5000) + 1,
			       (ARRAY['pending', 'paid', 'shipped', 'delivered', 'cancelled'])[(g % 5) + 1],
			       ((g * 7919) % 100000) / 100.0,
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + g * INTERVAL '1 second'
			FROM generate_series($1::bigint, $2::bigint) AS g
			ON CONFLICT (id) DO NOTHING`, from, to)
		if err != nil {
			return inserted, fmt.Errorf("insert large_orders %d-%d: %w", from, to, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return inserted, err
		}
		inserted += n
		fmt.Printf("large_orders: inserted rows %d-%d\n", from, to)
	}
	return inserted, nil
}

func timeUnbatchedUpdate(ctx context.Context, db *sql.DB) (int64, time.Duration, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return 0, 0, err
	}
	start := time.Now()
	res, err := tx.ExecContext(ctx, unbatchedUpdateUp)
	elapsed := time.Since(start)
	if err != nil {
		return 0, 0, fmt.Errorf("time unbatched update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	return n, elapsed, tx.Rollback()
}

func printLargeSummary(r largeSeedResult) {
	fmt.Println("\nLarge dataset for batch migration testing (database \"shop\")")
	fmt.Printf("  table            : %s\n", largeTable)
	fmt.Printf("  rows             : %d (inserted this run: %d)\n", r.rows, r.inserted)
	fmt.Printf("  unbatched UPDATE : %d rows in %s (run in a transaction, then rolled back)\n", r.updated, r.duration.Round(time.Millisecond))
	fmt.Printf("  statement        : %s\n", unbatchedUpdateUp)
	if r.duration <= time.Second {
		fmt.Println("  WARNING          : the unbatched UPDATE took 1s or less; raise SEED_LARGE_ROWS so it exceeds 1s")
	}
	fmt.Println("\n  Batch migration on the same dataset (Mode Batch in the SPA):")
	fmt.Printf("    table  : %s\n    cursor : id\n    size   : 10000\n", largeTable)
	fmt.Printf("    up     : %s\n    down   : %s\n", batchUpSQL, batchDownSQL)
}
