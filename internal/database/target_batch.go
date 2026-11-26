package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
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
	tag, err := tx.Exec(ctx, b.sql, b.from, hi.Int64)
	if err != nil {
		return batchOutcome{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public._dbmp_batch_checkpoints
			(source_execution_id, batch_no, direction, execution_id, cursor_from, cursor_to, row_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.sourceID, b.batchNo, string(b.direction), b.executionID, b.from, hi.Int64, tag.RowsAffected()); err != nil {
		return batchOutcome{}, err
	}
	if err := e.commitBatch(ctx, conn, tx); err != nil {
		return batchOutcome{}, err
	}
	return batchOutcome{to: hi.Int64, rows: tag.RowsAffected()}, nil
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
