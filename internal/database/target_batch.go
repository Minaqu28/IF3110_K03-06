package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type batchSpec struct {
	target      batchTarget
	sql         string
	sourceID    string
	executionID string
	direction   domain.Direction
	batchNo     int
	from        int64
	scopeMax    int64
	size        int
}

type batchOutcome struct {
	done bool
	to   int64
	rows int64
}

func (e *TargetExecutor) runBatch(ctx context.Context, conn *pgx.Conn, b batchSpec) (batchOutcome, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return batchOutcome{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var hi sql.NullInt64
	if err := tx.QueryRow(ctx, `
		SELECT max(k) FROM (
			SELECT `+b.target.column+` AS k FROM `+b.target.table+`
			WHERE `+b.target.column+` > $1 AND `+b.target.column+` <= $2
			ORDER BY `+b.target.column+` LIMIT $3) s`, b.from, b.scopeMax, b.size).Scan(&hi); err != nil {
		return batchOutcome{}, err
	}
	if !hi.Valid {
		return batchOutcome{done: true}, nil
	}
	if b.direction == domain.DirectionUp {
		if _, err := tx.Exec(ctx, `
			INSERT INTO public._dbmp_batch_backup (source_execution_id, batch_no, row_key, before_row)
			SELECT $1, $2, t.`+b.target.column+`, to_jsonb(t) FROM `+b.target.table+` t
			WHERE t.`+b.target.column+` > $3 AND t.`+b.target.column+` <= $4`,
			b.sourceID, b.batchNo, b.from, hi.Int64); err != nil {
			return batchOutcome{}, err
		}
	}
	ev := observability.SQLEvent{
		Query:       b.sql,
		Parameters:  []any{b.from, hi.Int64},
		ExecutionID: b.executionID,
		Batch:       b.batchNo,
	}
	sqlCtx, span, start := observability.StartSQL(ctx, nil, ev)
	tag, err := tx.Exec(sqlCtx, b.sql, b.from, hi.Int64)
	rows := tag.RowsAffected()
	observability.EndSQL(sqlCtx, nil, span, start, ev, &rows, err)
	if err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		return batchOutcome{}, err
	}
	if _, err := tx.Exec(sqlCtx, `
		INSERT INTO public._dbmp_batch_checkpoints
			(source_execution_id, batch_no, direction, execution_id, cursor_from, cursor_to, row_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.sourceID, b.batchNo, string(b.direction), b.executionID, b.from, hi.Int64, rows); err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		return batchOutcome{}, err
	}
	if err := e.commitBatch(sqlCtx, conn, tx); err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, true, err)
		return batchOutcome{}, err
	}
	observability.LogTxOutcome(sqlCtx, nil, ev, true, nil)
	return batchOutcome{to: hi.Int64, rows: rows}, nil
}

func (e *TargetExecutor) commitBatch(ctx context.Context, conn *pgx.Conn, tx pgx.Tx) error {
	if e.beforeCommit != nil {
		e.beforeCommit(conn.PgConn().PID())
	}
	if err := tx.Commit(ctx); err != nil {
		if pgErr, ok := commitRejected(err); ok {
			return pgErr
		}
		return fmt.Errorf("%w: %w", migrationengine.ErrCommitUncertain, err)
	}
	return nil
}

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
	result.Status, result.ErrorCode = partialStatus(result), ErrorCodeInterrupted
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
	result.Status, result.ErrorCode = partialStatus(result), ErrorCodeInterrupted
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

func unsafeRevert(reason string) refusal {
	return refusal{code: ErrorCodeUnsafeRevert, message: migrationengine.ErrUnsafeRevertMessage + " (" + reason + ")"}
}

func (e *TargetExecutor) revertSafety(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, version int64, source string) (refusal, error) {
	var (
		revisionID, migrationID int64
		action, mode            string
	)
	err := e.store.db.QueryRowContext(ctx, `
		SELECT revision_id, migration_id, action, mode FROM migration_executions WHERE id = $1 AND database_id = $2`,
		source, req.DatabaseID).Scan(&revisionID, &migrationID, &action, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return unsafeRevert("the execution to revert does not exist on this database"), nil
	}
	if err != nil {
		return refusal{}, err
	}
	switch {
	case migrationID != req.MigrationID || revisionID != req.RevisionID:
		return unsafeRevert("the down SQL must come from the revision that ran"), nil
	case domain.MigrationAction(action) == domain.MigrationActionRollback:
		return unsafeRevert("a rollback cannot be reverted"), nil
	case domain.ExecutionMode(mode) != domain.ExecutionModeBatch:
		return unsafeRevert("the execution to revert did not run in batch mode"), nil
	}
	var later bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM public._dbmp_migrations WHERE version > $1 AND status = 'APPLIED')`, version).Scan(&later); err != nil {
		return refusal{}, err
	}
	if later {
		return unsafeRevert("a later migration is still applied on this database"), nil
	}
	return refusal{}, nil
}
