package database_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const batchDown = `UPDATE public.orders SET v = v - 100 / d WHERE id > $1 AND id <= $2`

func revertOf(apply domain.ExecutionRequest, execution string) domain.ExecutionRequest {
	req := apply
	target := apply.ExecutionID
	req.Action, req.ExecutionID, req.TargetExecutionID = domain.MigrationActionRollback, execution, &target
	return req
}

func (h *targetHarness) downOrder(t *testing.T, source string) []int {
	t.Helper()
	rows, err := h.target.Query(context.Background(), `
		SELECT batch_no FROM public._dbmp_batch_checkpoints
		WHERE source_execution_id = $1 AND direction = 'DOWN' ORDER BY committed_at, batch_no`, source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		order = append(order, n)
	}
	return order
}

func TestTargetExecutorBatchRevertInReverseOrderPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	if r := h.run(t, apply, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 {
		t.Fatalf("apply = %+v", r)
	}
	log := &progressLog{}
	h.withProgress(log)

	revert := revertOf(apply, execA)
	r := h.run(t, revert, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 || r.AffectedRows != 50 ||
		r.RevertsExecutionID == nil || *r.RevertsExecutionID != apply.ExecutionID {
		t.Fatalf("revert = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatalf("%d rows still changed after the revert", changed)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3, 2, 1}) {
		t.Fatalf("DOWN checkpoints in order %v, want 5..1", order)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
	completed, cursors := log.summary()
	if !slices.Equal(completed, []int{1, 2, 3, 4, 5}) || cursors[0] != "40" || cursors[4] != "0" {
		t.Fatalf("revert reported %v / %v", completed, cursors)
	}
	for _, c := range log.calls {
		if c.Status != domain.BatchStatusCompensated {
			t.Fatalf("revert progress status = %s", c.Status)
		}
	}

	if again := h.run(t, revert, rev); again.Status != domain.ExecutionStatusSucceeded || again.AppliedBatches != 5 {
		t.Fatalf("revert replay = %+v", again)
	}
	if _, sum := h.ordersChanged(t); sum != 0 {
		t.Fatal("a replayed revert ran down SQL again")
	}
}

func TestTargetExecutorBatchRevertOfAPartialApplyPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	if r := h.run(t, apply, rev); r.Status != domain.ExecutionStatusPartial || r.AppliedBatches != 2 {
		t.Fatalf("apply = %+v", r)
	}
	r := h.run(t, revertOf(apply, execA), rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 2 || r.AffectedRows != 20 {
		t.Fatalf("revert of a partial apply = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a reverted partial apply left changes or a ledger row")
	}
}

func TestTargetExecutorBatchRevertPartialThenResumePostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	h.run(t, apply, rev)
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 15`); err != nil {
		t.Fatal(err)
	}

	revert := revertOf(apply, execA)
	r := h.run(t, revert, rev)
	if r.Status != domain.ExecutionStatusPartiallyReverted || r.ErrorCode != "22012" || r.AppliedBatches != 3 || r.AffectedRows != 30 {
		t.Fatalf("revert failing at batch 2 = %+v, want PARTIALLY_REVERTED after 3 batches", r)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3}) {
		t.Fatalf("DOWN checkpoints %v, want 5, 4, 3", order)
	}
	if status, _ := h.ledger(t, rev.MigrationID); status != "APPLIED" {
		t.Fatal("a partial revert must leave the ledger APPLIED")
	}
	if h.count(t, `SELECT count(*) FROM public.orders WHERE id <= 20 AND v = 100`) != 20 {
		t.Fatal("batches 1 and 2 must stay applied")
	}

	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 15`); err != nil {
		t.Fatal(err)
	}
	log := &progressLog{}
	h.withProgress(log)
	resumed := h.run(t, revert, rev)
	if resumed.Status != domain.ExecutionStatusSucceeded || resumed.AppliedBatches != 5 || resumed.AffectedRows != 50 {
		t.Fatalf("resumed revert = %+v", resumed)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3, 2, 1}) {
		t.Fatalf("DOWN checkpoints after resume %v", order)
	}
	if changed, sum := h.ordersChanged(t); changed != 0 || sum != 0 {
		t.Fatalf("after the resumed revert: %d rows changed, sum %d", changed, sum)
	}
	if completed, _ := log.summary(); !slices.Equal(completed, []int{3, 4, 5}) {
		t.Fatalf("resumed revert reported %v; want the 3 already reverted, then 4 and 5", completed)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
}
