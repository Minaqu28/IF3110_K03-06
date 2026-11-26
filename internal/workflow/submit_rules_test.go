package workflow

import (
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	testDB        = 7
	testMigration = 3
	testRevision  = 10
	testReview    = 20
)

var t0 = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

func baseState() SubmitState {
	locked := t0
	rev := domain.MigrationRevision{
		ID: testRevision, MigrationID: testMigration, DatabaseID: testDB, Revision: 2,
		UpSQL: "UPDATE t SET x = 1", DownSQL: "UPDATE t SET x = 0", Reversible: true,
		Status: domain.RevisionStatusApproved, LockedAt: &locked,
	}
	rev.DefinitionHash = domain.ComputeDefinitionHash(rev)
	return SubmitState{
		Revision: rev, LatestRevision: 2, MigrationVersion: 5,
		Review: &domain.ReviewRequest{ID: testReview, RevisionID: testRevision, DefinitionHash: rev.DefinitionHash, Status: domain.ReviewStatusApproved},
	}
}

func request(action domain.MigrationAction, target string) domain.ExecutionRequest {
	req := domain.ExecutionRequest{
		DatabaseID: testDB, MigrationID: testMigration, RevisionID: testRevision, ApprovalRequestID: testReview,
		ActorID: 1, Action: action, Mode: domain.ExecutionModeNormal,
	}
	if target != "" {
		req.TargetExecutionID = &target
	}
	return req
}

func exec(id string, minute int, action domain.MigrationAction, status domain.ExecutionStatus) ExecutionSummary {
	return ExecutionSummary{
		ID: id, MigrationID: testMigration, MigrationVersion: 5, RevisionID: testRevision, Action: action,
		Mode: domain.ExecutionModeNormal, Status: status, RequestedAt: t0.Add(time.Duration(minute) * time.Minute),
	}
}

func rollbackOf(id, target string, minute int, status domain.ExecutionStatus) ExecutionSummary {
	e := exec(id, minute, domain.MigrationActionRollback, status)
	e.TargetExecutionID = target
	return e
}

func otherMigration(id string, version, minute int, action domain.MigrationAction, status domain.ExecutionStatus) ExecutionSummary {
	e := exec(id, minute, action, status)
	e.MigrationID, e.MigrationVersion, e.RevisionID = testMigration+int64(version), version, testRevision+int64(version)
	return e
}

func TestCanSubmit(t *testing.T) {
	apply, forward, rollback := domain.MigrationActionApply, domain.MigrationActionForward, domain.MigrationActionRollback
	succeeded, failed, partial := domain.ExecutionStatusSucceeded, domain.ExecutionStatusFailed, domain.ExecutionStatusPartial
	running, unknown, partiallyReverted := domain.ExecutionStatusRunning, domain.ExecutionStatusUnknown, domain.ExecutionStatusPartiallyReverted

	cases := []struct {
		name  string
		state func(*SubmitState)
		req   func() domain.ExecutionRequest
		want  error
	}{
		{name: "apply on a fresh migration", req: func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "apply again after a failed apply",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, failed)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "apply after a successful apply", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "apply after a rollback must be forward", want: ErrActionNotAllowed,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "apply of an older revision", want: ErrNotLatestRevision,
			state: func(s *SubmitState) { s.LatestRevision = 3 },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},

		{name: "forward after a successful rollback",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(forward, "") }},
		{name: "forward without a rollback", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)} },
			req:   func() domain.ExecutionRequest { return request(forward, "") }},
		{name: "forward on a fresh migration", want: ErrActionNotAllowed,
			req: func() domain.ExecutionRequest { return request(forward, "") }},
		{name: "forward of an older revision", want: ErrNotLatestRevision,
			state: func(s *SubmitState) {
				s.LatestRevision = 3
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(forward, "") }},

		{name: "rollback of the applied execution",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)} },
			req:   func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback of a partial batch execution is let through the hold",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, partial)} },
			req:   func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback of a forward",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded), exec("f", 2, forward, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "f") }},
		{name: "rollback of an execution that is no longer in effect", want: ErrActionNotAllowed,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded), exec("f", 2, forward, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback twice", want: ErrActionNotAllowed,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback of a failed execution", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, failed)} },
			req:   func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback of an unknown execution", want: ErrActionNotAllowed,
			req: func() domain.ExecutionRequest { return request(rollback, "missing") }},
		{name: "rollback of another revision's execution", want: ErrActionNotAllowed,
			state: func(s *SubmitState) {
				e := exec("a", 0, apply, succeeded)
				e.RevisionID = testRevision - 1
				s.Executions = []ExecutionSummary{e}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback of an irreversible revision", want: ErrIrreversible,
			state: func(s *SubmitState) {
				s.Revision.Reversible, s.Revision.DownSQL, s.Revision.IrreversibleReason = false, "", "drops data"
				s.Revision.DefinitionHash = domain.ComputeDefinitionHash(s.Revision)
				s.Review.DefinitionHash = s.Revision.DefinitionHash
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback while a later migration is applied", want: ErrNotLatestApplied,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), otherMigration("b", 6, 1, apply, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "rollback after the later migration was rolled back",
			state: func(s *SubmitState) {
				later := otherMigration("rb", 6, 2, rollback, succeeded)
				later.TargetExecutionID = "b"
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), otherMigration("b", 6, 1, apply, succeeded), later}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "an earlier migration being applied does not block rollback",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{otherMigration("b", 4, 0, apply, succeeded), exec("a", 1, apply, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},

		{name: "held by a running migration elsewhere on the database", want: ErrDatabaseHeld,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{otherMigration("b", 6, 0, apply, running)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "held by a partial migration", want: ErrDatabaseHeld,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{otherMigration("b", 6, 0, apply, partial)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "held by an uncertain migration", want: ErrDatabaseHeld,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{otherMigration("b", 6, 0, apply, unknown)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "held by a partially reverted rollback", want: ErrDatabaseHeld,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, partiallyReverted)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "held by a pending submit of the same migration", want: ErrDatabaseHeld,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, domain.ExecutionStatusPending)}
			},
			req: func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "failed migrations elsewhere do not hold",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{otherMigration("b", 6, 0, apply, failed)} },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},

		{name: "resume a partial execution",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, partial)} },
			req:   func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "a"; return r }},
		{name: "resume an uncertain execution",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, unknown)} },
			req:   func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "a"; return r }},
		{name: "resume a partially reverted rollback",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, partiallyReverted)}
			},
			req: func() domain.ExecutionRequest { r := request(rollback, "a"); r.ExecutionID = "r"; return r }},
		{name: "resume keeps its revision even after a newer one exists",
			state: func(s *SubmitState) {
				s.LatestRevision = 3
				s.Executions = []ExecutionSummary{exec("a", 0, apply, partial)}
			},
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "a"; return r }},
		{name: "resume a finished execution", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)} },
			req:   func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "a"; return r }},
		{name: "resume an unknown execution id", want: ErrActionNotAllowed,
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "missing"; return r }},
		{name: "resume with a different action", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("a", 0, apply, partial)} },
			req:   func() domain.ExecutionRequest { r := request(forward, ""); r.ExecutionID = "a"; return r }},
		{name: "resume while another migration is running", want: ErrDatabaseHeld,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, partial), otherMigration("b", 6, 1, apply, running)}
			},
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.ExecutionID = "a"; return r }},

		{name: "revision still in review", want: ErrNotApproved,
			state: func(s *SubmitState) { s.Revision.Status = domain.RevisionStatusInReview },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "no review", want: ErrNotApproved,
			state: func(s *SubmitState) { s.Review = nil },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "review not approved", want: ErrNotApproved,
			state: func(s *SubmitState) { s.Review.Status = domain.ReviewStatusStale },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "request names another review", want: ErrNotApproved,
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.ApprovalRequestID++; return r }},
		{name: "review approved a different definition", want: ErrNotApproved,
			state: func(s *SubmitState) { s.Review.DefinitionHash = "0000" },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},
		{name: "stored hash does not match the SQL", want: ErrNotApproved,
			state: func(s *SubmitState) { s.Revision.UpSQL = "DROP TABLE t" },
			req:   func() domain.ExecutionRequest { return request(apply, "") }},

		{name: "unknown action", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { return request("RESUME", "") }},
		{name: "rollback without a target", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { return request(rollback, "") }},
		{name: "apply with a target", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { return request(apply, "a") }},
		{name: "batch mode on a normal revision", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.Mode = domain.ExecutionModeBatch; return r }},
		{name: "revision of another database", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.DatabaseID++; return r }},
		{name: "revision of another migration", want: admin.ErrInvalidInput,
			req: func() domain.ExecutionRequest { r := request(apply, ""); r.MigrationID++; return r }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := baseState()
			if c.state != nil {
				c.state(&state)
			}
			err := CanSubmit(state, c.req())
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
				t.Fatalf("CanSubmit = %v, want %v", err, c.want)
			}
		})
	}
}

func TestSubmitErrorsAreConflicts(t *testing.T) {
	for _, err := range []error{ErrNotApproved, ErrNotLatestRevision, ErrDatabaseHeld, ErrActionNotAllowed, ErrIrreversible, ErrNotLatestApplied} {
		if !errors.Is(err, admin.ErrConflict) {
			t.Errorf("%v does not wrap admin.ErrConflict", err)
		}
	}
}

func TestCanSubmitBatchMode(t *testing.T) {
	state := baseState()
	state.Revision.Batch = &domain.BatchConfig{Table: "public.t", CursorColumn: "id", BatchSize: 100}
	state.Revision.DefinitionHash = domain.ComputeDefinitionHash(state.Revision)
	state.Review.DefinitionHash = state.Revision.DefinitionHash
	req := request(domain.MigrationActionApply, "")
	if err := CanSubmit(state, req); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("normal mode on a batch revision: err = %v, want invalid input", err)
	}
	req.Mode = domain.ExecutionModeBatch
	if err := CanSubmit(state, req); err != nil {
		t.Fatalf("batch apply: %v", err)
	}
}

func TestCanRun(t *testing.T) {
	apply, rollback := domain.MigrationActionApply, domain.MigrationActionRollback
	pending, running, partial := domain.ExecutionStatusPending, domain.ExecutionStatusRunning, domain.ExecutionStatusPartial
	unknown, succeeded, failed := domain.ExecutionStatusUnknown, domain.ExecutionStatusSucceeded, domain.ExecutionStatusFailed
	run := func(action domain.MigrationAction, target, id string) func() domain.ExecutionRequest {
		return func() domain.ExecutionRequest { r := request(action, target); r.ExecutionID = id; return r }
	}
	cases := []struct {
		name  string
		state func(*SubmitState)
		req   func() domain.ExecutionRequest
		want  error
	}{
		{name: "first run of a queued apply",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, pending)} },
			req:   run(apply, "", "x")},
		{name: "first run after an earlier failure",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, failed), exec("x", 1, apply, pending)}
			},
			req: run(apply, "", "x")},
		{name: "first run when a newer revision appeared", want: ErrNotLatestRevision,
			state: func(s *SubmitState) {
				s.LatestRevision = 3
				s.Executions = []ExecutionSummary{exec("x", 0, apply, pending)}
			},
			req: run(apply, "", "x")},
		{name: "first run while another migration is running", want: ErrDatabaseHeld,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("x", 0, apply, pending), otherMigration("b", 6, 1, apply, running)}
			},
			req: run(apply, "", "x")},
		{name: "first run after the approval went stale", want: ErrNotApproved,
			state: func(s *SubmitState) {
				s.Review.Status = domain.ReviewStatusStale
				s.Executions = []ExecutionSummary{exec("x", 0, apply, pending)}
			},
			req: run(apply, "", "x")},
		{name: "first run of a rollback whose target is partial",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, partial), rollbackOf("x", "a", 1, pending)}
			},
			req: run(rollback, "a", "x")},
		{name: "retry of a running execution keeps its revision",
			state: func(s *SubmitState) {
				s.LatestRevision = 3
				s.Executions = []ExecutionSummary{exec("x", 0, apply, running)}
			},
			req: run(apply, "", "x")},
		{name: "resume of a partial execution",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, partial)} },
			req:   run(apply, "", "x")},
		{name: "reconcile an uncertain execution",
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, unknown)} },
			req:   run(apply, "", "x")},
		{name: "retry of a running rollback whose target is partial",
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("a", 0, apply, partial), rollbackOf("x", "a", 1, running)}
			},
			req: run(rollback, "a", "x")},
		{name: "resume while another migration is partial", want: ErrDatabaseHeld,
			state: func(s *SubmitState) {
				s.Executions = []ExecutionSummary{exec("x", 0, apply, partial), otherMigration("b", 6, 1, apply, partial)}
			},
			req: run(apply, "", "x")},
		{name: "an execution that already succeeded", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, succeeded)} },
			req:   run(apply, "", "x")},
		{name: "an execution that already failed", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, failed)} },
			req:   run(apply, "", "x")},
		{name: "an unknown execution", want: ErrActionNotAllowed,
			req: run(apply, "", "missing")},
		{name: "the message disagrees with the stored execution", want: ErrActionNotAllowed,
			state: func(s *SubmitState) { s.Executions = []ExecutionSummary{exec("x", 0, apply, pending)} },
			req:   run(domain.MigrationActionForward, "", "x")},
		{name: "no execution id", want: admin.ErrInvalidInput,
			req: run(apply, "", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := baseState()
			if c.state != nil {
				c.state(&state)
			}
			err := CanRun(state, c.req())
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
				t.Fatalf("CanRun = %v, want %v", err, c.want)
			}
		})
	}
}
