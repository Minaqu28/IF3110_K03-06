package database

import (
	"context"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

var _ migrationengine.TargetExecutor = (*TargetExecutor)(nil)

const (
	ErrorCodeLedgerConflict     = "LEDGER_CONFLICT"
	ErrorCodeIrreversible       = "IRREVERSIBLE"
	ErrorCodeTransactionControl = "TRANSACTION_CONTROL"
	ErrorCodeTransactionEnded   = "TRANSACTION_ENDED"
)

type TargetExecutor struct {
	store    *AdminPostgresStore
	options  TargetOptions
	progress migrationengine.ProgressSink
	now      func() time.Time
}

func NewTargetExecutor(store *AdminPostgresStore, options TargetOptions, progress migrationengine.ProgressSink) *TargetExecutor {
	return &TargetExecutor{store: store, options: options, progress: progress, now: time.Now}
}

func (e *TargetExecutor) Execute(ctx context.Context, req domain.ExecutionRequest, rev domain.MigrationRevision) (domain.ExecutionResult, error) {
	if req.ExecutionID == "" || !req.Action.Valid() || rev.ID != req.RevisionID || rev.MigrationID != req.MigrationID || rev.DatabaseID != req.DatabaseID {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	if req.Mode != domain.ExecutionModeNormal {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	started := e.now()
	var version int64
	if err := e.store.db.QueryRowContext(ctx, `SELECT version FROM migrations WHERE id = $1 AND database_id = $2`,
		req.MigrationID, req.DatabaseID).Scan(&version); err != nil {
		return domain.ExecutionResult{}, adminError(err)
	}
	target, err := e.store.openTarget(ctx, req.DatabaseID, e.options)
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	defer target.Close(context.WithoutCancel(ctx))
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		return domain.ExecutionResult{}, err
	}
	result := domain.ExecutionResult{ExecutionID: req.ExecutionID, RevertsExecutionID: req.TargetExecutionID, StartedAt: started}
	result, err = runNormal(ctx, target.conn, req, rev, version, result)
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	finished := e.now()
	result.FinishedAt = &finished
	return result, nil
}

func refused(result domain.ExecutionResult, code, message string) domain.ExecutionResult {
	result.Status, result.ErrorCode, result.ErrorMessage = domain.ExecutionStatusFailed, code, message
	return result
}
