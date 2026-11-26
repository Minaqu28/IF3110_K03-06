package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
)

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
