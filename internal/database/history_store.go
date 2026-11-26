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
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt_id, execution_id, attempt_number, status, sql_snapshot,
		       error_code, error_message, started_at, finished_at
		FROM execution_attempts
		WHERE database_id = $1
		ORDER BY started_at, attempt_id`, databaseID)
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
