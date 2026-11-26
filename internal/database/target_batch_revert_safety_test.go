package database_test

import (
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

func TestTargetExecutorRefusesUnsafeBatchRevertsPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 30)
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 30)
	h.run(t, apply, rev)
	later := h.migration(t, 2, `CREATE TABLE later (id int)`, `DROP TABLE later`)
	if r := h.run(t, h.request(later, domain.MigrationActionApply, execB, ""), later); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("later migration = %+v", r)
	}

	refusedWith := func(name string, req domain.ExecutionRequest, rv domain.MigrationRevision) {
		t.Helper()
		r := h.run(t, req, rv)
		if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeUnsafeRevert ||
			!strings.Contains(r.ErrorMessage, migrationengine.ErrUnsafeRevertMessage) || r.AppliedBatches != 0 {
			t.Fatalf("%s = %+v, want an UNSAFE_REVERT refusal", name, r)
		}
		if changed, sum := h.ordersChanged(t); changed != 30 || sum != 3000 {
			t.Fatalf("%s changed the data: %d rows, sum %d", name, changed, sum)
		}
	}

	refusedWith("revert while a later migration is applied", revertOf(apply, execA), rev)

	other := rev
	other.ID++
	wrongRevision := revertOf(apply, execA)
	wrongRevision.RevisionID = other.ID
	refusedWith("revert with another revision's down SQL", wrongRevision, other)

	unknown := revertOf(apply, execA)
	missing := "dddddddd-0000-4000-8000-000000000001"
	unknown.TargetExecutionID = &missing
	refusedWith("revert of an unknown execution", unknown, rev)

	if r := h.run(t, h.request(later, domain.MigrationActionRollback, execC, execB), later); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("rolling back the later migration = %+v", r)
	}
	if r := h.run(t, revertOf(apply, execA), rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 3 {
		t.Fatalf("revert once the later migration is rolled back = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatalf("%d rows still changed", changed)
	}
}

func TestTargetExecutorRefusesIrreversibleBatchRevertPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 20)
	rev, apply := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 20)
	h.run(t, apply, rev)
	r := h.run(t, revertOf(apply, execA), rev)
	if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeIrreversible || r.ErrorMessage != migrationengine.ErrUnsafeRevertMessage {
		t.Fatalf("irreversible revert = %+v", r)
	}
	if status, _ := h.ledger(t, rev.MigrationID); status != "APPLIED" {
		t.Fatal("a refused revert changed the ledger")
	}
}
