package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type executionScope struct {
	min int64
	max int64
}

type checkpointProgress struct {
	lastBatch int
	cursor    int64
	rows      int64
}

func (e *TargetExecutor) loadScope(ctx context.Context, req domain.ExecutionRequest) (executionScope, error) {
	var lo, hi sql.NullInt64
	if err := e.store.db.QueryRowContext(ctx, `
		SELECT scope_min, scope_max FROM migration_executions WHERE id = $1 AND database_id = $2`,
		req.ExecutionID, req.DatabaseID).Scan(&lo, &hi); err != nil {
		return executionScope{}, adminError(err)
	}
	if !lo.Valid || !hi.Valid {
		return executionScope{}, admin.ErrInvalidInput
	}
	return executionScope{min: lo.Int64, max: hi.Int64}, nil
}

func loadCheckpoints(ctx context.Context, conn *pgx.Conn, sourceID string, direction domain.Direction, start int64) (checkpointProgress, error) {
	var p checkpointProgress
	err := conn.QueryRow(ctx, `
		SELECT COALESCE(max(batch_no), 0), COALESCE(max(cursor_to), $3), COALESCE(sum(row_count), 0)::bigint
		FROM public._dbmp_batch_checkpoints WHERE source_execution_id = $1 AND direction = $2`,
		sourceID, string(direction), start).Scan(&p.lastBatch, &p.cursor, &p.rows)
	return p, err
}

func (e *TargetExecutor) runBatchMode(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, scope executionScope, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	if err := migrationengine.ValidateBatch(rev); err != nil {
		return refused(result, ErrorCodeInvalidBatch, err.Error()), nil
	}
	target, err := inspectBatchTarget(ctx, conn, *rev.Batch)
	if errors.Is(err, migrationengine.ErrInvalidBatch) {
		return refused(result, ErrorCodeInvalidBatch, err.Error()), nil
	}
	if err != nil {
		return result, err
	}
	progress, err := loadCheckpoints(ctx, conn, req.ExecutionID, domain.DirectionUp, scope.min)
	if err != nil {
		return result, err
	}
	result.AppliedBatches, result.AffectedRows = progress.lastBatch, progress.rows
	if progress.lastBatch > 0 {
		if err := e.report(ctx, req.ExecutionID, result, progress.cursor); err != nil {
			return progressFailed(result, err), nil
		}
	}
	var recorded bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public._dbmp_migrations WHERE execution_id = $1)`, req.ExecutionID).Scan(&recorded); err != nil {
		return result, err
	}
	if recorded {
		result.Status = domain.ExecutionStatusSucceeded
		return result, nil
	}
	if refuse, err := checkLedger(ctx, conn, req, rev, version); err != nil || refuse.code != "" {
		return refused(result, refuse.code, refuse.message), err
	}
	spec := batchSpec{
		target: target, sql: rev.UpSQL, sourceID: req.ExecutionID, executionID: req.ExecutionID,
		direction: domain.DirectionUp, batchNo: progress.lastBatch + 1, from: progress.cursor,
		scopeMax: scope.max, size: rev.Batch.BatchSize,
	}
	for {
		if ctx.Err() != nil {
			return interrupted(ctx, result)
		}
		out, err := e.runBatch(ctx, conn, spec)
		if err != nil {
			return batchFailure(ctx, result, err)
		}
		if out.done {
			return e.finishBatch(ctx, conn, req, rev, version, result)
		}
		result.AppliedBatches++
		result.AffectedRows += out.rows
		spec.batchNo++
		spec.from = out.to
		if err := e.report(ctx, req.ExecutionID, result, out.to); err != nil {
			return progressFailed(result, err), nil
		}
	}
}

func (e *TargetExecutor) report(ctx context.Context, executionID string, result domain.ExecutionResult, cursor int64) error {
	return e.progress.Report(ctx, executionID, domain.BatchProgress{
		Status: domain.BatchStatusCommitted, Completed: result.AppliedBatches,
		LastCursor: strconv.FormatInt(cursor, 10), AffectedRows: result.AffectedRows,
	})
}

func progressFailed(result domain.ExecutionResult, err error) domain.ExecutionResult {
	result.Status, result.ErrorCode = domain.ExecutionStatusPartial, ErrorCodeInterrupted
	result.ErrorMessage = "stopped after a batch because its progress could not be saved: " + err.Error()
	return result
}

func checkLedger(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64) (refusal, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return refusal{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	_, refuse, err := planNormal(ctx, tx, req, rev, version)
	return refuse, err
}

func (e *TargetExecutor) finishBatch(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(targetMigrationLockKey)); err != nil {
		return result, err
	}
	plan, refuse, err := planNormal(ctx, tx, req, rev, version)
	if err != nil {
		return result, err
	}
	if refuse.code != "" {
		return refused(result, refuse.code, refuse.message), nil
	}
	if err := plan.record(ctx, tx); err != nil {
		return result, err
	}
	result, ok := e.commitResult(ctx, conn, tx, result)
	if ok {
		result.Status = domain.ExecutionStatusSucceeded
	}
	return result, nil
}

func batchFailure(ctx context.Context, result domain.ExecutionResult, err error) (domain.ExecutionResult, error) {
	if errors.Is(err, migrationengine.ErrCommitUncertain) {
		return uncertain(result, err), nil
	}
	if ctx.Err() != nil {
		return interrupted(ctx, result)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return refused(result, pgErr.Code, describeError(err)), nil
	}
	return result, err
}

func interrupted(ctx context.Context, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	if result.AppliedBatches == 0 {
		return result, ctx.Err()
	}
	result.Status, result.ErrorCode = domain.ExecutionStatusPartial, ErrorCodeInterrupted
	result.ErrorMessage = "stopped between batches; resume continues from the last checkpoint"
	return result, nil
}

func lockTargetSession(ctx context.Context, conn *pgx.Conn) error {
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(targetMigrationLockKey)).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return migrationengine.ErrTargetBusy
	}
	return nil
}
