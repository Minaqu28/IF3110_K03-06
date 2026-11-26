package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

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
