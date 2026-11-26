package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

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
		SELECT a.attempt_id, a.execution_id, a.attempt_number, a.status, a.sql_snapshot,
		       a.error_code, a.error_message, a.started_at, a.finished_at
		FROM execution_attempts a
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
			a               domain.ExecutionAttempt
			status          string
			errCode, errMsg sql.NullString
			finishedAt      sql.NullTime
		)
		if err := rows.Scan(&a.AttemptID, &a.ExecutionID, &a.AttemptNumber, &status, &a.SQLSnapshot,
			&errCode, &errMsg, &a.StartedAt, &finishedAt); err != nil {
			return nil, err
		}
		a.Status = domain.ExecutionStatus(status)
		a.ErrorCode, a.ErrorMessage = errCode.String, errMsg.String
		if finishedAt.Valid {
			t := finishedAt.Time
			a.FinishedAt = &t
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}
