package database

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const targetMigrationLockKey = 7310002

const bookkeepingSQL = `
CREATE TABLE IF NOT EXISTS public._dbmp_migrations (
	version BIGINT PRIMARY KEY,
	migration_id BIGINT NOT NULL UNIQUE,
	sql_text TEXT NOT NULL,
	executed_by BIGINT NOT NULL,
	executed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	status TEXT NOT NULL DEFAULT 'APPLIED' CHECK (status = 'APPLIED')
);

DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_attribute
		WHERE attrelid = 'public._dbmp_migrations'::regclass AND attname = 'execution_id' AND NOT attisdropped
	) THEN
		ALTER TABLE public._dbmp_migrations ADD COLUMN execution_id UUID UNIQUE;
	END IF;
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'public._dbmp_migrations'::regclass AND conname = '_dbmp_migrations_status_v2'
	) THEN
		ALTER TABLE public._dbmp_migrations DROP CONSTRAINT IF EXISTS _dbmp_migrations_status_check;
		ALTER TABLE public._dbmp_migrations ADD CONSTRAINT _dbmp_migrations_status_v2
			CHECK (status IN ('APPLIED', 'PARTIAL', 'REVERTED'));
	END IF;
END $$;

CREATE TABLE IF NOT EXISTS public._dbmp_batch_checkpoints (
	source_execution_id UUID NOT NULL,
	batch_no INTEGER NOT NULL CHECK (batch_no > 0),
	direction TEXT NOT NULL CHECK (direction IN ('UP', 'DOWN')),
	execution_id UUID NOT NULL,
	cursor_from BIGINT NOT NULL,
	cursor_to BIGINT NOT NULL CHECK (cursor_to > cursor_from),
	row_count BIGINT NOT NULL CHECK (row_count >= 0),
	committed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (source_execution_id, batch_no, direction)
);

CREATE TABLE IF NOT EXISTS public._dbmp_batch_backup (
	source_execution_id UUID NOT NULL,
	batch_no INTEGER NOT NULL,
	row_key BIGINT NOT NULL,
	before_row JSONB NOT NULL,
	PRIMARY KEY (source_execution_id, batch_no, row_key)
);
`

func ensureBookkeeping(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(targetMigrationLockKey)); err != nil {
		return err
	}
	if _, err := tx.Conn().PgConn().Exec(ctx, bookkeepingSQL).ReadAll(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
