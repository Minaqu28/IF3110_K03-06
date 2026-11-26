package database_test

import (
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func (h *targetHarness) tableExists(t *testing.T, name string) bool {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`, name) == 1
}

func TestTargetExecutorRollbackAndForwardPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `CREATE TABLE gadgets (id int)`, `DROP TABLE gadgets`)
	if r := h.run(t, h.request(rev, domain.MigrationActionApply, execA, ""), rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("apply = %+v", r)
	}

	rollback := h.request(rev, domain.MigrationActionRollback, execB, execA)
	r := h.run(t, rollback, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.RevertsExecutionID == nil || *r.RevertsExecutionID != execA {
		t.Fatalf("rollback = %+v", r)
	}
	if h.tableExists(t, "gadgets") {
		t.Fatal("down SQL did not run")
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execB {
		t.Fatalf("ledger after rollback = %s / %s", status, execution)
	}
	if r := h.run(t, rollback, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("rollback replay = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execC, execA), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rolling back the same apply twice = %+v", r)
	}

	forward := h.request(rev, domain.MigrationActionForward, execC, "")
	if r := h.run(t, forward, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("forward = %+v", r)
	}
	if !h.tableExists(t, "gadgets") {
		t.Fatal("forward did not re-run the up SQL")
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != execC {
		t.Fatalf("ledger after forward = %s / %s", status, execution)
	}
	if r := h.run(t, forward, rev); r.Status != domain.ExecutionStatusSucceeded || !h.tableExists(t, "gadgets") {
		t.Fatalf("forward replay = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionForward, execD, ""), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("forward of an applied migration = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execD, execA), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rollback aimed at the superseded apply = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execD, execC), rev); r.Status != domain.ExecutionStatusSucceeded || h.tableExists(t, "gadgets") {
		t.Fatalf("rollback of the forward = %+v", r)
	}
}

func TestTargetExecutorRollbackRefusalsPostgres(t *testing.T) {
	h := newTargetHarness(t)
	irreversible := h.migration(t, 1, `CREATE TABLE logs (id int)`, "")
	h.run(t, h.request(irreversible, domain.MigrationActionApply, execA, ""), irreversible)
	if r := h.run(t, h.request(irreversible, domain.MigrationActionRollback, execB, execA), irreversible); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeIrreversible {
		t.Fatalf("irreversible rollback = %+v", r)
	}

	other := h.migration(t, 2, `CREATE TABLE notes (id int)`, `DROP TABLE notes`)
	if r := h.run(t, h.request(other, domain.MigrationActionRollback, execB, execA), other); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rollback aimed at another migration's execution = %+v", r)
	}
	if r := h.run(t, h.request(other, domain.MigrationActionForward, execB, ""), other); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("forward of a never-applied migration = %+v", r)
	}

	failing := h.migration(t, 3, `CREATE TABLE kept (id int)`, `DROP TABLE nope`)
	h.run(t, h.request(failing, domain.MigrationActionApply, execC, ""), failing)
	if r := h.run(t, h.request(failing, domain.MigrationActionRollback, execD, execC), failing); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != "42P01" {
		t.Fatalf("failing down SQL = %+v", r)
	}
	if status, execution := h.ledger(t, failing.MigrationID); status != "APPLIED" || execution != execC || !h.tableExists(t, "kept") {
		t.Fatalf("a failed rollback changed the ledger: %s / %s", status, execution)
	}
	if status, _ := h.ledger(t, irreversible.MigrationID); status != "APPLIED" {
		t.Fatal("the refused rollback changed the ledger")
	}
}
