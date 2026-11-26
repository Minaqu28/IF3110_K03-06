package database

import (
	"context"
	"errors"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/jackc/pgx/v5"
)

func planRollback(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision) (normalPlan, refusal, error) {
	if req.TargetExecutionID == nil || *req.TargetExecutionID == "" {
		return normalPlan{}, refusal{}, admin.ErrInvalidInput
	}
	target := *req.TargetExecutionID
	if !rev.Reversible || strings.TrimSpace(rev.DownSQL) == "" {
		return normalPlan{}, refusal{code: ErrorCodeIrreversible, message: "this revision has no safe down SQL"}, nil
	}
	var (
		migrationID int64
		status      string
	)
	err := tx.QueryRow(ctx, `
		SELECT migration_id, status FROM public._dbmp_migrations WHERE execution_id = $1 FOR UPDATE`, target).
		Scan(&migrationID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return normalPlan{}, conflict("the execution to roll back is not the one recorded as applied on the target database"), nil
	}
	if err != nil {
		return normalPlan{}, refusal{}, err
	}
	if migrationID != rev.MigrationID || status != "APPLIED" {
		return normalPlan{}, conflict("the execution to roll back is not currently applied on the target database"), nil
	}
	return normalPlan{sql: rev.DownSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public._dbmp_migrations
			SET status = 'REVERTED', execution_id = $2, executed_by = $3, executed_at = now()
			WHERE execution_id = $1`, target, req.ExecutionID, req.ActorID)
		return err
	}}, refusal{}, nil
}

func planForward(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision) (normalPlan, refusal, error) {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT status FROM public._dbmp_migrations WHERE migration_id = $1 FOR UPDATE`, rev.MigrationID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return normalPlan{}, conflict("this migration was never applied on the target database; use APPLY"), nil
	}
	if err != nil {
		return normalPlan{}, refusal{}, err
	}
	if status != "REVERTED" {
		return normalPlan{}, conflict("only a reverted migration can be applied forward again"), nil
	}
	return normalPlan{sql: rev.UpSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public._dbmp_migrations
			SET status = 'APPLIED', execution_id = $2, sql_text = $3, executed_by = $4, executed_at = now()
			WHERE migration_id = $1`, rev.MigrationID, req.ExecutionID, rev.UpSQL, req.ActorID)
		return err
	}}, refusal{}, nil
}
