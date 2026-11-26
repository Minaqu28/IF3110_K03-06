package database

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/jackc/pgx/v5/pgconn"
)

func requireTargetSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

func TestEnsureBookkeepingIntegration(t *testing.T) {
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	for range 2 {
		if err := ensureBookkeeping(ctx, target.conn); err != nil {
			t.Fatal(err)
		}
	}
	var tables int
	if err := target.conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'
		AND table_name IN ('_dbmp_migrations', '_dbmp_batch_checkpoints', '_dbmp_batch_backup')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("bookkeeping tables = %d, err = %v", tables, err)
	}

	exec := "11111111-1111-1111-1111-111111111111"
	if _, err := target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id, status)
		VALUES (1, 10, 'SELECT 1', 1, $1, 'REVERTED')`, exec); err != nil {
		t.Fatalf("widened status and execution_id: %v", err)
	}
	_, err := target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id)
		VALUES (2, 11, 'SELECT 1', 1, $1)`, exec)
	requireTargetSQLState(t, err, "23505")
	_, err = target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, status)
		VALUES (3, 12, 'SELECT 1', 1, 'BOGUS')`)
	requireTargetSQLState(t, err, "23514")

	checkpoint := `INSERT INTO public._dbmp_batch_checkpoints
		(source_execution_id, batch_no, direction, execution_id, cursor_from, cursor_to, row_count)
		VALUES ($1, 1, $2, $1, 0, 100, 100)`
	if _, err := target.conn.Exec(ctx, checkpoint, exec, "UP"); err != nil {
		t.Fatal(err)
	}
	_, err = target.conn.Exec(ctx, checkpoint, exec, "UP")
	requireTargetSQLState(t, err, "23505")
	if _, err := target.conn.Exec(ctx, checkpoint, exec, "DOWN"); err != nil {
		t.Fatalf("a DOWN checkpoint for the same batch: %v", err)
	}
	_, err = target.conn.Exec(ctx, checkpoint, exec, "SIDEWAYS")
	requireTargetSQLState(t, err, "23514")

	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatalf("rerun with data present: %v", err)
	}
	var rows int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_checkpoints`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("checkpoints after rerun = %d, err = %v", rows, err)
	}
}

func TestEnsureBookkeepingUpgradesTheLegacyLedgerIntegration(t *testing.T) {
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	_, applied, err := target.ApplyVersionedMigration(ctx, explorer.Migration{ID: 7, Version: 1, SQL: "CREATE TABLE legacy (id int)"}, 1, 10)
	if err != nil || applied {
		t.Fatalf("legacy apply: applied = %v, err = %v", applied, err)
	}
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatal(err)
	}
	var status string
	var execution *string
	if err := target.conn.QueryRow(ctx, `SELECT status, execution_id::text FROM public._dbmp_migrations WHERE migration_id = 7`).Scan(&status, &execution); err != nil {
		t.Fatal(err)
	}
	if status != "APPLIED" || execution != nil {
		t.Fatalf("legacy row = %s / %v", status, execution)
	}
	if _, applied, err := target.ApplyVersionedMigration(ctx, explorer.Migration{ID: 8, Version: 2, SQL: "SELECT 1"}, 1, 10); err != nil || applied {
		t.Fatalf("legacy apply after the upgrade: applied = %v, err = %v", applied, err)
	}
}

func TestEnsureBookkeepingConcurrentIntegration(t *testing.T) {
	targets := openIsolatedTargets(t, 4)
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureBookkeeping(context.Background(), target.conn)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bookkeeping: %v", err)
		}
	}
}
