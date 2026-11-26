package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
)

var _ migrationengine.TargetExecutor = (*TargetExecutor)(nil)

const (
	ErrorCodeLedgerConflict     = "LEDGER_CONFLICT"
	ErrorCodeIrreversible       = "IRREVERSIBLE"
	ErrorCodeTransactionControl = "TRANSACTION_CONTROL"
	ErrorCodeTransactionEnded   = "TRANSACTION_ENDED"
	ErrorCodeCommitUncertain    = "COMMIT_UNCERTAIN"
	ErrorCodeInvalidBatch       = "INVALID_BATCH"
	ErrorCodeInterrupted        = "INTERRUPTED"
)

type TargetExecutor struct {
	store    *AdminPostgresStore
	options  TargetOptions
	progress migrationengine.ProgressSink
	now      func() time.Time

	beforeCommit func(pid uint32)
}

func NewTargetExecutor(store *AdminPostgresStore, options TargetOptions, progress migrationengine.ProgressSink) *TargetExecutor {
	return &TargetExecutor{store: store, options: options, progress: progress, now: time.Now}
}

func (e *TargetExecutor) Execute(ctx context.Context, req domain.ExecutionRequest, rev domain.MigrationRevision) (domain.ExecutionResult, error) {
	if req.ExecutionID == "" || !req.Action.Valid() || rev.ID != req.RevisionID || rev.MigrationID != req.MigrationID || rev.DatabaseID != req.DatabaseID {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	batch := req.Mode == domain.ExecutionModeBatch
	if (req.Mode != domain.ExecutionModeNormal && !batch) || batch != (rev.Batch != nil) {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	if batch && req.Action == domain.MigrationActionRollback {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	started := e.now()
	var version int64
	if err := e.store.db.QueryRowContext(ctx, `SELECT version FROM migrations WHERE id = $1 AND database_id = $2`,
		req.MigrationID, req.DatabaseID).Scan(&version); err != nil {
		return domain.ExecutionResult{}, adminError(err)
	}
	var scope executionScope
	if batch {
		var err error
		if scope, err = e.loadScope(ctx, req); err != nil {
			return domain.ExecutionResult{}, err
		}
	}
	target, err := e.store.openTarget(ctx, req.DatabaseID, e.options)
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	defer target.Close(context.WithoutCancel(ctx))
	if batch {
		if err := lockTargetSession(ctx, target.conn); err != nil {
			return domain.ExecutionResult{}, err
		}
	}
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		return domain.ExecutionResult{}, err
	}
	result := domain.ExecutionResult{ExecutionID: req.ExecutionID, RevertsExecutionID: req.TargetExecutionID, StartedAt: started}
	if batch {
		result, err = e.runBatchMode(ctx, target.conn, req, rev, version, scope, result)
	} else {
		result, err = e.runNormal(ctx, target.conn, req, rev, version, result)
	}
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	finished := e.now()
	result.FinishedAt = &finished
	return result, nil
}

func refused(result domain.ExecutionResult, code, message string) domain.ExecutionResult {
	result.Status, result.ErrorCode, result.ErrorMessage = domain.ExecutionStatusFailed, code, message
	if result.AppliedBatches > 0 {
		result.Status = domain.ExecutionStatusPartial
	}
	return result
}

func (e *TargetExecutor) commitResult(ctx context.Context, conn *pgx.Conn, tx pgx.Tx, result domain.ExecutionResult) (domain.ExecutionResult, bool) {
	if e.beforeCommit != nil {
		e.beforeCommit(conn.PgConn().PID())
	}
	if err := tx.Commit(ctx); err != nil {
		if pgErr, ok := commitRejected(err); ok {
			return refused(result, pgErr.Code, describeError(err)), false
		}
		return uncertain(result, err), false
	}
	return result, true
}

func uncertain(result domain.ExecutionResult, err error) domain.ExecutionResult {
	result.Status, result.ErrorCode = domain.ExecutionStatusUnknown, ErrorCodeCommitUncertain
	if !errors.Is(err, migrationengine.ErrCommitUncertain) {
		err = fmt.Errorf("%w: %w", migrationengine.ErrCommitUncertain, err)
	}
	result.ErrorMessage = err.Error()
	return result
}
