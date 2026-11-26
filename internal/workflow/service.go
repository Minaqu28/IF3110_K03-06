package workflow

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type RevisionLister interface {
	ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error)
}

type ExecutionHistoryLister interface {
	ListExecutionHistory(ctx context.Context, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error)
}

type ReviewLister interface {
	ListReviewRequests(ctx context.Context, databaseID int64) ([]domain.ReviewRequest, error)
}

// QueryService provides authorized read-only access to migration revisions and execution history.
type QueryService struct {
	access    AccessChecker
	revisions RevisionLister
	history   ExecutionHistoryLister
}

func NewQueryService(access AccessChecker, revisions RevisionLister, history ExecutionHistoryLister) *QueryService {
	return &QueryService{access: access, revisions: revisions, history: history}
}

func (s *QueryService) ListMigrationRevisions(ctx context.Context, actor domain.User, databaseID int64) ([]domain.MigrationRevision, error) {
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, databaseID); err != nil {
		return nil, err
	}
	return s.revisions.ListRevisions(ctx, databaseID)
}

func (s *QueryService) ListExecutionHistory(ctx context.Context, actor domain.User, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, databaseID); err != nil {
		return nil, err
	}
	return s.history.ListExecutionHistory(ctx, databaseID, hideRollback)
}

// JobService combines review, submission, and job polling behind the HTTP workflow contract.
type JobService struct {
	access  AccessChecker
	reviews *ReviewService
	lister  ReviewLister
	submit  *SubmitService
	jobs    JobStore
}

func NewJobService(access AccessChecker, reviews *ReviewService, lister ReviewLister, submit *SubmitService, jobs JobStore) *JobService {
	return &JobService{access: access, reviews: reviews, lister: lister, submit: submit, jobs: jobs}
}

func (s *JobService) ListReviewRequests(ctx context.Context, actor domain.User, databaseID int64) ([]domain.ReviewRequest, error) {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, databaseID)
	if err != nil {
		return nil, err
	}
	if !level.Allows(authorization.AccessViewOnly) {
		return nil, admin.ErrForbidden
	}
	return s.lister.ListReviewRequests(ctx, databaseID)
}

func (s *JobService) DecideApproval(ctx context.Context, actor domain.User, reviewID int64, status domain.ApprovalStatus, comment string) (domain.ReviewRequest, error) {
	return s.reviews.DecideApproval(ctx, actor, reviewID, status, comment)
}

func (s *JobService) SubmitExecution(ctx context.Context, actor domain.User, req domain.ExecutionRequest) (domain.Job, error) {
	return s.submit.SubmitExecution(ctx, actor, req)
}

func (s *JobService) GetJob(ctx context.Context, actor domain.User, jobID string) (domain.Job, error) {
	if jobID == "" {
		return domain.Job{}, admin.ErrInvalidInput
	}
	job, err := s.jobs.GetJob(ctx, jobID)
	if err != nil {
		return domain.Job{}, err
	}
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, job.DatabaseID); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}
