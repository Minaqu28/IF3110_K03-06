package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"

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

type ActionReviewSubmitter interface {
	SubmitActionForReview(ctx context.Context, databaseID, revisionID, requestedBy int64, action domain.MigrationAction, targetExecutionID *string, approvers []RequiredApprover) (domain.ReviewRequest, error)
}

type ReviewService struct {
	access    AccessChecker
	grants    GrantReader
	reviews   ReviewSubmitter
	decisions ReviewDecider
}

func NewReviewService(access AccessChecker, grants GrantReader, reviews ReviewSubmitter, decisions ReviewDecider) *ReviewService {
	return &ReviewService{access: access, grants: grants, reviews: reviews, decisions: decisions}
}

func (s *ReviewService) SubmitForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64) (domain.ReviewRequest, error) {
	return s.SubmitActionForReview(ctx, actor, databaseID, revisionID, domain.MigrationActionApply, nil)
}

func (s *ReviewService) SubmitActionForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64, action domain.MigrationAction, targetExecutionID *string) (domain.ReviewRequest, error) {
	if action == "" {
		action = domain.MigrationActionApply
	}
	if !action.Valid() {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if action == domain.MigrationActionRollback && (targetExecutionID == nil || *targetExecutionID == "") {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if action != domain.MigrationActionRollback && targetExecutionID != nil {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
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
	if actionSubmitter, ok := s.reviews.(ActionReviewSubmitter); ok {
		return actionSubmitter.SubmitActionForReview(ctx, databaseID, revisionID, actor.ID, action, targetExecutionID, approvers)
	}
	if action != domain.MigrationActionApply {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
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

type ReviewDecider interface {
	DecideReview(ctx context.Context, reviewID, leaderID int64, status domain.ApprovalStatus, comment string, resolve func([]domain.ApprovalDecision) domain.ReviewStatus) (domain.ReviewRequest, error)
}

func (s *ReviewService) DecideApproval(ctx context.Context, actor domain.User, reviewID int64, status domain.ApprovalStatus, comment string) (domain.ReviewRequest, error) {
	if status != domain.ApprovalStatusApproved && status != domain.ApprovalStatusRejected {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if actor.ID <= 0 || !actor.IsActive {
		return domain.ReviewRequest{}, admin.ErrForbidden
	}
	return s.decisions.DecideReview(ctx, reviewID, actor.ID, status, comment, ResolveReview)
}

var (
	ErrNoApprovers        = errors.New("no group leader can approve migrations on this database")
	ErrGroupMissingLeader = fmt.Errorf("%w: every group with access to this database must have a leader assigned", admin.ErrConflict)
)

type GroupGrant struct {
	GroupID    int64
	DatabaseID int64
	LeaderID   int64
	Level      authorization.AccessLevel
}

type RequiredApprover struct {
	GroupID  int64
	LeaderID int64
}

func RequiredGroups(databaseID int64, grants []GroupGrant) ([]RequiredApprover, error) {
	seen := map[int64]bool{}
	var required []RequiredApprover
	for _, g := range grants {
		if g.DatabaseID != databaseID || seen[g.GroupID] || !g.Level.Allows(authorization.AccessViewOnly) {
			continue
		}
		if g.LeaderID <= 0 {
			return nil, ErrGroupMissingLeader
		}
		seen[g.GroupID] = true
		required = append(required, RequiredApprover{GroupID: g.GroupID, LeaderID: g.LeaderID})
	}
	if len(required) == 0 {
		return nil, ErrNoApprovers
	}
	sort.Slice(required, func(i, j int) bool { return required[i].GroupID < required[j].GroupID })
	return required, nil
}

func ResolveReview(decisions []domain.ApprovalDecision) domain.ReviewStatus {
	if len(decisions) == 0 {
		return domain.ReviewStatusPending
	}
	approved := 0
	for _, d := range decisions {
		switch d.Status {
		case domain.ApprovalStatusRejected:
			return domain.ReviewStatusRejected
		case domain.ApprovalStatusApproved:
			approved++
		}
	}
	if approved == len(decisions) {
		return domain.ReviewStatusApproved
	}
	return domain.ReviewStatusPending
}
