package database_test

import (
	"context"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

func TestEngineRunsAndRecordsEveryAttemptPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	_, req := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	history := database.NewHistoryPostgresStore(h.platform)
	engine := migrationengine.NewEngine(database.NewRevisionPostgresStore(h.platform), history, h.executor)

	first, err := engine.Execute(ctx, req)
	if err != nil || first.Status != domain.ExecutionStatusPartial || first.AppliedBatches != 2 {
		t.Fatalf("first run = %+v, err = %v", first, err)
	}
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	second, err := engine.Execute(ctx, req)
	if err != nil || second.Status != domain.ExecutionStatusSucceeded || second.AppliedBatches != 5 {
		t.Fatalf("resume = %+v, err = %v", second, err)
	}

	attempts, err := history.ListAttempts(ctx, h.databaseID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
	for i, want := range []domain.ExecutionStatus{domain.ExecutionStatusPartial, domain.ExecutionStatusSucceeded} {
		a := attempts[i]
		if a.ExecutionID != req.ExecutionID || a.AttemptNumber != i+1 || a.Status != want || a.SQLSnapshot != batchUp || a.FinishedAt == nil {
			t.Fatalf("attempt %d = %+v", i+1, a)
		}
	}
	if attempts[0].ErrorCode != "22012" || attempts[1].ErrorCode != "" {
		t.Fatalf("error codes = %q, %q", attempts[0].ErrorCode, attempts[1].ErrorCode)
	}
}

func TestEngineRefusesATamperedRevisionPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 10)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 10)
	execSQL(t, h.platform, `ALTER TABLE migration_revisions DISABLE TRIGGER USER`)
	execSQL(t, h.platform, `UPDATE migration_revisions SET up_sql = 'UPDATE public.orders SET v = 999 WHERE id > $1 AND id <= $2' WHERE id = $1`, rev.ID)
	execSQL(t, h.platform, `ALTER TABLE migration_revisions ENABLE TRIGGER USER`)

	history := database.NewHistoryPostgresStore(h.platform)
	engine := migrationengine.NewEngine(database.NewRevisionPostgresStore(h.platform), history, h.executor)
	res, err := engine.Execute(context.Background(), req)
	if err != nil || res.Status != domain.ExecutionStatusFailed || res.ErrorCode != migrationengine.ErrorCodeDefinitionChanged {
		t.Fatalf("tampered revision = %+v, err = %v", res, err)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatal("tampered SQL reached the target")
	}
	attempts, err := history.ListAttempts(context.Background(), h.databaseID)
	if err != nil || len(attempts) != 1 || attempts[0].ErrorCode != migrationengine.ErrorCodeDefinitionChanged {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
}
