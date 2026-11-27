package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type ExecutionPostgresStore struct {
	db *sql.DB
}

func NewExecutionPostgresStore(db *sql.DB) *ExecutionPostgresStore {
	return &ExecutionPostgresStore{db: db}
}

func (s *ExecutionPostgresStore) CreateExecution(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionRequest, error) {
	return createExecution(ctx, s.db, req, nil)
}

func createExecution(ctx context.Context, q dbtx, req domain.ExecutionRequest, scope *workflow.BatchScope) (domain.ExecutionRequest, error) {
	if !req.Action.Valid() || req.Action.RequiresTargetExecution() != (req.TargetExecutionID != nil) {
		return domain.ExecutionRequest{}, admin.ErrInvalidInput
	}
	var scopeMin, scopeMax sql.NullInt64
	if scope != nil {
		scopeMin = sql.NullInt64{Int64: scope.Min, Valid: true}
		scopeMax = sql.NullInt64{Int64: scope.Max, Valid: true}
	}
	created := req
	created.ExecutionID = ""
	created.JobID = ""
	err := q.QueryRowContext(ctx, `
		INSERT INTO migration_executions (
		    database_id, migration_id, revision_id, approval_request_id,
		    action, mode, target_execution_id, requested_by, scope_min, scope_max
		)
		SELECT r.database_id, r.migration_id, r.id, q.id, $5, $6, $7, $8, $9, $10
		FROM migration_revisions r
		JOIN review_requests q ON q.revision_id = r.id
		WHERE r.database_id = $1 AND r.migration_id = $2 AND r.id = $3 AND q.id = $4
		RETURNING id, requested_at`,
		req.DatabaseID, req.MigrationID, req.RevisionID, req.ApprovalRequestID,
		string(req.Action), string(req.Mode), req.TargetExecutionID,
		sql.NullInt64{Int64: req.ActorID, Valid: req.ActorID > 0}, scopeMin, scopeMax).
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

func (s *ExecutionPostgresStore) FinishExecution(ctx context.Context, res domain.ExecutionResult) error {
	if res.ExecutionID == "" {
		return admin.ErrInvalidInput
	}
	var finishedAt sql.NullTime
	if res.FinishedAt != nil {
		finishedAt = sql.NullTime{Time: *res.FinishedAt, Valid: true}
	}
	return rowsAffected(s.db.ExecContext(ctx, `
		UPDATE migration_executions
		SET status         = $2,
		    applied_batches = GREATEST(applied_batches, $3),
		    affected_rows   = GREATEST(affected_rows, $4),
		    error_code      = $5,
		    error_message   = $6,
		    started_at      = COALESCE(started_at, $7),
		    finished_at     = COALESCE($8, finished_at)
		WHERE id = $1`,
		res.ExecutionID, string(res.Status), res.AppliedBatches, res.AffectedRows,
		nullString(res.ErrorCode), nullString(res.ErrorMessage), res.StartedAt, finishedAt))
}

type HistoryPostgresStore struct {
	db *sql.DB
}

var _ migrationengine.HistoryStore = (*HistoryPostgresStore)(nil)

func NewHistoryPostgresStore(db *sql.DB) *HistoryPostgresStore {
	return &HistoryPostgresStore{db: db}
}

func (s *HistoryPostgresStore) AppendAttempt(ctx context.Context, attempt domain.ExecutionAttempt) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_attempts (
		    execution_id, database_id, attempt_number, status, sql_snapshot,
		    error_code, error_message, started_at, finished_at
		)
		SELECT e.id, e.database_id,
		       COALESCE((SELECT max(a.attempt_number) FROM execution_attempts a WHERE a.execution_id = e.id), 0) + 1,
		       $2, $3, $4, $5, $6, $7
		FROM migration_executions e
		WHERE e.id = $1`,
		attempt.ExecutionID, string(attempt.Status), attempt.SQLSnapshot,
		nullString(attempt.ErrorCode), nullString(attempt.ErrorMessage), attempt.StartedAt, attempt.FinishedAt)
	if err != nil {
		return adminError(err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return admin.ErrNotFound
	}
	return nil
}

func (s *HistoryPostgresStore) ListAttempts(ctx context.Context, databaseID int64) ([]domain.ExecutionAttempt, error) {
	return s.ListExecutionHistory(ctx, databaseID, false)
}

func (s *HistoryPostgresStore) ListExecutionHistory(ctx context.Context, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.attempt_id, a.execution_id, e.migration_id, m.version, e.revision_id, r.revision,
		       e.action, e.mode, COALESCE(e.requested_by, 0), COALESCE(u.username, ''),
		       e.target_execution_id::text, a.attempt_number, a.status, a.sql_snapshot,
		       a.error_code, a.error_message, a.started_at, a.finished_at
		FROM execution_attempts a
		JOIN migration_executions e ON e.id = a.execution_id
		JOIN migrations m ON m.id = e.migration_id
		JOIN migration_revisions r ON r.id = e.revision_id
		LEFT JOIN users u ON u.id = e.requested_by
		WHERE a.database_id = $1
		  AND (
		    NOT $2
		    OR a.execution_id NOT IN (
		      SELECT id FROM migration_executions
		      WHERE database_id = $1 AND action = 'ROLLBACK' AND status = 'SUCCEEDED' AND target_execution_id IS NOT NULL
		      UNION
		      SELECT target_execution_id FROM migration_executions
		      WHERE database_id = $1 AND action = 'ROLLBACK' AND status = 'SUCCEEDED' AND target_execution_id IS NOT NULL
		    )
		  )
		ORDER BY a.started_at, a.attempt_id`, databaseID, hideRollback)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []domain.ExecutionAttempt
	for rows.Next() {
		var (
			a                    domain.ExecutionAttempt
			action, mode, status string
			reverts              sql.NullString
			errCode, errMsg      sql.NullString
			finishedAt           sql.NullTime
		)
		if err := rows.Scan(
			&a.AttemptID, &a.ExecutionID, &a.MigrationID, &a.MigrationVersion, &a.RevisionID, &a.Revision,
			&action, &mode, &a.ActorID, &a.ActorUsername,
			&reverts, &a.AttemptNumber, &status, &a.SQLSnapshot,
			&errCode, &errMsg, &a.StartedAt, &finishedAt,
		); err != nil {
			return nil, err
		}
		a.Action = domain.MigrationAction(action)
		a.Mode = domain.ExecutionMode(mode)
		a.Status = domain.ExecutionStatus(status)
		a.ErrorCode, a.ErrorMessage = errCode.String, errMsg.String
		if reverts.Valid {
			t := reverts.String
			a.RevertsExecutionID = &t
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			a.FinishedAt = &t
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}
