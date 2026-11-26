package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ workflow.ScopeReader = (*AdminPostgresStore)(nil)

func (s *AdminPostgresStore) BatchScope(ctx context.Context, databaseID int64, batch domain.BatchConfig) (workflow.BatchScope, error) {
	table, column, err := batchIdentifiers(batch)
	if err != nil {
		return workflow.BatchScope{}, err
	}
	target, err := s.OpenTarget(ctx, databaseID, true)
	if err != nil {
		return workflow.BatchScope{}, err
	}
	defer target.Close(context.WithoutCancel(ctx))
	pg, ok := target.(*pgTarget)
	if !ok {
		return workflow.BatchScope{}, errors.New("batch scope needs a PostgreSQL target")
	}
	var lo, hi sql.NullInt64
	err = pg.conn.QueryRow(ctx, `SELECT min(`+column+`), max(`+column+`) FROM `+table).Scan(&lo, &hi)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42703" || pgErr.Code == "3F000") {
		return workflow.BatchScope{}, admin.ErrInvalidInput
	}
	if err != nil {
		return workflow.BatchScope{}, err
	}
	if !hi.Valid {
		return workflow.BatchScope{}, nil
	}
	return workflow.BatchScope{Min: lo.Int64 - 1, Max: hi.Int64}, nil
}

func batchIdentifiers(batch domain.BatchConfig) (string, string, error) {
	parts := strings.Split(batch.Table, ".")
	if len(parts) > 2 || batch.CursorColumn == "" {
		return "", "", admin.ErrInvalidInput
	}
	for _, p := range parts {
		if p == "" {
			return "", "", admin.ErrInvalidInput
		}
	}
	return pgx.Identifier(parts).Sanitize(), pgx.Identifier{batch.CursorColumn}.Sanitize(), nil
}
