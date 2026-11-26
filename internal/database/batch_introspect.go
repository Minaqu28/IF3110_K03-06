package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
)

type batchTarget struct {
	table  string
	column string
}

func inspectBatchTarget(ctx context.Context, conn *pgx.Conn, b domain.BatchConfig) (batchTarget, error) {
	table, column, err := batchIdentifiers(b)
	if err != nil {
		return batchTarget{}, fmt.Errorf("%w: malformed table or cursor column", migrationengine.ErrInvalidBatch)
	}
	var (
		relation sql.NullInt64
		kind     string
	)
	if err := conn.QueryRow(ctx, `
		SELECT c.oid::bigint, c.relkind::text FROM pg_class c WHERE c.oid = to_regclass($1)`, table).
		Scan(&relation, &kind); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return batchTarget{}, err
	}
	if !relation.Valid || (kind != "r" && kind != "p") {
		return batchTarget{}, fmt.Errorf("%w: table %s does not exist", migrationengine.ErrInvalidBatch, b.Table)
	}
	var (
		attnum   int16
		typeName string
		notNull  bool
		unique   bool
	)
	err = conn.QueryRow(ctx, `
		SELECT a.attnum, format_type(a.atttypid, NULL), a.attnotnull,
		       EXISTS (SELECT 1 FROM pg_index i
		               WHERE i.indrelid = a.attrelid AND i.indisunique AND i.indnkeyatts = 1
		                 AND i.indkey[0] = a.attnum AND i.indpred IS NULL)
		FROM pg_attribute a
		WHERE a.attrelid = $1::bigint::oid AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`,
		relation.Int64, b.CursorColumn).Scan(&attnum, &typeName, &notNull, &unique)
	if errors.Is(err, pgx.ErrNoRows) {
		return batchTarget{}, fmt.Errorf("%w: column %s does not exist on %s", migrationengine.ErrInvalidBatch, b.CursorColumn, b.Table)
	}
	if err != nil {
		return batchTarget{}, err
	}
	switch {
	case typeName != "smallint" && typeName != "integer" && typeName != "bigint":
		return batchTarget{}, fmt.Errorf("%w: cursor column %s is %s, not an integer type", migrationengine.ErrInvalidBatch, b.CursorColumn, typeName)
	case !notNull:
		return batchTarget{}, fmt.Errorf("%w: cursor column %s must be NOT NULL", migrationengine.ErrInvalidBatch, b.CursorColumn)
	case !unique:
		return batchTarget{}, fmt.Errorf("%w: cursor column %s needs a unique index of its own", migrationengine.ErrInvalidBatch, b.CursorColumn)
	}
	return batchTarget{table: table, column: column}, nil
}
