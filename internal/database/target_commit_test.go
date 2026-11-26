package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

func TestTargetExecutorLostCommitIsUnknownPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `CREATE TABLE widgets (id int)`, "")
	req := h.request(rev, domain.MigrationActionApply, execA, "")
	h.executor.SetBeforeCommit(func(pid uint32) {
		var ok bool
		if err := h.target.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, int64(pid)).Scan(&ok); err != nil || !ok {
			t.Errorf("terminate backend %d: ok = %v, err = %v", pid, ok, err)
		}
	})

	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusUnknown || r.ErrorCode != database.ErrorCodeCommitUncertain || r.FinishedAt == nil ||
		!strings.Contains(r.ErrorMessage, migrationengine.ErrCommitUncertain.Error()) {
		t.Fatalf("lost commit = %+v", r)
	}
	if h.tableExists(t, "widgets") || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("the terminated transaction left changes behind")
	}

	h.executor.SetBeforeCommit(nil)
	again := h.run(t, req, rev)
	if again.Status != domain.ExecutionStatusSucceeded || !h.tableExists(t, "widgets") {
		t.Fatalf("reconciling retry = %+v", again)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AffectedRows != 0 {
		t.Fatalf("second retry should only reconcile: %+v", r)
	}
}

func TestTargetExecutorRejectedCommitIsFailedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `
		CREATE TABLE parents (id int PRIMARY KEY);
		CREATE TABLE children (parent int REFERENCES parents DEFERRABLE INITIALLY DEFERRED);
		INSERT INTO children VALUES (1)`, "")
	r := h.run(t, h.request(rev, domain.MigrationActionApply, execA, ""), rev)
	if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != "23503" {
		t.Fatalf("a commit the server refused = %+v, want FAILED 23503", r)
	}
	if h.tableExists(t, "parents") || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a refused commit left changes behind")
	}
}
