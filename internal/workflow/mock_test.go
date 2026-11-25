package workflow

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

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
