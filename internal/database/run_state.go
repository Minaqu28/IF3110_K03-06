package database

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var _ workflow.RunStateReader = (*SubmitPostgresStore)(nil)

func (s *SubmitPostgresStore) LoadRunState(ctx context.Context, req domain.ExecutionRequest) (domain.User, workflow.SubmitState, error) {
	var actor domain.User
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, username, is_platform_admin, is_active FROM users WHERE id = $1`, req.ActorID).
		Scan(&actor.ID, &actor.Username, &actor.IsPlatformAdmin, &actor.IsActive); err != nil {
		return domain.User{}, workflow.SubmitState{}, adminError(err)
	}
	state, err := loadSubmitState(ctx, s.db, req)
	if err != nil {
		return domain.User{}, workflow.SubmitState{}, err
	}
	return actor, state, nil
}
