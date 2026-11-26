package workflow

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

type RunStateReader interface {
	LoadRunState(ctx context.Context, req domain.ExecutionRequest) (domain.User, SubmitState, error)
}

type ExecutionAuthorizer struct {
	access AccessChecker
	states RunStateReader
}

var _ worker.ExecutionAuthorizer = (*ExecutionAuthorizer)(nil)

func NewExecutionAuthorizer(access AccessChecker, states RunStateReader) *ExecutionAuthorizer {
	return &ExecutionAuthorizer{access: access, states: states}
}

func (a *ExecutionAuthorizer) AuthorizeExecution(ctx context.Context, req domain.ExecutionRequest) error {
	if req.ExecutionID == "" || req.ActorID <= 0 {
		return admin.ErrInvalidInput
	}
	actor, state, err := a.states.LoadRunState(ctx, req)
	if errors.Is(err, admin.ErrNotFound) {
		return admin.ErrForbidden
	}
	if err != nil {
		return err
	}
	_, level, err := a.access.ConnectionWithAccess(ctx, actor, req.DatabaseID)
	if err != nil {
		return err
	}
	if !level.Allows(authorization.AccessViewEdit) {
		return admin.ErrForbidden
	}
	return CanRun(state, req)
}
