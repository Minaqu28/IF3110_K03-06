package database_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type progressLog struct {
	mu     sync.Mutex
	ids    []string
	calls  []domain.BatchProgress
	failAt int
}

func (p *progressLog) sink(_ context.Context, executionID string, progress domain.BatchProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, executionID)
	p.calls = append(p.calls, progress)
	if len(p.calls) == p.failAt {
		return errors.New("platform database unavailable")
	}
	return nil
}

func (p *progressLog) summary() (completed []int, cursors []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		completed = append(completed, c.Completed)
		cursors = append(cursors, c.LastCursor)
	}
	return completed, cursors
}

func (h *targetHarness) withProgress(p *progressLog) {
	h.executor = database.NewTargetExecutor(h.store, database.WorkerTargetOptions(time.Minute), p.sink)
}

func TestTargetExecutorReportsEveryCommittedBatchPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	log := &progressLog{}
	h.withProgress(log)

	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusPartial || r.AppliedBatches != 2 {
		t.Fatalf("first attempt = %+v", r)
	}
	completed, cursors := log.summary()
	if !slices.Equal(completed, []int{1, 2}) || cursors[1] != "20" {
		t.Fatalf("first attempt reported %v / %v, want one call per committed batch", completed, cursors)
	}
	for i, c := range log.calls {
		if c.Status != domain.BatchStatusCommitted || c.AffectedRows != int64(10*(i+1)) || log.ids[i] != req.ExecutionID {
			t.Fatalf("call %d = %+v for %s", i, c, log.ids[i])
		}
	}

	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	*log = progressLog{}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("resume = %+v", r)
	}
	completed, cursors = log.summary()
	if !slices.Equal(completed, []int{2, 3, 4, 5}) || cursors[0] != "20" || cursors[3] != "50" {
		t.Fatalf("resume reported %v / %v; want the reconciled checkpoint first, then one call per batch", completed, cursors)
	}
}

func TestTargetExecutorStopsWhenProgressCannotBeSavedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	h.withProgress(&progressLog{failAt: 2})

	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusPartial || r.ErrorCode != database.ErrorCodeInterrupted || r.AppliedBatches != 2 {
		t.Fatalf("progress failure = %+v, want PARTIAL after the batch that could not be reported", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 20 {
		t.Fatalf("%d rows changed; the run must stop right after the failed report", changed)
	}
	h.withProgress(&progressLog{})
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 {
		t.Fatalf("resume = %+v", r)
	}
}
