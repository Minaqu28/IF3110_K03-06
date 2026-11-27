package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func executionOf(id string, action domain.MigrationAction, target string) domain.ExecutionRequest {
	req := request(action, target)
	req.ExecutionID = id
	return req
}

func TestAvailableActions(t *testing.T) {
	apply, rollback := domain.MigrationActionApply, domain.MigrationActionRollback
	cases := []struct {
		name      string
		history   []ExecutionSummary
		execution domain.ExecutionRequest
		state     func(*SubmitState)
		want      JobActions
	}{
		{"succeeded apply can be rolled back", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusSucceeded)},
			executionOf("a", apply, ""), nil, JobActions{CanRollback: true}},
		{"partial apply can resume or be reverted", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusPartial)},
			executionOf("a", apply, ""), nil, JobActions{CanResume: true, CanRollback: true}},
		{"uncertain apply can resume", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusUnknown)},
			executionOf("a", apply, ""), nil, JobActions{CanResume: true}},
		{"failed apply offers nothing", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusFailed)},
			executionOf("a", apply, ""), nil, JobActions{}},
		{"irreversible apply cannot be rolled back", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusSucceeded)},
			executionOf("a", apply, ""), func(s *SubmitState) {
				s.Revision.Reversible, s.Revision.DownSQL = false, ""
				s.Revision.DefinitionHash = domain.ComputeDefinitionHash(s.Revision)
				s.Review.DefinitionHash = s.Revision.DefinitionHash
			}, JobActions{}},
		{"a later applied migration blocks the rollback", []ExecutionSummary{
			exec("a", 1, apply, domain.ExecutionStatusSucceeded), otherMigration("b", 6, 2, apply, domain.ExecutionStatusSucceeded)},
			executionOf("a", apply, ""), nil, JobActions{}},
		{"succeeded rollback can be forwarded", []ExecutionSummary{
			exec("a", 1, apply, domain.ExecutionStatusSucceeded), withTarget(exec("r", 2, rollback, domain.ExecutionStatusSucceeded), "a")},
			executionOf("r", rollback, "a"), nil, JobActions{CanForward: true}},
		{"partially reverted rollback can resume", []ExecutionSummary{
			exec("a", 1, apply, domain.ExecutionStatusSucceeded), withTarget(exec("r", 2, rollback, domain.ExecutionStatusPartiallyReverted), "a")},
			executionOf("r", rollback, "a"), nil, JobActions{CanResume: true}},
		{"not approved offers nothing", []ExecutionSummary{exec("a", 1, apply, domain.ExecutionStatusPartial)},
			executionOf("a", apply, ""), func(s *SubmitState) { s.Review.Status = domain.ReviewStatusRejected }, JobActions{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := baseState()
			state.Executions = c.history
			if c.state != nil {
				c.state(&state)
			}
			if got := AvailableActions(state, c.execution, true); got != c.want {
				t.Fatalf("actions = %+v, want %+v", got, c.want)
			}
			if got := AvailableActions(state, c.execution, false); got != (JobActions{}) {
				t.Fatalf("view-only actions = %+v, want none", got)
			}
		})
	}
}

func withTarget(e ExecutionSummary, target string) ExecutionSummary {
	e.TargetExecutionID = target
	return e
}

type fakeExecutions struct {
	req domain.ExecutionRequest
	err error
}

func (f fakeExecutions) GetExecution(context.Context, string) (domain.ExecutionRequest, domain.ExecutionResult, error) {
	return f.req, domain.ExecutionResult{}, f.err
}

type fakeRunStates struct {
	state SubmitState
	seen  *domain.ExecutionRequest
}

func (f fakeRunStates) LoadRunState(_ context.Context, req domain.ExecutionRequest) (domain.User, SubmitState, error) {
	*f.seen = req
	return domain.User{}, f.state, nil
}

func TestActionAdvisor(t *testing.T) {
	state := baseState()
	state.Executions = []ExecutionSummary{exec("a", 1, domain.MigrationActionApply, domain.ExecutionStatusPartial)}
	execution := executionOf("a", domain.MigrationActionApply, "")
	execution.ActorID = 999
	var seen domain.ExecutionRequest
	advisor := NewActionAdvisor(fakeAccess{level: authorization.AccessViewEdit}, fakeExecutions{req: execution}, fakeRunStates{state: state, seen: &seen})
	got, err := advisor.Actions(context.Background(), editor, domain.Job{ExecutionID: "a", DatabaseID: testDB})
	if err != nil || got != (JobActions{CanResume: true, CanRollback: true}) || seen.ActorID != editor.ID {
		t.Fatalf("actions = %+v, err = %v, state loaded for actor %d", got, err, seen.ActorID)
	}
	for name, access := range map[string]fakeAccess{"view only": {level: authorization.AccessViewOnly}, "no access": {err: admin.ErrForbidden}} {
		a := NewActionAdvisor(access, fakeExecutions{err: errors.New("must not be read")}, fakeRunStates{seen: &seen})
		if got, err := a.Actions(context.Background(), editor, domain.Job{}); err != nil || got != (JobActions{}) {
			t.Fatalf("%s: actions = %+v, err = %v", name, got, err)
		}
	}
	boom := errors.New("platform down")
	a := NewActionAdvisor(fakeAccess{level: authorization.AccessViewEdit}, fakeExecutions{err: boom}, fakeRunStates{seen: &seen})
	if _, err := a.Actions(context.Background(), editor, domain.Job{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

var errUnexpectedWorkflowMockCall = errors.New("unexpected workflow mock call")

type mockApprovalStore struct {
	createReview  func(context.Context, domain.ReviewRequest) (domain.ReviewRequest, error)
	listDecisions func(context.Context, int64) ([]domain.ApprovalDecision, error)
	saveDecision  func(context.Context, domain.ApprovalDecision) (domain.ApprovalDecision, error)
}

var _ ApprovalStore = (*mockApprovalStore)(nil)

func (m *mockApprovalStore) CreateReview(ctx context.Context, review domain.ReviewRequest) (domain.ReviewRequest, error) {
	if m.createReview == nil {
		return domain.ReviewRequest{}, errUnexpectedWorkflowMockCall
	}
	return m.createReview(ctx, review)
}

func (m *mockApprovalStore) ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error) {
	if m.listDecisions == nil {
		return nil, errUnexpectedWorkflowMockCall
	}
	return m.listDecisions(ctx, reviewID)
}

func (m *mockApprovalStore) SaveDecision(ctx context.Context, decision domain.ApprovalDecision) (domain.ApprovalDecision, error) {
	if m.saveDecision == nil {
		return domain.ApprovalDecision{}, errUnexpectedWorkflowMockCall
	}
	return m.saveDecision(ctx, decision)
}

type mockJobStore struct {
	createJob func(context.Context, domain.Job) error
	getJob    func(context.Context, string) (domain.Job, error)
}

var _ JobStore = (*mockJobStore)(nil)

func (m *mockJobStore) CreateJob(ctx context.Context, job domain.Job) error {
	if m.createJob == nil {
		return errUnexpectedWorkflowMockCall
	}
	return m.createJob(ctx, job)
}

func (m *mockJobStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	if m.getJob == nil {
		return domain.Job{}, errUnexpectedWorkflowMockCall
	}
	return m.getJob(ctx, jobID)
}

type mockJobPublisher struct {
	publish func(context.Context, domain.MigrationJobMessage) error
}

var _ JobPublisher = (*mockJobPublisher)(nil)

func (m *mockJobPublisher) Publish(ctx context.Context, message domain.MigrationJobMessage) error {
	if m.publish == nil {
		return errUnexpectedWorkflowMockCall
	}
	return m.publish(ctx, message)
}
