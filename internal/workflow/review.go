package workflow

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type AccessChecker interface {
	ConnectionWithAccess(ctx context.Context, actor domain.User, databaseID int64) (domain.Connection, authorization.AccessLevel, error)
}

var _ AccessChecker = (*admin.AdminService)(nil)

type GrantReader interface {
	ListGroupGrants(ctx context.Context, databaseID int64) ([]GroupGrant, error)
}

type ReviewSubmitter interface {
	SubmitForReview(ctx context.Context, databaseID, revisionID, requestedBy int64, approvers []RequiredApprover) (domain.ReviewRequest, error)
}

type ReviewService struct {
	access  AccessChecker
	grants  GrantReader
	reviews ReviewSubmitter
}

func NewReviewService(access AccessChecker, grants GrantReader, reviews ReviewSubmitter) *ReviewService {
	return &ReviewService{access: access, grants: grants, reviews: reviews}
}

func (s *ReviewService) SubmitForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64) (domain.ReviewRequest, error) {
	if err := s.requireEdit(ctx, actor, databaseID); err != nil {
		return domain.ReviewRequest{}, err
	}
	grants, err := s.grants.ListGroupGrants(ctx, databaseID)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	approvers, err := RequiredGroups(databaseID, grants)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	return s.reviews.SubmitForReview(ctx, databaseID, revisionID, actor.ID, approvers)
}

func (s *ReviewService) requireEdit(ctx context.Context, actor domain.User, databaseID int64) error {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, databaseID)
	if err != nil {
		return err
	}
	if !level.Allows(authorization.AccessViewEdit) {
		return admin.ErrForbidden
	}
	return nil
}
