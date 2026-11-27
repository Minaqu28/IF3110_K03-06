package workflow

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeRevisions struct {
	rev   domain.MigrationRevision
	err   error
	calls int
}

func (f *fakeRevisions) GetRevision(context.Context, int64, int64) (domain.MigrationRevision, error) {
	f.calls++
	return f.rev, f.err
}

type fakeScopes struct {
	scope BatchScope
	err   error
	calls int
}

func (f *fakeScopes) BatchScope(context.Context, int64, domain.BatchConfig) (BatchScope, error) {
	f.calls++
	return f.scope, f.err
}

type fakeExecSubmitter struct {
	state       SubmitState
	created     bool
	err         error
	submissions []Submission
	checkErr    error
}

func (f *fakeExecSubmitter) SubmitExecution(_ context.Context, sub Submission, check func(SubmitState) error) (SubmitResult, error) {
	f.submissions = append(f.submissions, sub)
	if f.err != nil {
		return SubmitResult{}, f.err
	}
	if f.checkErr = check(f.state); f.checkErr != nil {
		return SubmitResult{}, f.checkErr
	}
	job := domain.Job{ID: sub.JobID, ExecutionID: "exec-1", DatabaseID: sub.Request.DatabaseID, Status: domain.JobStatusQueued}
	return SubmitResult{Job: job, Message: domain.MigrationJobMessage{JobID: sub.JobID}, Created: f.created}, nil
}

type fakePublisher struct {
	err      error
	messages []domain.MigrationJobMessage
}

func (f *fakePublisher) Publish(_ context.Context, m domain.MigrationJobMessage) error {
	f.messages = append(f.messages, m)
	return f.err
}

type fakeMarker struct {
	err  error
	jobs []string
}

func (f *fakeMarker) MarkPublished(_ context.Context, jobID string) error {
	f.jobs = append(f.jobs, jobID)
	return f.err
}

type submitHarness struct {
	revisions *fakeRevisions
	scopes    *fakeScopes
	submitter *fakeExecSubmitter
	publisher *fakePublisher
	marker    *fakeMarker
	service   *SubmitService
}

func newSubmitHarness(level authorization.AccessLevel) *submitHarness {
	h := &submitHarness{
		revisions: &fakeRevisions{}, scopes: &fakeScopes{},
		submitter: &fakeExecSubmitter{state: baseState(), created: true},
		publisher: &fakePublisher{}, marker: &fakeMarker{},
	}
	h.service = NewSubmitService(fakeAccess{level: level}, h.revisions, h.scopes, h.submitter, h.publisher, h.marker)
	h.service.newID = func() string { return "generated-id" }
	return h
}

var editor = domain.User{ID: 1, IsActive: true}

func TestSubmitExecutionQueuesAndPublishes(t *testing.T) {
	h := newSubmitHarness(authorization.AccessViewEdit)
	req := request(domain.MigrationActionApply, "")
	req.ActorID = 999
	job, err := h.service.SubmitExecution(context.Background(), editor, req)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "generated-id" || job.Status != domain.JobStatusQueued {
		t.Fatalf("job = %+v", job)
	}
	sub := h.submitter.submissions[0]
	if sub.JobID != "generated-id" || sub.Request.ActorID != editor.ID || sub.Request.JobID != "" || sub.Scope != nil {
		t.Fatalf("submission = %+v", sub)
	}
	if len(h.publisher.messages) != 1 || h.publisher.messages[0].JobID != "generated-id" {
		t.Fatalf("published = %+v", h.publisher.messages)
	}
	if len(h.marker.jobs) != 1 || h.marker.jobs[0] != "generated-id" {
		t.Fatalf("marked = %+v", h.marker.jobs)
	}
	if h.scopes.calls != 0 || h.revisions.calls != 0 {
		t.Fatal("normal mode should not read a batch scope")
	}
}

func TestSubmitExecutionUsesTheClientJobID(t *testing.T) {
	h := newSubmitHarness(authorization.AccessViewEdit)
	req := request(domain.MigrationActionApply, "")
	req.JobID = "client-chosen"
	job, err := h.service.SubmitExecution(context.Background(), editor, req)
	if err != nil || job.ID != "client-chosen" || h.submitter.submissions[0].JobID != "client-chosen" {
		t.Fatalf("job = %+v, err = %v", job, err)
	}
}

func TestSubmitExecutionRunsCanSubmitInsideTheStore(t *testing.T) {
	h := newSubmitHarness(authorization.AccessViewEdit)
	h.submitter.state.Executions = []ExecutionSummary{otherMigration("b", 6, 0, domain.MigrationActionApply, domain.ExecutionStatusRunning)}
	if _, err := h.service.SubmitExecution(context.Background(), editor, request(domain.MigrationActionApply, "")); !errors.Is(err, ErrDatabaseHeld) {
		t.Fatalf("err = %v, want database held", err)
	}
	if len(h.publisher.messages) != 0 {
		t.Fatal("a refused submit was published")
	}
}

func TestSubmitExecutionRefusals(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name  string
		level authorization.AccessLevel
		setup func(*submitHarness)
		want  error
	}{
		{"view-only user", authorization.AccessViewOnly, nil, admin.ErrForbidden},
		{"no access", authorization.AccessNone, nil, admin.ErrForbidden},
		{"store error", authorization.AccessViewEdit, func(h *submitHarness) { h.submitter.err = boom }, boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSubmitHarness(c.level)
			if c.setup != nil {
				c.setup(h)
			}
			if _, err := h.service.SubmitExecution(context.Background(), editor, request(domain.MigrationActionApply, "")); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if len(h.publisher.messages) != 0 {
				t.Fatal("published after a refusal")
			}
			if c.level != authorization.AccessViewEdit && len(h.submitter.submissions) != 0 {
				t.Fatal("submitted without edit access")
			}
		})
	}
}

func TestSubmitExecutionReplayIsNotPublishedAgain(t *testing.T) {
	h := newSubmitHarness(authorization.AccessViewEdit)
	h.submitter.created = false
	if _, err := h.service.SubmitExecution(context.Background(), editor, request(domain.MigrationActionApply, "")); err != nil {
		t.Fatal(err)
	}
	if len(h.publisher.messages) != 0 || len(h.marker.jobs) != 0 {
		t.Fatal("a replayed job was published again")
	}
}

func TestSubmitExecutionPublishFailuresStillReturnTheJob(t *testing.T) {
	h := newSubmitHarness(authorization.AccessViewEdit)
	h.publisher.err = errors.New("broker down")
	job, err := h.service.SubmitExecution(context.Background(), editor, request(domain.MigrationActionApply, ""))
	if err != nil || job.ID == "" {
		t.Fatalf("job = %+v, err = %v", job, err)
	}
	if len(h.marker.jobs) != 0 {
		t.Fatal("job marked published although publish failed")
	}

	h = newSubmitHarness(authorization.AccessViewEdit)
	h.marker.err = errors.New("db down")
	if job, err := h.service.SubmitExecution(context.Background(), editor, request(domain.MigrationActionApply, "")); err != nil || job.ID == "" {
		t.Fatalf("mark failure: job = %+v, err = %v", job, err)
	}
}

func TestSubmitExecutionBatchScope(t *testing.T) {
	batchRev := baseState().Revision
	batchRev.Batch = &domain.BatchConfig{Table: "public.t", CursorColumn: "id", BatchSize: 100}
	batchReq := func(action domain.MigrationAction, target, execution string) domain.ExecutionRequest {
		r := request(action, target)
		r.Mode, r.ExecutionID = domain.ExecutionModeBatch, execution
		return r
	}

	t.Run("a new batch apply fixes the scope", func(t *testing.T) {
		h := newSubmitHarness(authorization.AccessViewEdit)
		h.revisions.rev, h.scopes.scope = batchRev, BatchScope{Min: 4, Max: 20}
		h.submitter.err = errors.New("stop")
		_, _ = h.service.SubmitExecution(context.Background(), editor, batchReq(domain.MigrationActionApply, "", ""))
		sub := h.submitter.submissions[0]
		if sub.Scope == nil || *sub.Scope != (BatchScope{Min: 4, Max: 20}) {
			t.Fatalf("scope = %+v", sub.Scope)
		}
	})
	for name, req := range map[string]domain.ExecutionRequest{
		"rollback":         batchReq(domain.MigrationActionRollback, "a", ""),
		"resume":           batchReq(domain.MigrationActionApply, "", "a"),
		"normal mode only": request(domain.MigrationActionApply, ""),
	} {
		t.Run(name+" reads no scope", func(t *testing.T) {
			h := newSubmitHarness(authorization.AccessViewEdit)
			h.revisions.rev = batchRev
			h.submitter.err = errors.New("stop")
			_, _ = h.service.SubmitExecution(context.Background(), editor, req)
			if h.scopes.calls != 0 || h.submitter.submissions[0].Scope != nil {
				t.Fatalf("scope read %d times, submission scope %+v", h.scopes.calls, h.submitter.submissions[0].Scope)
			}
		})
	}
	t.Run("scope errors stop the submit", func(t *testing.T) {
		h := newSubmitHarness(authorization.AccessViewEdit)
		h.revisions.rev, h.scopes.err = batchRev, admin.ErrInvalidInput
		if _, err := h.service.SubmitExecution(context.Background(), editor, batchReq(domain.MigrationActionApply, "", "")); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatalf("err = %v, want invalid input", err)
		}
		if len(h.submitter.submissions) != 0 {
			t.Fatal("submitted without a scope")
		}
	})
	t.Run("batch mode on a normal revision", func(t *testing.T) {
		h := newSubmitHarness(authorization.AccessViewEdit)
		h.revisions.rev = baseState().Revision
		if _, err := h.service.SubmitExecution(context.Background(), editor, batchReq(domain.MigrationActionApply, "", "")); !errors.Is(err, admin.ErrInvalidInput) {
			t.Fatalf("err = %v, want invalid input", err)
		}
	})
}

func TestNewUUID(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := newUUID()
		if !pattern.MatchString(id) || seen[id] {
			t.Fatalf("bad or repeated uuid %q", id)
		}
		seen[id] = true
	}
}

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
		{name: "apply review cannot authorize rollback", want: ErrNotApproved,
			state: func(s *SubmitState) {
				s.Review.Action = apply
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "apply review cannot authorize forward", want: ErrNotApproved,
			state: func(s *SubmitState) {
				s.Review.Action = apply
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(forward, "") }},
		{name: "rollback review for different target execution rejected", want: ErrNotApproved,
			state: func(s *SubmitState) {
				other := "a"
				s.Review.Action = rollback
				s.Review.TargetExecutionID = &other
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded), exec("f", 2, forward, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "f") }},
		{name: "rollback review matching target execution succeeds",
			state: func(s *SubmitState) {
				target := "a"
				s.Review.Action = rollback
				s.Review.TargetExecutionID = &target
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(rollback, "a") }},
		{name: "forward review authorizes forward",
			state: func(s *SubmitState) {
				s.Review.Action = forward
				s.Executions = []ExecutionSummary{exec("a", 0, apply, succeeded), rollbackOf("r", "a", 1, succeeded)}
			},
			req: func() domain.ExecutionRequest { return request(forward, "") }},

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

type recordingAccess struct {
	level authorization.AccessLevel
	err   error
	seen  []domain.User
}

func (r *recordingAccess) ConnectionWithAccess(_ context.Context, actor domain.User, _ int64) (domain.Connection, authorization.AccessLevel, error) {
	r.seen = append(r.seen, actor)
	return domain.Connection{}, r.level, r.err
}

type fakeRunState struct {
	actor domain.User
	state SubmitState
	err   error
	reqs  []domain.ExecutionRequest
}

func (f *fakeRunState) LoadRunState(_ context.Context, req domain.ExecutionRequest) (domain.User, SubmitState, error) {
	f.reqs = append(f.reqs, req)
	return f.actor, f.state, f.err
}

func TestAuthorizeExecution(t *testing.T) {
	queued := func() *fakeRunState {
		s := baseState()
		s.Executions = []ExecutionSummary{exec("x", 0, domain.MigrationActionApply, domain.ExecutionStatusPending)}
		return &fakeRunState{actor: domain.User{ID: 1, IsActive: true, Username: "fresh"}, state: s}
	}
	req := request(domain.MigrationActionApply, "")
	req.ExecutionID = "x"

	t.Run("authorized with the freshly loaded user", func(t *testing.T) {
		states, access := queued(), &recordingAccess{level: authorization.AccessViewEdit}
		if err := NewExecutionAuthorizer(access, states).AuthorizeExecution(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(access.seen) != 1 || access.seen[0].Username != "fresh" || len(states.reqs) != 1 {
			t.Fatalf("access saw %+v, state loads %d", access.seen, len(states.reqs))
		}
	})

	boom := errors.New("boom")
	cases := []struct {
		name   string
		req    func() domain.ExecutionRequest
		states func() *fakeRunState
		access *recordingAccess
		want   error
	}{
		{"user or execution gone", nil, func() *fakeRunState { s := queued(); s.err = admin.ErrNotFound; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrForbidden},
		{"state load fails", nil, func() *fakeRunState { s := queued(); s.err = boom; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, boom},
		{"edit access revoked", nil, queued, &recordingAccess{level: authorization.AccessViewOnly}, admin.ErrForbidden},
		{"user deactivated", nil, queued, &recordingAccess{err: admin.ErrForbidden}, admin.ErrForbidden},
		{"rules refuse", nil, func() *fakeRunState { s := queued(); s.state.LatestRevision = 3; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, ErrNotLatestRevision},
		{"no execution id", func() domain.ExecutionRequest { r := req; r.ExecutionID = ""; return r }, queued,
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrInvalidInput},
		{"no actor", func() domain.ExecutionRequest { r := req; r.ActorID = 0; return r }, queued,
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrInvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req
			if c.req != nil {
				r = c.req()
			}
			if err := NewExecutionAuthorizer(c.access, c.states()).AuthorizeExecution(context.Background(), r); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
