package workflow

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

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
