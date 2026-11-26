package database

import (
	"context"
	"database/sql"

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
