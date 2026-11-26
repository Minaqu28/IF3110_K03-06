package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func replayedJob(ctx context.Context, tx *sql.Tx, sub workflow.Submission) (workflow.SubmitResult, bool, error) {
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id = $1`, sub.JobID))
	if errors.Is(err, sql.ErrNoRows) {
		return workflow.SubmitResult{}, false, nil
	}
	if err != nil {
		return workflow.SubmitResult{}, false, adminError(err)
	}
	req := sub.Request
	var same bool
	if err := tx.QueryRowContext(ctx, `
		SELECT database_id = $2 AND revision_id = $3 AND action = $4
		       AND target_execution_id IS NOT DISTINCT FROM $5::uuid
		       AND requested_by IS NOT DISTINCT FROM $6::bigint
		       AND ($7 = '' OR id::text = $7)
		FROM migration_executions WHERE id = $1`,
		job.ExecutionID, req.DatabaseID, req.RevisionID, string(req.Action), req.TargetExecutionID,
		sql.NullInt64{Int64: req.ActorID, Valid: req.ActorID > 0}, req.ExecutionID).Scan(&same); err != nil {
		return workflow.SubmitResult{}, false, adminError(err)
	}
	if !same {
		return workflow.SubmitResult{}, false, admin.ErrConflict
	}
	return workflow.SubmitResult{Job: job, Created: false}, true, nil
}
