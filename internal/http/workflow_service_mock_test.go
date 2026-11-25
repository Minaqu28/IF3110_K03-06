package httpserver

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

var errUnexpectedWorkflowMockCall = errors.New("unexpected workflow service mock call")

type mockWorkflowJobService struct {
	listReviews     func(context.Context, domain.User, int64) ([]domain.ReviewRequest, error)
	decideApproval  func(context.Context, domain.User, int64, domain.ApprovalStatus, string) (domain.ReviewRequest, error)
	submitExecution func(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error)
	getJob          func(context.Context, domain.User, string) (domain.Job, error)
}

var _ WorkflowJobService = (*mockWorkflowJobService)(nil)

func (m *mockWorkflowJobService) ListReviewRequests(ctx context.Context, actor domain.User, databaseID int64) ([]domain.ReviewRequest, error) {
	if m.listReviews == nil {
		return nil, errUnexpectedWorkflowMockCall
	}
	return m.listReviews(ctx, actor, databaseID)
}

func (m *mockWorkflowJobService) DecideApproval(ctx context.Context, actor domain.User, reviewID int64, status domain.ApprovalStatus, comment string) (domain.ReviewRequest, error) {
	if m.decideApproval == nil {
		return domain.ReviewRequest{}, errUnexpectedWorkflowMockCall
	}
	return m.decideApproval(ctx, actor, reviewID, status, comment)
}

func (m *mockWorkflowJobService) SubmitExecution(ctx context.Context, actor domain.User, request domain.ExecutionRequest) (domain.Job, error) {
	if m.submitExecution == nil {
		return domain.Job{}, errUnexpectedWorkflowMockCall
	}
	return m.submitExecution(ctx, actor, request)
}

func (m *mockWorkflowJobService) GetJob(ctx context.Context, actor domain.User, jobID string) (domain.Job, error) {
	if m.getJob == nil {
		return domain.Job{}, errUnexpectedWorkflowMockCall
	}
	return m.getJob(ctx, actor, jobID)
}
