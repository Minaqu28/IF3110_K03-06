package migrationengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type engineHarness struct {
	rev       domain.MigrationRevision
	revErr    error
	targetRes domain.ExecutionResult
	targetErr error
	executed  []domain.MigrationRevision
	requests  []domain.ExecutionRequest
	attempts  []domain.ExecutionAttempt
	appendErr error
	engine    *Engine
}

func hashed(rev domain.MigrationRevision) domain.MigrationRevision {
	rev.DefinitionHash = domain.ComputeDefinitionHash(rev)
	return rev
}

func newEngineHarness() *engineHarness {
	h := &engineHarness{
		rev: hashed(domain.MigrationRevision{
			ID: 7, MigrationID: 3, DatabaseID: 1, UpSQL: "CREATE TABLE t (id int)", DownSQL: "DROP TABLE t", Reversible: true,
		}),
		targetRes: domain.ExecutionResult{ExecutionID: "exec-1", Status: domain.ExecutionStatusSucceeded},
	}
	revisions := &mockRevisionStore{getRevision: func(_ context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error) {
		if databaseID != h.rev.DatabaseID || revisionID != h.rev.ID {
			return domain.MigrationRevision{}, admin.ErrNotFound
		}
		return h.rev, h.revErr
	}}
	history := &mockHistoryStore{appendAttempt: func(_ context.Context, a domain.ExecutionAttempt) error {
		h.attempts = append(h.attempts, a)
		return h.appendErr
	}}
	target := &mockTargetExecutor{execute: func(_ context.Context, req domain.ExecutionRequest, rev domain.MigrationRevision) (domain.ExecutionResult, error) {
		h.requests, h.executed = append(h.requests, req), append(h.executed, rev)
		return h.targetRes, h.targetErr
	}}
	h.engine = NewEngine(revisions, history, target)
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h.engine.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	return h
}

func engineRequest(action domain.MigrationAction) domain.ExecutionRequest {
	req := domain.ExecutionRequest{ExecutionID: "exec-1", DatabaseID: 1, MigrationID: 3, RevisionID: 7, ActorID: 5, Action: action, Mode: domain.ExecutionModeNormal}
	if action == domain.MigrationActionRollback {
		target := "exec-0"
		req.TargetExecutionID = &target
	}
	return req
}

func TestEngineRunsTheStoredRevision(t *testing.T) {
	h := newEngineHarness()
	req := engineRequest(domain.MigrationActionApply)
	res, err := h.engine.Execute(context.Background(), req)
	if err != nil || res.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if len(h.executed) != 1 || h.executed[0].UpSQL != h.rev.UpSQL || h.requests[0] != req {
		t.Fatalf("target got %+v / %+v", h.executed, h.requests)
	}
}

func TestEngineRefusesAChangedDefinition(t *testing.T) {
	for name, mutate := range map[string]func(*domain.MigrationRevision){
		"up SQL edited":   func(r *domain.MigrationRevision) { r.UpSQL = "DROP TABLE everything" },
		"down SQL edited": func(r *domain.MigrationRevision) { r.DownSQL = "SELECT 1" },
		"batch size edited": func(r *domain.MigrationRevision) {
			r.Batch = &domain.BatchConfig{Table: "t", CursorColumn: "id", BatchSize: 1}
		},
		"no hash": func(r *domain.MigrationRevision) { r.DefinitionHash = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness()
			mutate(&h.rev)
			res, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionRollback))
			if err != nil || res.Status != domain.ExecutionStatusFailed || res.ErrorCode != ErrorCodeDefinitionChanged ||
				res.ExecutionID != "exec-1" || res.RevertsExecutionID == nil || res.FinishedAt == nil || !res.FinishedAt.After(res.StartedAt) {
				t.Fatalf("result = %+v, err = %v", res, err)
			}
			if len(h.executed) != 0 {
				t.Fatal("a changed definition reached the target")
			}
		})
	}
}

func TestEngineRefusesAnInvalidBatch(t *testing.T) {
	h := newEngineHarness()
	h.rev.UpSQL = "UPDATE t SET a = 1"
	h.rev.DownSQL = "UPDATE t SET a = 0 WHERE id > $1 AND id <= $2"
	h.rev.Batch = &domain.BatchConfig{Table: "t", CursorColumn: "id", BatchSize: 10}
	h.rev = hashed(h.rev)
	req := engineRequest(domain.MigrationActionApply)
	req.Mode = domain.ExecutionModeBatch
	res, err := h.engine.Execute(context.Background(), req)
	if err != nil || res.ErrorCode != ErrorCodeInvalidBatch || len(h.executed) != 0 {
		t.Fatalf("result = %+v, err = %v, executed %d", res, err, len(h.executed))
	}
}

func TestEnginePassesErrorsThrough(t *testing.T) {
	h := newEngineHarness()
	h.revErr = errors.New("platform down")
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); !errors.Is(err, h.revErr) {
		t.Fatalf("revision load error = %v", err)
	}
	h = newEngineHarness()
	req := engineRequest(domain.MigrationActionApply)
	req.RevisionID = 99
	if _, err := h.engine.Execute(context.Background(), req); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown revision = %v", err)
	}
	h = newEngineHarness()
	h.targetErr = ErrTargetBusy
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("target error = %v", err)
	}
}
