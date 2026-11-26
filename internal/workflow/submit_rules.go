package workflow

import (
	"fmt"
	"sort"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

var (
	ErrNotApproved       = fmt.Errorf("%w: revision is not approved", admin.ErrConflict)
	ErrNotLatestRevision = fmt.Errorf("%w: only the latest revision of a migration can run", admin.ErrConflict)
	ErrDatabaseHeld      = fmt.Errorf("%w: another migration on this database is unfinished", admin.ErrConflict)
	ErrActionNotAllowed  = fmt.Errorf("%w: action is not allowed in the current state", admin.ErrConflict)
	ErrIrreversible      = fmt.Errorf("%w: pembatalan tidak aman; buat migration perbaikan baru", admin.ErrConflict)
	ErrNotLatestApplied  = fmt.Errorf("%w: a later migration is still applied on this database", admin.ErrConflict)
)

type ExecutionSummary struct {
	ID                string
	MigrationID       int64
	MigrationVersion  int
	RevisionID        int64
	Action            domain.MigrationAction
	Mode              domain.ExecutionMode
	Status            domain.ExecutionStatus
	TargetExecutionID string
	RequestedAt       time.Time
}

type SubmitState struct {
	Revision         domain.MigrationRevision
	LatestRevision   int
	MigrationVersion int
	Review           *domain.ReviewRequest
	Executions       []ExecutionSummary
}

func CanSubmit(state SubmitState, req domain.ExecutionRequest) error {
	if err := checkRequestShape(state.Revision, req); err != nil {
		return err
	}
	if !approvedFor(state, req) {
		return ErrNotApproved
	}
	if req.ExecutionID != "" {
		return checkResume(state, req)
	}
	target := targetOf(req)
	if err := checkHold(state.Executions, func(e ExecutionSummary) bool {
		return e.ID == target && e.Status == domain.ExecutionStatusPartial
	}); err != nil {
		return err
	}
	history := migrationHistory(state.Executions, req.MigrationID)
	switch req.Action {
	case domain.MigrationActionApply:
		if state.Revision.Revision != state.LatestRevision {
			return ErrNotLatestRevision
		}
		for _, e := range history {
			if e.Status != domain.ExecutionStatusFailed {
				return ErrActionNotAllowed
			}
		}
	case domain.MigrationActionForward:
		if state.Revision.Revision != state.LatestRevision {
			return ErrNotLatestRevision
		}
		last, ok := lastEffective(history)
		if !ok || last.Action != domain.MigrationActionRollback || last.Status != domain.ExecutionStatusSucceeded {
			return ErrActionNotAllowed
		}
	case domain.MigrationActionRollback:
		if !state.Revision.Reversible {
			return ErrIrreversible
		}
		last, ok := lastEffective(history)
		if !ok || last.ID != target || last.RevisionID != req.RevisionID || last.Action == domain.MigrationActionRollback ||
			(last.Status != domain.ExecutionStatusSucceeded && last.Status != domain.ExecutionStatusPartial) {
			return ErrActionNotAllowed
		}
		if laterMigrationApplied(state.Executions, state.MigrationVersion) {
			return ErrNotLatestApplied
		}
	}
	return nil
}

func checkRequestShape(rev domain.MigrationRevision, req domain.ExecutionRequest) error {
	if !req.Action.Valid() || req.Action.RequiresTargetExecution() != (targetOf(req) != "") {
		return admin.ErrInvalidInput
	}
	wantMode := domain.ExecutionModeNormal
	if rev.Batch != nil {
		wantMode = domain.ExecutionModeBatch
	}
	if req.Mode != wantMode || rev.ID != req.RevisionID || rev.MigrationID != req.MigrationID || rev.DatabaseID != req.DatabaseID {
		return admin.ErrInvalidInput
	}
	return nil
}

func approvedFor(state SubmitState, req domain.ExecutionRequest) bool {
	rev, review := state.Revision, state.Review
	if rev.Status != domain.RevisionStatusApproved || rev.LockedAt == nil ||
		review == nil || review.ID != req.ApprovalRequestID || review.Status != domain.ReviewStatusApproved ||
		review.RevisionID != rev.ID || review.DefinitionHash != rev.DefinitionHash ||
		rev.DefinitionHash != domain.ComputeDefinitionHash(rev) {
		return false
	}
	if review.Action != "" && review.Action != req.Action {
		return false
	}
	if req.Action == domain.MigrationActionRollback && review.TargetExecutionID != nil && *review.TargetExecutionID != targetOf(req) {
		return false
	}
	return true
}

func checkResume(state SubmitState, req domain.ExecutionRequest) error {
	var existing *ExecutionSummary
	for i := range state.Executions {
		if state.Executions[i].ID == req.ExecutionID {
			existing = &state.Executions[i]
		}
	}
	if existing == nil || existing.RevisionID != req.RevisionID || existing.Action != req.Action ||
		existing.Mode != req.Mode || existing.TargetExecutionID != targetOf(req) {
		return ErrActionNotAllowed
	}
	switch existing.Status {
	case domain.ExecutionStatusPartial, domain.ExecutionStatusUnknown, domain.ExecutionStatusPartiallyReverted:
	default:
		return ErrActionNotAllowed
	}
	return checkHold(state.Executions, func(e ExecutionSummary) bool { return e.ID == existing.ID })
}

func checkHold(executions []ExecutionSummary, exempt func(ExecutionSummary) bool) error {
	for _, e := range executions {
		if isOpen(e.Status) && !exempt(e) {
			return ErrDatabaseHeld
		}
	}
	return nil
}

func isOpen(s domain.ExecutionStatus) bool {
	switch s {
	case domain.ExecutionStatusPending, domain.ExecutionStatusRunning, domain.ExecutionStatusPartial,
		domain.ExecutionStatusUnknown, domain.ExecutionStatusPartiallyReverted:
		return true
	}
	return false
}

func migrationHistory(executions []ExecutionSummary, migrationID int64) []ExecutionSummary {
	var history []ExecutionSummary
	for _, e := range executions {
		if e.MigrationID == migrationID {
			history = append(history, e)
		}
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].RequestedAt.Before(history[j].RequestedAt) })
	return history
}

func lastEffective(history []ExecutionSummary) (ExecutionSummary, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Status != domain.ExecutionStatusFailed && history[i].Status != domain.ExecutionStatusPending {
			return history[i], true
		}
	}
	return ExecutionSummary{}, false
}

func laterMigrationApplied(executions []ExecutionSummary, version int) bool {
	later := map[int64][]ExecutionSummary{}
	for _, e := range executions {
		if e.MigrationVersion > version {
			later[e.MigrationID] = append(later[e.MigrationID], e)
		}
	}
	for migrationID := range later {
		last, ok := lastEffective(migrationHistory(executions, migrationID))
		if ok && !(last.Action == domain.MigrationActionRollback && last.Status == domain.ExecutionStatusSucceeded) {
			return true
		}
	}
	return false
}

func targetOf(req domain.ExecutionRequest) string {
	if req.TargetExecutionID == nil {
		return ""
	}
	return *req.TargetExecutionID
}

func CanRun(state SubmitState, req domain.ExecutionRequest) error {
	if req.ExecutionID == "" {
		return admin.ErrInvalidInput
	}
	if err := checkRequestShape(state.Revision, req); err != nil {
		return err
	}
	if !approvedFor(state, req) {
		return ErrNotApproved
	}
	var self *ExecutionSummary
	var others []ExecutionSummary
	for i, e := range state.Executions {
		if e.ID == req.ExecutionID {
			self = &state.Executions[i]
		} else {
			others = append(others, e)
		}
	}
	if self == nil || self.RevisionID != req.RevisionID || self.Action != req.Action ||
		self.Mode != req.Mode || self.TargetExecutionID != targetOf(req) {
		return ErrActionNotAllowed
	}
	switch self.Status {
	case domain.ExecutionStatusPending:
		fresh := state
		fresh.Executions = others
		firstRun := req
		firstRun.ExecutionID = ""
		return CanSubmit(fresh, firstRun)
	case domain.ExecutionStatusRunning, domain.ExecutionStatusPartial, domain.ExecutionStatusUnknown, domain.ExecutionStatusPartiallyReverted:
		target := targetOf(req)
		return checkHold(state.Executions, func(e ExecutionSummary) bool {
			return e.ID == self.ID || (e.ID == target && e.Status == domain.ExecutionStatusPartial)
		})
	}
	return ErrActionNotAllowed
}
