package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

var _ worker.JobStateStore = (*JobPostgresStore)(nil)

func (s *JobPostgresStore) SaveJob(ctx context.Context, job domain.Job) error {
	progress, err := progressJSON(job.Progress)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var executionID string
	err = tx.QueryRowContext(ctx, `
		UPDATE migration_jobs
		SET status = $2, attempt_count = $3, progress = $4::jsonb,
		    error_code = $5, error_message = $6, updated_at = now()
		WHERE id = $1 AND status NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
		RETURNING execution_id`,
		job.ID, string(job.Status), job.AttemptCount, progress,
		nullString(job.ErrorCode), nullString(job.ErrorMessage)).Scan(&executionID)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if qErr := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM migration_jobs WHERE id = $1)`, job.ID).Scan(&exists); qErr != nil {
			return qErr
		}
		if exists {
			return admin.ErrConflict
		}
		return admin.ErrNotFound
	}
	if err != nil {
		return adminError(err)
	}
	if err := mirrorJobIntoExecution(ctx, tx, executionID, job); err != nil {
		return adminError(err)
	}
	return tx.Commit()
}
