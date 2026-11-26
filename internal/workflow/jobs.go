package workflow

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type JobGetter interface {
	GetJob(ctx context.Context, jobID string) (domain.Job, error)
}

type JobQueryService struct {
	access AccessChecker
	jobs   JobGetter
}

func NewJobQueryService(access AccessChecker, jobs JobGetter) *JobQueryService {
	return &JobQueryService{access: access, jobs: jobs}
}

func (s *JobQueryService) GetJob(ctx context.Context, actor domain.User, jobID string) (domain.Job, error) {
	job, err := s.jobs.GetJob(ctx, jobID)
	if err != nil {
		return domain.Job{}, err
	}
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, job.DatabaseID)
	if errors.Is(err, admin.ErrForbidden) || (err == nil && !level.Allows(authorization.AccessViewOnly)) {
		return domain.Job{}, admin.ErrNotFound
	}
	if err != nil {
		return domain.Job{}, err
	}
	return job, nil
}
