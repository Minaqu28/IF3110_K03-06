package workflow

import (
	"context"
	"errors"
	"regexp"
	"testing"

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
