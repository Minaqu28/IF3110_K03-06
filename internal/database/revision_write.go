package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func (s *RevisionPostgresStore) CreateRevision(ctx context.Context, rev domain.MigrationRevision) (domain.MigrationRevision, error) {
	var batchTable, batchCol sql.NullString
	var batchSize sql.NullInt64
	if rev.Batch != nil {
		batchTable = sql.NullString{String: rev.Batch.Table, Valid: true}
		batchCol = sql.NullString{String: rev.Batch.CursorColumn, Valid: true}
		batchSize = sql.NullInt64{Int64: int64(rev.Batch.BatchSize), Valid: true}
	}
	created, err := scanRevision(s.db.QueryRowContext(ctx, `
		INSERT INTO migration_revisions (
		    migration_id, database_id, revision, title, up_sql, down_sql, reversible, irreversible_reason,
		    definition_hash, batch_table, batch_cursor_column, batch_size, created_by
		)
		SELECT m.id, m.database_id,
		       COALESCE((SELECT max(r.revision) FROM migration_revisions r WHERE r.migration_id = m.id), 0) + 1,
		       $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
		FROM migrations m
		WHERE m.id = $1 AND m.database_id = $2
		RETURNING `+revisionColumns,
		rev.MigrationID, rev.DatabaseID, rev.Title, rev.UpSQL, nullString(rev.DownSQL), rev.Reversible,
		nullString(rev.IrreversibleReason), domain.ComputeDefinitionHash(rev), batchTable, batchCol, batchSize,
		sql.NullInt64{Int64: rev.CreatedBy, Valid: rev.CreatedBy > 0}))
	if err != nil {
		return domain.MigrationRevision{}, adminError(err)
	}
	return created, nil
}

func (s *RevisionPostgresStore) LockRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.MigrationRevision{}, err
	}
	defer tx.Rollback()
	locked, err := lockRevisionTx(ctx, tx, databaseID, revisionID)
	if err != nil {
		return domain.MigrationRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MigrationRevision{}, err
	}
	return locked, nil
}

func lockRevisionTx(ctx context.Context, tx *sql.Tx, databaseID, revisionID int64) (domain.MigrationRevision, error) {
	rev, err := scanRevision(tx.QueryRowContext(ctx, `
		SELECT `+revisionColumns+` FROM migration_revisions
		WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, revisionID))
	if err != nil {
		return domain.MigrationRevision{}, adminError(err)
	}
	if rev.Status != domain.RevisionStatusDraft || rev.LockedAt != nil {
		return domain.MigrationRevision{}, admin.ErrConflict
	}
	locked, err := scanRevision(tx.QueryRowContext(ctx, `
		UPDATE migration_revisions
		SET status = 'IN_REVIEW', locked_at = now(), definition_hash = $2
		WHERE id = $1
		RETURNING `+revisionColumns, revisionID, domain.ComputeDefinitionHash(rev)))
	if err != nil {
		return domain.MigrationRevision{}, adminError(err)
	}
	return locked, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
