package database

import (
	"context"
	"errors"
	"strconv"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/jackc/pgx/v5"
)

type revertBatch struct {
	batchNo int
	from    int64
	to      int64
}

func (e *TargetExecutor) runBatchRevert(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	if req.TargetExecutionID == nil || *req.TargetExecutionID == "" {
		return result, admin.ErrInvalidInput
	}
	source := *req.TargetExecutionID
	if !rev.Reversible {
		return refused(result, ErrorCodeIrreversible, migrationengine.ErrUnsafeRevertMessage), nil
	}
	if err := migrationengine.ValidateBatch(rev); err != nil {
		return refused(result, ErrorCodeInvalidBatch, err.Error()), nil
	}
	if _, err := inspectBatchTarget(ctx, conn, *rev.Batch); err != nil {
		if errors.Is(err, migrationengine.ErrInvalidBatch) {
			return refused(result, ErrorCodeInvalidBatch, err.Error()), nil
		}
		return result, err
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(row_count), 0)::bigint FROM public._dbmp_batch_checkpoints
		WHERE source_execution_id = $1 AND direction = 'DOWN' AND execution_id = $2`,
		source, req.ExecutionID).Scan(&result.AppliedBatches, &result.AffectedRows); err != nil {
		return result, err
	}
	refuse, err := e.revertRefusal(ctx, conn, req, rev, version, source)
	if err != nil || refuse.code != "" {
		return refused(result, refuse.code, refuse.message), err
	}
	if result.AppliedBatches > 0 {
		if err := e.reportRevert(ctx, req.ExecutionID, result, ""); err != nil {
			return progressFailed(result, err), nil
		}
	}
	for {
		if ctx.Err() != nil {
			return interrupted(ctx, result)
		}
		next, ok, err := nextRevertBatch(ctx, conn, source)
		if err != nil {
			return result, err
		}
		if !ok {
			return e.finishRevert(ctx, conn, req, source, result)
		}
		rows, err := e.runRevertBatch(ctx, conn, rev.DownSQL, source, req.ExecutionID, next)
		if err != nil {
			return batchFailure(ctx, result, err)
		}
		result.AppliedBatches++
		result.AffectedRows += rows
		if err := e.reportRevert(ctx, req.ExecutionID, result, strconv.FormatInt(next.from, 10)); err != nil {
			return progressFailed(result, err), nil
		}
	}
}

func (e *TargetExecutor) revertRefusal(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, source string) (refusal, error) {
	if refuse, err := e.revertSafety(ctx, conn, req, version, source); err != nil || refuse.code != "" {
		return refuse, err
	}
	var (
		execution, status string
		found             bool
	)
	err := conn.QueryRow(ctx, `
		SELECT COALESCE(execution_id::text, ''), status FROM public._dbmp_migrations WHERE migration_id = $1`,
		rev.MigrationID).Scan(&execution, &status)
	switch {
	case err == nil:
		found = true
	case !errors.Is(err, pgx.ErrNoRows):
		return refusal{}, err
	}
	if found && execution != source && execution != req.ExecutionID {
		return conflict("the execution to revert is not the one recorded on the target database"), nil
	}
	if !found {
		var batches int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM public._dbmp_batch_checkpoints WHERE source_execution_id = $1 AND direction = 'UP'`,
			source).Scan(&batches); err != nil {
			return refusal{}, err
		}
		if batches == 0 {
			return conflict("the execution to revert committed no batches on the target database"), nil
		}
	}
	return refusal{}, nil
}

func nextRevertBatch(ctx context.Context, conn *pgx.Conn, source string) (revertBatch, bool, error) {
	var b revertBatch
	err := conn.QueryRow(ctx, `
		SELECT u.batch_no, u.cursor_from, u.cursor_to FROM public._dbmp_batch_checkpoints u
		WHERE u.source_execution_id = $1 AND u.direction = 'UP'
		  AND NOT EXISTS (SELECT 1 FROM public._dbmp_batch_checkpoints d
		                  WHERE d.source_execution_id = u.source_execution_id AND d.batch_no = u.batch_no AND d.direction = 'DOWN')
		ORDER BY u.batch_no DESC LIMIT 1`, source).Scan(&b.batchNo, &b.from, &b.to)
	if errors.Is(err, pgx.ErrNoRows) {
		return revertBatch{}, false, nil
	}
	return b, err == nil, err
}

func (e *TargetExecutor) runRevertBatch(ctx context.Context, conn *pgx.Conn, downSQL, source, executionID string, b revertBatch) (int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	ev := observability.SQLEvent{
		Query:       downSQL,
		Parameters:  []any{b.from, b.to},
		ExecutionID: executionID,
		Batch:       b.batchNo,
	}
	sqlCtx, span, start := observability.StartSQL(ctx, nil, ev)
	tag, err := tx.Exec(sqlCtx, downSQL, b.from, b.to)
	rows := tag.RowsAffected()
	observability.EndSQL(sqlCtx, nil, span, start, ev, &rows, err)
	if err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		return 0, err
	}
	if _, err := tx.Exec(sqlCtx, `
		INSERT INTO public._dbmp_batch_checkpoints
			(source_execution_id, batch_no, direction, execution_id, cursor_from, cursor_to, row_count)
		VALUES ($1, $2, 'DOWN', $3, $4, $5, $6)`, source, b.batchNo, executionID, b.from, b.to, rows); err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		return 0, err
	}
	if err := e.commitBatch(sqlCtx, conn, tx); err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, true, err)
		return 0, err
	}
	observability.LogTxOutcome(sqlCtx, nil, ev, true, nil)
	return rows, nil
}

func (e *TargetExecutor) finishRevert(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, source string, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `
		UPDATE public._dbmp_migrations
		SET status = 'REVERTED', execution_id = $2, executed_by = $3, executed_at = now()
		WHERE execution_id = $1 AND status = 'APPLIED'`, source, req.ExecutionID, req.ActorID); err != nil {
		return result, err
	}
	result, ok := e.commitResult(ctx, conn, tx, result)
	if ok {
		result.Status = domain.ExecutionStatusSucceeded
	}
	return result, nil
}

func (e *TargetExecutor) reportRevert(ctx context.Context, executionID string, result domain.ExecutionResult, cursor string) error {
	return e.progress.Report(ctx, executionID, domain.BatchProgress{
		Status: domain.BatchStatusCompensated, Completed: result.AppliedBatches,
		LastCursor: cursor, AffectedRows: result.AffectedRows,
	})
}
