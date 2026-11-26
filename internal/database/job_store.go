package database

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type JobPostgresStore struct {
	db *sql.DB
}

var _ workflow.JobStore = (*JobPostgresStore)(nil)

func NewJobPostgresStore(db *sql.DB) *JobPostgresStore {
	return &JobPostgresStore{db: db}
}

func (s *JobPostgresStore) CreateJob(ctx context.Context, job domain.Job) error {
	return createJob(ctx, s.db, job, nil)
}

func createJob(ctx context.Context, q dbtx, job domain.Job, traceContext map[string]string) error {
	if job.ID == "" || job.ExecutionID == "" {
		return admin.ErrInvalidInput
	}
	progress, err := progressJSON(job.Progress)
	if err != nil {
		return err
	}
	traceRaw, err := traceContextJSON(traceContext)
	if err != nil {
		return err
	}
	result, err := q.ExecContext(ctx, `
		INSERT INTO migration_jobs (id, execution_id, database_id, progress, trace_context)
		SELECT $1, e.id, e.database_id, $4::jsonb, $5::jsonb
		FROM migration_executions e
		WHERE e.id = $2 AND e.database_id = $3`,
		job.ID, job.ExecutionID, job.DatabaseID, progress, traceRaw)
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

func traceContextJSON(tc map[string]string) (sql.NullString, error) {
	if len(tc) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(tc)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func (s *JobPostgresStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id = $1`, jobID))
	if err != nil {
		return domain.Job{}, adminError(err)
	}
	return job, nil
}

const jobColumns = `id, execution_id, database_id, status, attempt_count, progress,
	error_code, error_message, created_at, updated_at`

func scanJob(row rowScanner) (domain.Job, error) {
	var (
		job             domain.Job
		status          string
		progress        []byte
		errCode, errMsg sql.NullString
	)
	if err := row.Scan(&job.ID, &job.ExecutionID, &job.DatabaseID, &status, &job.AttemptCount, &progress,
		&errCode, &errMsg, &job.CreatedAt, &job.UpdatedAt); err != nil {
		return domain.Job{}, err
	}
	job.Status = domain.JobStatus(status)
	job.ErrorCode, job.ErrorMessage = errCode.String, errMsg.String
	if progress != nil {
		job.Progress = &domain.BatchProgress{}
		if err := json.Unmarshal(progress, job.Progress); err != nil {
			return domain.Job{}, err
		}
	}
	return job, nil
}

func progressJSON(p *domain.BatchProgress) (sql.NullString, error) {
	if p == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func (s *JobPostgresStore) UpdateProgressByExecution(ctx context.Context, executionID string, progress domain.BatchProgress) error {
	raw, err := progressJSON(&progress)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE migration_jobs
		SET progress = $2::jsonb, updated_at = now()
		WHERE execution_id = $1 AND status = 'RUNNING'`, executionID, raw); err != nil {
		return adminError(err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE migration_executions
		SET applied_batches = GREATEST(applied_batches, $2),
		    affected_rows   = GREATEST(affected_rows, $3),
		    last_cursor     = COALESCE(NULLIF($4, ''), last_cursor)
		WHERE id = $1 AND status NOT IN ('SUCCEEDED', 'FAILED')`,
		executionID, progress.Completed, progress.AffectedRows, progress.LastCursor); err != nil {
		return adminError(err)
	}
	return tx.Commit()
}
