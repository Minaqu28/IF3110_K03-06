package httpserver

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type MigrationQueryService interface {
	ListMigrationRevisions(context.Context, domain.User, int64) ([]domain.MigrationRevision, error)
	ListExecutionHistory(context.Context, domain.User, int64, bool) ([]domain.ExecutionAttempt, error)
}

type WorkflowJobService interface {
	ListReviewRequests(context.Context, domain.User, int64) ([]domain.ReviewRequest, error)
	DecideApproval(context.Context, domain.User, int64, domain.ApprovalStatus, string) (domain.ReviewRequest, error)
	SubmitExecution(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error)
	GetJob(context.Context, domain.User, string) (domain.Job, error)
}

type RevisionWorkflowService interface {
	CreateRevision(context.Context, domain.User, domain.MigrationRevision) (domain.MigrationRevision, error)
	SubmitForReview(context.Context, domain.User, int64, int64) (domain.ReviewRequest, error)
}

type ActionReviewWorkflowService interface {
	SubmitActionForReview(context.Context, domain.User, int64, int64, domain.MigrationAction, *string) (domain.ReviewRequest, error)
}

type ReviewDecisionService interface {
	ListReviewDecisions(context.Context, domain.User, int64, int64) ([]domain.ApprovalDecision, error)
}

type ExecutionDetailService interface {
	GetExecution(context.Context, domain.User, string) (domain.ExecutionRequest, domain.ExecutionResult, error)
}
