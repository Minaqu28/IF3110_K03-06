package workflow

import (
	"errors"
	"sort"

	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
)

var ErrNoApprovers = errors.New("no group leader can approve migrations on this database")

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
		if g.DatabaseID != databaseID || g.LeaderID <= 0 || seen[g.GroupID] || !g.Level.Allows(authorization.AccessViewEdit) {
			continue
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
