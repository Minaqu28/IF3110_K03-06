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
