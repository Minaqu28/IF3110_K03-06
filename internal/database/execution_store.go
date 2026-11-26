package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type ExecutionPostgresStore struct {
	db *sql.DB
}

func NewExecutionPostgresStore(db *sql.DB) *ExecutionPostgresStore {
	return &ExecutionPostgresStore{db: db}
}

func (s *ExecutionPostgresStore) CreateExecution(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionRequest, error) {
	if !req.Action.Valid() || req.Action.RequiresTargetExecution() != (req.TargetExecutionID != nil) {
		return domain.ExecutionRequest{}, admin.ErrInvalidInput
	}
	created := req
	created.ExecutionID = ""
	created.JobID = ""
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO migration_executions (
		    database_id, migration_id, revision_id, approval_request_id,
		    action, mode, target_execution_id, requested_by
		)
		SELECT r.database_id, r.migration_id, r.id, q.id, $5, $6, $7, $8
		FROM migration_revisions r
		JOIN review_requests q ON q.revision_id = r.id
		WHERE r.database_id = $1 AND r.migration_id = $2 AND r.id = $3 AND q.id = $4
		RETURNING id, requested_at`,
		req.DatabaseID, req.MigrationID, req.RevisionID, req.ApprovalRequestID,
		string(req.Action), string(req.Mode), req.TargetExecutionID,
		sql.NullInt64{Int64: req.ActorID, Valid: req.ActorID > 0}).
		Scan(&created.ExecutionID, &created.RequestedAt)
	if err != nil {
		return domain.ExecutionRequest{}, adminError(err)
	}
	return created, nil
}

func (s *ExecutionPostgresStore) GetExecution(ctx context.Context, executionID string) (domain.ExecutionRequest, domain.ExecutionResult, error) {
	var (
		req                   domain.ExecutionRequest
		res                   domain.ExecutionResult
		action, mode, status  string
		target                sql.NullString
		actor                 sql.NullInt64
		errCode, errMsg       sql.NullString
		startedAt, finishedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, database_id, migration_id, revision_id, approval_request_id, requested_by,
		       action, mode, target_execution_id, requested_at,
		       status, applied_batches, affected_rows, error_code, error_message, started_at, finished_at
		FROM migration_executions WHERE id = $1`, executionID).
		Scan(&req.ExecutionID, &req.DatabaseID, &req.MigrationID, &req.RevisionID, &req.ApprovalRequestID, &actor,
			&action, &mode, &target, &req.RequestedAt,
			&status, &res.AppliedBatches, &res.AffectedRows, &errCode, &errMsg, &startedAt, &finishedAt)
	if err != nil {
		return domain.ExecutionRequest{}, domain.ExecutionResult{}, adminError(err)
	}
	req.ActorID = actor.Int64
	req.Action = domain.MigrationAction(action)
	req.Mode = domain.ExecutionMode(mode)
	res.ExecutionID = req.ExecutionID
	res.Status = domain.ExecutionStatus(status)
	res.ErrorCode, res.ErrorMessage = errCode.String, errMsg.String
	res.StartedAt = startedAt.Time
	if target.Valid {
		t := target.String
		req.TargetExecutionID = &t
		res.RevertsExecutionID = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		res.FinishedAt = &t
	}
	return req, res, nil
}
