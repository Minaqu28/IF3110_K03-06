package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
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

var _ worker.JobStore = (*JobPostgresStore)(nil)

func (s *JobPostgresStore) SaveJob(ctx context.Context, job domain.Job) error {
	return s.saveJob(ctx, job, nil, nil)
}

func (s *JobPostgresStore) SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error {
	return s.saveJob(ctx, job, nil, &notBefore)
}

func (s *JobPostgresStore) saveJob(ctx context.Context, job domain.Job, guard func(context.Context, *sql.Tx, string) error, notBefore *time.Time) error {
	progress, err := progressJSON(job.Progress)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(ctx, tx, job.ID); err != nil {
			return err
		}
	}
	var executionID string
	err = tx.QueryRowContext(ctx, `
		UPDATE migration_jobs
		SET status = $2, attempt_count = $3, progress = $4::jsonb,
		    error_code = $5, error_message = $6, updated_at = now(),
		    next_attempt_at = COALESCE($7, next_attempt_at),
		    published_at = CASE WHEN $2 IN ('RETRY_WAIT', 'NEEDS_RECONCILIATION') THEN NULL ELSE published_at END
		WHERE id = $1 AND status NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
		RETURNING execution_id`,
		job.ID, string(job.Status), job.AttemptCount, progress,
		nullString(job.ErrorCode), nullString(job.ErrorMessage), notBefore).Scan(&executionID)
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

type FencedJobStore struct {
	jobs  *JobPostgresStore
	owner string
	token int64
}

var _ worker.JobStore = (*FencedJobStore)(nil)

func (s *JobPostgresStore) Fenced(owner string, token int64) *FencedJobStore {
	return &FencedJobStore{jobs: s, owner: owner, token: token}
}

func (f *FencedJobStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	return f.jobs.GetJob(ctx, jobID)
}

func (f *FencedJobStore) SaveJob(ctx context.Context, job domain.Job) error {
	return f.jobs.saveJob(ctx, job, f.guard, nil)
}

func (f *FencedJobStore) SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error {
	return f.jobs.saveJob(ctx, job, f.guard, &notBefore)
}

func (f *FencedJobStore) guard(ctx context.Context, tx *sql.Tx, jobID string) error {
	var held bool
	err := tx.QueryRowContext(ctx, `
		SELECT TRUE
		FROM migration_jobs j
		JOIN database_locks l ON l.database_id = j.database_id AND l.execution_id = j.execution_id
		WHERE j.id = $1 AND l.owner = $2 AND l.fencing_token = $3 AND l.lease_expires_at > now()
		FOR SHARE OF l`, jobID, f.owner, f.token).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return worker.ErrLockLost
	}
	return adminError(err)
}

func mirrorJobIntoExecution(ctx context.Context, tx *sql.Tx, executionID string, job domain.Job) error {
	var status sql.NullString
	switch job.Status {
	case domain.JobStatusRunning:
		status = sql.NullString{String: string(domain.ExecutionStatusRunning), Valid: true}
	case domain.JobStatusNeedsReconciliation:
		status = sql.NullString{String: string(domain.ExecutionStatusUnknown), Valid: true}
	case domain.JobStatusSucceeded:
		status = sql.NullString{String: string(domain.ExecutionStatusSucceeded), Valid: true}
	}
	var batches, rows sql.NullInt64
	var cursor sql.NullString
	if job.Progress != nil {
		batches = sql.NullInt64{Int64: int64(job.Progress.Completed), Valid: true}
		rows = sql.NullInt64{Int64: job.Progress.AffectedRows, Valid: true}
		cursor = nullString(job.Progress.LastCursor)
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE migration_executions
		SET status = CASE
		        WHEN $2::text IS NOT NULL THEN $2
		        WHEN $6 IN ('FAILED', 'CANCELLED', 'RETRY_WAIT') AND COALESCE($3, applied_batches) > 0
		            THEN CASE WHEN action = 'ROLLBACK' THEN 'PARTIALLY_REVERTED' ELSE 'PARTIAL' END
		        WHEN $6 IN ('FAILED', 'CANCELLED') THEN 'FAILED'
		        ELSE status END,
		    started_at      = CASE WHEN $2 = 'RUNNING' THEN COALESCE(started_at, now()) ELSE started_at END,
		    finished_at     = CASE WHEN $6 IN ('SUCCEEDED', 'FAILED', 'CANCELLED') THEN now() ELSE finished_at END,
		    applied_batches = COALESCE($3, applied_batches),
		    affected_rows   = COALESCE($4, affected_rows),
		    last_cursor     = CASE WHEN $3 IS NULL THEN last_cursor ELSE $5 END,
		    error_code      = $7,
		    error_message   = $8
		WHERE id = $1 AND status NOT IN ('SUCCEEDED', 'FAILED')`,
		executionID, status, batches, rows, cursor, string(job.Status), nullString(job.ErrorCode), nullString(job.ErrorMessage))
	return err
}

func (s *JobPostgresStore) MarkPublished(ctx context.Context, jobID string) error {
	return rowsAffected(s.db.ExecContext(ctx, `
		UPDATE migration_jobs SET published_at = COALESCE(published_at, now())
		WHERE id = $1`, jobID))
}

func (s *JobPostgresStore) PublishDue(ctx context.Context, limit int, publish func(context.Context, domain.MigrationJobMessage) error) (int, error) {
	if limit <= 0 {
		return 0, admin.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, e.id, e.database_id, e.migration_id, e.revision_id, e.approval_request_id,
		       e.requested_by, e.action, e.mode, e.target_execution_id, e.requested_at, j.trace_context
		FROM migration_jobs j
		JOIN migration_executions e ON e.id = j.execution_id
		WHERE j.published_at IS NULL
		  AND j.status IN ('QUEUED', 'RETRY_WAIT', 'NEEDS_RECONCILIATION')
		  AND j.next_attempt_at <= now()
		ORDER BY j.next_attempt_at, j.created_at
		LIMIT $1
		FOR UPDATE OF j SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	messages, err := scanJobMessages(rows)
	if err != nil {
		return 0, err
	}
	published := 0
	var firstErr error
	for _, msg := range messages {
		if err := publish(ctx, msg); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE migration_jobs SET published_at = now() WHERE id = $1`, msg.JobID); err != nil {
			return 0, err
		}
		published++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return published, firstErr
}

func scanJobMessages(rows *sql.Rows) ([]domain.MigrationJobMessage, error) {
	defer rows.Close()
	var messages []domain.MigrationJobMessage
	for rows.Next() {
		var (
			msg          domain.MigrationJobMessage
			actor        sql.NullInt64
			action, mode string
			target       sql.NullString
			traceRaw     []byte
		)
		if err := rows.Scan(&msg.JobID, &msg.ExecutionID, &msg.DatabaseID, &msg.MigrationID, &msg.RevisionID,
			&msg.ApprovalRequestID, &actor, &action, &mode, &target, &msg.RequestedAt, &traceRaw); err != nil {
			return nil, err
		}
		msg.SchemaVersion = domain.MigrationJobSchemaVersion
		msg.ActorID = actor.Int64
		msg.Action = domain.MigrationAction(action)
		msg.Mode = domain.ExecutionMode(mode)
		if target.Valid {
			t := target.String
			msg.TargetExecutionID = &t
		}
		if len(traceRaw) > 0 {
			var tc map[string]string
			if err := json.Unmarshal(traceRaw, &tc); err == nil && len(tc) > 0 {
				msg.TraceContext = tc
			}
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *JobPostgresStore) SweepExpiredJobs(ctx context.Context) (int, error) {
	var swept int
	err := s.db.QueryRowContext(ctx, `
		WITH expired AS (
		    UPDATE migration_jobs j
		    SET status = 'NEEDS_RECONCILIATION', published_at = NULL, next_attempt_at = now(), updated_at = now(),
		        error_code = 'LEASE_EXPIRED', error_message = 'Worker berhenti sebelum hasil eksekusi tersimpan'
		    WHERE j.status = 'RUNNING'
		      AND NOT EXISTS (
		          SELECT 1 FROM database_locks l
		          WHERE l.database_id = j.database_id AND l.execution_id = j.execution_id
		            AND l.lease_expires_at > now()
		      )
		    RETURNING j.execution_id
		), executions AS (
		    UPDATE migration_executions e
		    SET status = 'UNKNOWN'
		    WHERE e.id IN (SELECT execution_id FROM expired) AND e.status NOT IN ('SUCCEEDED', 'FAILED')
		)
		SELECT count(*) FROM expired`).Scan(&swept)
	return swept, err
}

func JobStatusCounts(db *sql.DB) func(context.Context) (map[domain.JobStatus]int64, error) {
	return func(ctx context.Context) (map[domain.JobStatus]int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT status, count(*) FROM migration_jobs GROUP BY status`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		counts := map[domain.JobStatus]int64{}
		for rows.Next() {
			var status string
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				return nil, err
			}
			counts[domain.JobStatus(status)] = n
		}
		return counts, rows.Err()
	}
}

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
