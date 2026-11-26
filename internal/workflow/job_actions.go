package workflow

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type JobActions struct {
	CanResume   bool `json:"can_resume"`
	CanRollback bool `json:"can_rollback"`
	CanForward  bool `json:"can_forward"`
}

func AvailableActions(state SubmitState, execution domain.ExecutionRequest, canEdit bool) JobActions {
	if !canEdit || execution.ExecutionID == "" {
		return JobActions{}
	}
	actions := JobActions{CanResume: CanSubmit(state, execution) == nil}
	fresh := execution
	fresh.ExecutionID, fresh.JobID = "", ""
	if execution.Action == domain.MigrationActionRollback {
		forward := fresh
		forward.Action, forward.TargetExecutionID = domain.MigrationActionForward, nil
		actions.CanForward = CanSubmit(stateForAction(state, domain.MigrationActionForward, nil), forward) == nil
		return actions
	}
	rollback := fresh
	target := execution.ExecutionID
	rollback.Action, rollback.TargetExecutionID = domain.MigrationActionRollback, &target
	actions.CanRollback = CanSubmit(stateForAction(state, domain.MigrationActionRollback, &target), rollback) == nil
	return actions
}

func stateForAction(state SubmitState, action domain.MigrationAction, target *string) SubmitState {
	if state.Review == nil {
		return state
	}
	revCopy := *state.Review
	revCopy.Action = action
	revCopy.TargetExecutionID = target
	state.Review = &revCopy
	return state
}

type ExecutionReader interface {
	GetExecution(ctx context.Context, executionID string) (domain.ExecutionRequest, domain.ExecutionResult, error)
}

type ActionAdvisor struct {
	access     AccessChecker
	executions ExecutionReader
	states     RunStateReader
}

func NewActionAdvisor(access AccessChecker, executions ExecutionReader, states RunStateReader) *ActionAdvisor {
	return &ActionAdvisor{access: access, executions: executions, states: states}
}

func (a *ActionAdvisor) Actions(ctx context.Context, actor domain.User, job domain.Job) (JobActions, error) {
	_, level, err := a.access.ConnectionWithAccess(ctx, actor, job.DatabaseID)
	if err != nil || !level.Allows(authorization.AccessViewEdit) {
		return JobActions{}, nil
	}
	execution, _, err := a.executions.GetExecution(ctx, job.ExecutionID)
	if err != nil {
		return JobActions{}, err
	}
	execution.ActorID = actor.ID
	_, state, err := a.states.LoadRunState(ctx, execution)
	if err != nil {
		return JobActions{}, err
	}
	return AvailableActions(state, execution, true), nil
}
