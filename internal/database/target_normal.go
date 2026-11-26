package database

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type normalPlan struct {
	sql    string
	record func(ctx context.Context, tx pgx.Tx) error
}

type refusal struct {
	code    string
	message string
}

func conflict(message string) refusal {
	return refusal{code: ErrorCodeLedgerConflict, message: message}
}

func (e *TargetExecutor) runNormal(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(targetMigrationLockKey)); err != nil {
		return result, err
	}
	var recorded string
	err = tx.QueryRow(ctx, `SELECT status FROM public._dbmp_migrations WHERE execution_id = $1`, req.ExecutionID).Scan(&recorded)
	if err == nil {
		result.Status = domain.ExecutionStatusSucceeded
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	plan, refuse, err := planNormal(ctx, tx, req, rev, version)
	if err != nil {
		return result, err
	}
	if refuse.code != "" {
		return refused(result, refuse.code, refuse.message), nil
	}
	if explorer.HasTransactionControl(plan.sql) {
		return refused(result, ErrorCodeTransactionControl, "transaction control statements are not allowed in migration SQL"), nil
	}
	affected, err := runStatements(ctx, tx, plan.sql)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return refused(result, pgErr.Code, describeError(err)), nil
		}
		return result, err
	}
	if tx.Conn().PgConn().TxStatus() != 'T' {
		return refused(result, ErrorCodeTransactionEnded, "migration SQL ended the transaction"), nil
	}
	if err := plan.record(ctx, tx); err != nil {
		return result, err
	}
	result, ok := e.commitResult(ctx, conn, tx, result)
	if !ok {
		return result, nil
	}
	result.Status, result.AffectedRows = domain.ExecutionStatusSucceeded, affected
	return result, nil
}

func planNormal(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64) (normalPlan, refusal, error) {
	switch req.Action {
	case domain.MigrationActionApply:
		return planApply(ctx, tx, req, rev, version)
	case domain.MigrationActionRollback:
		return planRollback(ctx, tx, req, rev)
	case domain.MigrationActionForward:
		return planForward(ctx, tx, req, rev)
	}
	return normalPlan{}, refusal{}, admin.ErrInvalidInput
}

func planApply(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64) (normalPlan, refusal, error) {
	var taken bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM public._dbmp_migrations WHERE migration_id = $1 OR version = $2)`,
		rev.MigrationID, version).Scan(&taken); err != nil {
		return normalPlan{}, refusal{}, err
	}
	if taken {
		return normalPlan{}, conflict("this migration or its version is already recorded on the target database"), nil
	}
	return normalPlan{sql: rev.UpSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id, status)
			VALUES ($1, $2, $3, $4, $5, 'APPLIED')`, version, rev.MigrationID, rev.UpSQL, req.ActorID, req.ExecutionID)
		return err
	}}, refusal{}, nil
}

func runStatements(ctx context.Context, tx pgx.Tx, sql string) (int64, error) {
	mrr := tx.Conn().PgConn().Exec(ctx, sql)
	var affected int64
	var firstErr error
	for mrr.NextResult() {
		tag, err := mrr.ResultReader().Close()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		affected += tag.RowsAffected()
	}
	if err := mrr.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return affected, firstErr
}

func commitRejected(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil, false
	}
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return pgErr, severity == "ERROR"
}
