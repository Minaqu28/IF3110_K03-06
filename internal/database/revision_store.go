package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

type RevisionPostgresStore struct {
	db *sql.DB
}

var _ migrationengine.RevisionStore = (*RevisionPostgresStore)(nil)

func NewRevisionPostgresStore(db *sql.DB) *RevisionPostgresStore {
	return &RevisionPostgresStore{db: db}
}

const revisionColumns = `id, migration_id, database_id, revision, title, up_sql, down_sql, reversible,
	irreversible_reason, status, definition_hash, batch_table, batch_cursor_column, batch_size,
	created_by, created_at, locked_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRevision(row rowScanner) (domain.MigrationRevision, error) {
	var (
		rev                  domain.MigrationRevision
		downSQL, reason      sql.NullString
		batchTable, batchCol sql.NullString
		batchSize            sql.NullInt64
		createdBy            sql.NullInt64
		lockedAt             sql.NullTime
		status               string
	)
	err := row.Scan(&rev.ID, &rev.MigrationID, &rev.DatabaseID, &rev.Revision, &rev.Title, &rev.UpSQL,
		&downSQL, &rev.Reversible, &reason, &status, &rev.DefinitionHash, &batchTable, &batchCol, &batchSize,
		&createdBy, &rev.CreatedAt, &lockedAt)
	if err != nil {
		return domain.MigrationRevision{}, err
	}
	rev.DownSQL = downSQL.String
	rev.IrreversibleReason = reason.String
	rev.Status = domain.RevisionStatus(status)
	rev.CreatedBy = createdBy.Int64
	if lockedAt.Valid {
		t := lockedAt.Time
		rev.LockedAt = &t
	}
	if batchTable.Valid {
		rev.Batch = &domain.BatchConfig{Table: batchTable.String, CursorColumn: batchCol.String, BatchSize: int(batchSize.Int64)}
	}
	return rev, nil
}

func (s *RevisionPostgresStore) GetRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error) {
	rev, err := scanRevision(s.db.QueryRowContext(ctx, `
		SELECT `+revisionColumns+` FROM migration_revisions
		WHERE database_id = $1 AND id = $2`, databaseID, revisionID))
	if err != nil {
		return domain.MigrationRevision{}, adminError(err)
	}
	return rev, nil
}

func (s *RevisionPostgresStore) ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+revisionColumns+` FROM migration_revisions
		WHERE database_id = $1 ORDER BY migration_id, revision`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []domain.MigrationRevision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, rev)
	}
	return revisions, rows.Err()
}

func (s *RevisionPostgresStore) CreateRevision(ctx context.Context, rev domain.MigrationRevision) (domain.MigrationRevision, error) {
	var batchTable, batchCol sql.NullString
	var batchSize sql.NullInt64
	if rev.Batch != nil {
		batchTable = sql.NullString{String: rev.Batch.Table, Valid: true}
		batchCol = sql.NullString{String: rev.Batch.CursorColumn, Valid: true}
		batchSize = sql.NullInt64{Int64: int64(rev.Batch.BatchSize), Valid: true}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.MigrationRevision{}, err
	}
	defer tx.Rollback()
	created, err := scanRevision(tx.QueryRowContext(ctx, `
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
	if err := staleOlderReviewsTx(ctx, tx, created.MigrationID, created.Revision); err != nil {
		return domain.MigrationRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MigrationRevision{}, err
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

func staleOlderReviewsTx(ctx context.Context, tx *sql.Tx, migrationID int64, revision int) error {
	_, err := tx.ExecContext(ctx, `
		WITH staled AS (
		    UPDATE review_requests q
		    SET status = 'STALE', resolved_at = now()
		    FROM migration_revisions r
		    WHERE q.revision_id = r.id AND r.migration_id = $1 AND r.revision < $2 AND q.status = 'PENDING'
		    RETURNING q.id
		)
		UPDATE approval_decisions d
		SET status = 'STALE'
		WHERE d.review_id IN (SELECT id FROM staled)`, migrationID, revision)
	return adminError(err)
}
