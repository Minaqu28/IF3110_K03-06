package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

const targetLockKey = 7310002

func TestTargetExecutorSecondBatchRunOnSameDatabaseIsBusyPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 30)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 30)
	other := database.NewTargetExecutor(h.store, database.WorkerTargetOptions(time.Minute), nil)

	var secondErr error
	calls := 0
	h.executor.SetBeforeCommit(func(uint32) {
		if calls++; calls == 2 {
			_, secondErr = other.Execute(context.Background(), req, rev)
		}
	})
	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 3 {
		t.Fatalf("first executor = %+v", r)
	}
	if !errors.Is(secondErr, migrationengine.ErrTargetBusy) || migrationengine.ClassifyError(secondErr) != migrationengine.ErrorTransient {
		t.Fatalf("second executor between batch commits: err = %v, want a transient busy error", secondErr)
	}
	if changed, sum := h.ordersChanged(t); changed != 30 || sum != 3000 {
		t.Fatalf("%d rows changed, sum %d; the second executor must not run a batch", changed, sum)
	}
}

func TestTargetExecutorBusyUntilTheSessionLockIsReleasedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 20)
	ctx := context.Background()
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 20)
	if _, err := h.target.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(targetLockKey)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := h.executor.Execute(ctx, req, rev); !errors.Is(err, migrationengine.ErrTargetBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("a busy target took %v to report; it must not wait for the lock", waited)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 || h.tableExists(t, "_dbmp_batch_checkpoints") {
		t.Fatal("a busy run touched the target before taking the lock")
	}
	if _, err := h.target.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(targetLockKey)); err != nil {
		t.Fatal(err)
	}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 2 {
		t.Fatalf("after release = %+v", r)
	}
	var held int
	if err := h.target.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1`, targetLockKey).Scan(&held); err != nil || held != 0 {
		t.Fatalf("advisory locks still held after the run: %d, err = %v", held, err)
	}
}
