package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const targetConnectTimeout = 5 * time.Second

var (
	_ explorer.Store               = (*AdminPostgresStore)(nil)
	_ explorer.MigrationStore      = (*AdminPostgresStore)(nil)
	_ explorer.AttemptStore        = (*AdminPostgresStore)(nil)
	_ explorer.TransactionalTarget = (*pgTarget)(nil)
	_ explorer.SchemaTarget        = (*pgTarget)(nil)
	_ explorer.VersionedTarget     = (*pgTarget)(nil)
)

func TargetDSN(c domain.Connection, password string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.DBuser, password),
		Host:     net.JoinHostPort(c.Host, strconv.FormatInt(c.Port, 10)),
		Path:     "/" + c.DBname,
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}
	return u.String()
}

func (s *AdminPostgresStore) OpenTarget(ctx context.Context, dbID int64, readOnly bool) (explorer.Target, error) {
	target, err := s.openTarget(ctx, dbID, ViewerTargetOptions(readOnly))
	if err != nil {
		return nil, err
	}
	return target, nil
}

func describeConnectError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return &explorer.ConnectError{Reason: "autentikasi database gagal (username atau password salah).", Err: err}
		case "3D000":
			return &explorer.ConnectError{Reason: "database tidak ditemukan pada server target.", Err: err}
		case "42501":
			return &explorer.ConnectError{Reason: "akses ke database ditolak oleh server.", Err: err}
		case "53300":
			return &explorer.ConnectError{Reason: "koneksi server database penuh (too many connections).", Err: err}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &explorer.ConnectError{Reason: "batas waktu koneksi habis (timeout).", Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &explorer.ConnectError{Reason: "batas waktu koneksi habis (timeout).", Err: err}
	}
	return &explorer.ConnectError{Reason: "host atau port database tidak dapat dijangkau.", Err: err}
}

type pgTarget struct {
	conn     *pgx.Conn
	readOnly bool
	dbID     int64
	dbName   string
}

func (t *pgTarget) sqlEvent(query string, readOnly bool, params ...any) observability.SQLEvent {
	return observability.SQLEvent{
		DatabaseID:   t.dbID,
		DatabaseName: t.dbName,
		Query:        query,
		Parameters:   params,
		ReadOnly:     readOnly,
	}
}

func (t *pgTarget) Close(ctx context.Context) error { return t.conn.Close(ctx) }

func (t *pgTarget) Tables(ctx context.Context) ([]explorer.Table, error) {
	return t.TablesInSchema(ctx, "public")
}

func (t *pgTarget) Schemas(ctx context.Context) ([]string, error) {
	t.resetTx(ctx)
	const query = `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name <> 'information_schema' AND schema_name NOT LIKE 'pg_%'
		ORDER BY schema_name`
	ev := t.sqlEvent(query, true)
	ctx, span, start := observability.StartSQL(ctx, nil, ev)
	rows, err := t.conn.Query(ctx, query)
	if err != nil {
		observability.EndSQL(ctx, nil, span, start, ev, nil, err)
		return nil, err
	}
	schemas, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var schema string
		err := row.Scan(&schema)
		return schema, err
	})
	rowCount := int64(len(schemas))
	observability.EndSQL(ctx, nil, span, start, ev, &rowCount, err)
	return schemas, err
}

func (t *pgTarget) TablesInSchema(ctx context.Context, schema string) ([]explorer.Table, error) {
	t.resetTx(ctx)
	const listQuery = `
		SELECT table_name, table_type FROM information_schema.tables
		WHERE table_schema = $1 ORDER BY table_name`
	ev := t.sqlEvent(listQuery, true, schema)
	qCtx, span, start := observability.StartSQL(ctx, nil, ev)
	rows, err := t.conn.Query(qCtx, listQuery, schema)
	if err != nil {
		observability.EndSQL(qCtx, nil, span, start, ev, nil, err)
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Table, error) {
		table := explorer.Table{Schema: schema}
		err := row.Scan(&table.Name, &table.Type)
		return table, err
	})
	rowCount := int64(len(tables))
	observability.EndSQL(qCtx, nil, span, start, ev, &rowCount, err)
	if err != nil {
		return nil, err
	}
	for i := range tables {
		ident := pgx.Identifier{schema, tables[i].Name}.Sanitize()
		countQuery := `SELECT count(*) FROM ` + ident
		cEv := t.sqlEvent(countQuery, true)
		cCtx, cSpan, cStart := observability.StartSQL(ctx, nil, cEv)
		cErr := t.conn.QueryRow(cCtx, countQuery).Scan(&tables[i].Rows)
		one := int64(1)
		if cErr != nil {
			tables[i].Rows = -1
			observability.EndSQL(cCtx, nil, cSpan, cStart, cEv, nil, cErr)
		} else {
			observability.EndSQL(cCtx, nil, cSpan, cStart, cEv, &one, nil)
		}
	}
	return tables, nil
}

func (t *pgTarget) Preview(ctx context.Context, table string, limit int) (explorer.StatementResult, error) {
	return t.PreviewPage(ctx, "public", table, limit, 0)
}

func (t *pgTarget) tableExists(ctx context.Context, schema, table string) (bool, error) {
	var exists bool
	err := t.conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = $2)`, schema, table).Scan(&exists)
	return exists, err
}

func (t *pgTarget) TableDetails(ctx context.Context, schema, table string) (explorer.TableDetails, error) {
	t.resetTx(ctx)
	exists, err := t.tableExists(ctx, schema, table)
	if err != nil {
		return explorer.TableDetails{}, err
	}
	if !exists {
		return explorer.TableDetails{}, explorer.ErrTableNotFound
	}
	var details explorer.TableDetails
	const colQuery = `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`
	colEv := t.sqlEvent(colQuery, true, schema, table)
	colCtx, colSpan, colStart := observability.StartSQL(ctx, nil, colEv)
	rows, err := t.conn.Query(colCtx, colQuery, schema, table)
	if err != nil {
		observability.EndSQL(colCtx, nil, colSpan, colStart, colEv, nil, err)
		return details, err
	}
	details.Columns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Column, error) {
		var column explorer.Column
		err := row.Scan(&column.Name, &column.Type, &column.Nullable, &column.Default)
		return column, err
	})
	colCount := int64(len(details.Columns))
	observability.EndSQL(colCtx, nil, colSpan, colStart, colEv, &colCount, err)
	if err != nil {
		return details, err
	}
	const conQuery = `
		SELECT con.conname, con.contype::text, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY con.conname`
	conEv := t.sqlEvent(conQuery, true, schema, table)
	conCtx, conSpan, conStart := observability.StartSQL(ctx, nil, conEv)
	rows, err = t.conn.Query(conCtx, conQuery, schema, table)
	if err != nil {
		observability.EndSQL(conCtx, nil, conSpan, conStart, conEv, nil, err)
		return details, err
	}
	details.Constraints, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Constraint, error) {
		var constraint explorer.Constraint
		err := row.Scan(&constraint.Name, &constraint.Type, &constraint.Definition)
		return constraint, err
	})
	conCount := int64(len(details.Constraints))
	observability.EndSQL(conCtx, nil, conSpan, conStart, conEv, &conCount, err)
	if err != nil {
		return details, err
	}
	const idxQuery = `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2
		ORDER BY indexname`
	idxEv := t.sqlEvent(idxQuery, true, schema, table)
	idxCtx, idxSpan, idxStart := observability.StartSQL(ctx, nil, idxEv)
	rows, err = t.conn.Query(idxCtx, idxQuery, schema, table)
	if err != nil {
		observability.EndSQL(idxCtx, nil, idxSpan, idxStart, idxEv, nil, err)
		return details, err
	}
	details.Indexes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Index, error) {
		var index explorer.Index
		err := row.Scan(&index.Name, &index.Definition)
		return index, err
	})
	idxCount := int64(len(details.Indexes))
	observability.EndSQL(idxCtx, nil, idxSpan, idxStart, idxEv, &idxCount, err)
	return details, err
}

func (t *pgTarget) PreviewPage(ctx context.Context, schema, table string, limit, offset int) (explorer.StatementResult, error) {
	t.resetTx(ctx)
	if limit < 1 || limit > explorer.MaxResultRows || offset < 0 {
		return explorer.StatementResult{}, errors.New("invalid pagination")
	}
	exists, err := t.tableExists(ctx, schema, table)
	if err != nil {
		return explorer.StatementResult{}, err
	}
	if !exists {
		return explorer.StatementResult{}, explorer.ErrTableNotFound
	}
	order, err := t.previewOrder(ctx, schema, table)
	if err != nil {
		return explorer.StatementResult{}, err
	}
	query := `SELECT * FROM ` + pgx.Identifier{schema, table}.Sanitize() + ` ORDER BY ` + order + ` LIMIT ` + strconv.Itoa(limit+1) + ` OFFSET ` + strconv.Itoa(offset)
	ev := t.sqlEvent(query, true)
	ctx, span, start := observability.StartSQL(ctx, nil, ev)
	result, rowCount, execErr := collectWithMeta(t.conn.PgConn().ExecParams(ctx, query, nil, nil, nil, nil), limit)
	observability.EndSQL(ctx, nil, span, start, ev, &rowCount, execErr)
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

func (t *pgTarget) previewOrder(ctx context.Context, schema, table string) (string, error) {
	rows, err := t.conn.Query(ctx, `
		SELECT a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		WHERE n.nspname = $1 AND c.relname = $2 AND i.indisprimary
		ORDER BY a.attnum`, schema, table)
	if err != nil {
		return "", err
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var name string
		err := row.Scan(&name)
		return name, err
	})
	if err != nil {
		return "", err
	}
	if len(keys) > 0 {
		for i := range keys {
			keys[i] = pgx.Identifier{keys[i]}.Sanitize()
		}
		return strings.Join(keys, ", "), nil
	}
	var kind string
	err = t.conn.QueryRow(ctx, `
		SELECT c.relkind::text FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&kind)
	if err != nil {
		return "", err
	}
	if kind == "r" {
		return "ctid", nil
	}
	return "1", nil
}

func (t *pgTarget) Run(ctx context.Context, sql string, maxRows int) []explorer.StatementResult {
	defer t.resetTx(ctx)
	ev := t.sqlEvent(sql, t.readOnly)
	ctx, span, start := observability.StartSQL(ctx, nil, ev)
	pg := t.conn.PgConn()
	if t.readOnly {
		res, rowCount, err := collectWithMeta(pg.ExecParams(ctx, sql, nil, nil, nil, nil), maxRows)
		observability.EndSQL(ctx, nil, span, start, ev, &rowCount, err)
		return []explorer.StatementResult{res}
	}
	var results []explorer.StatementResult
	var totalRows int64
	var firstErr error
	mrr := pg.Exec(ctx, sql)
	for mrr.NextResult() {
		res, rc, err := collectWithMeta(mrr.ResultReader(), maxRows)
		totalRows += rc
		if err != nil && firstErr == nil {
			firstErr = err
		}
		results = append(results, res)
	}
	if err := mrr.Close(); err != nil {
		if firstErr == nil {
			firstErr = err
		}
		if !hasError(results) {
			results = append(results, explorer.StatementResult{Error: describeError(err)})
		}
	}
	observability.EndSQL(ctx, nil, span, start, ev, &totalRows, firstErr)
	return results
}

func (t *pgTarget) RunInTx(ctx context.Context, statements []string, maxRows int) []explorer.StatementResult {
	t.resetTx(ctx)
	tx, err := t.conn.Begin(ctx)
	if err != nil {
		return []explorer.StatementResult{{Error: describeError(err)}}
	}
	var results []explorer.StatementResult
	for _, stmt := range statements {
		ev := t.sqlEvent(stmt, t.readOnly)
		stmtCtx, span, start := observability.StartSQL(ctx, nil, ev)
		res, rc, execErr := collectWithMeta(tx.Conn().PgConn().ExecParams(stmtCtx, stmt, nil, nil, nil, nil), maxRows)
		observability.EndSQL(stmtCtx, nil, span, start, ev, &rc, execErr)
		results = append(results, res)
		if res.Error != "" {
			rbErr := tx.Rollback(context.WithoutCancel(ctx))
			observability.LogTxOutcome(ctx, nil, ev, false, rbErr)
			return results
		}
	}
	txEv := t.sqlEvent("COMMIT", t.readOnly)
	if err := tx.Commit(ctx); err != nil {
		observability.LogTxOutcome(ctx, nil, txEv, true, err)
		rbErr := tx.Rollback(context.WithoutCancel(ctx))
		observability.LogTxOutcome(ctx, nil, t.sqlEvent("ROLLBACK", t.readOnly), false, rbErr)
		results = append(results, explorer.StatementResult{Error: describeError(err)})
		return results
	}
	observability.LogTxOutcome(ctx, nil, txEv, true, nil)
	return results
}

func (t *pgTarget) ApplyVersionedMigration(ctx context.Context, migration explorer.Migration, userID int64, maxRows int) ([]explorer.StatementResult, bool, error) {
	if migration.ID <= 0 || migration.Version <= 0 || userID <= 0 {
		return nil, false, admin.ErrInvalidInput
	}
	if explorer.HasTransactionControl(migration.SQL) {
		return []explorer.StatementResult{{Error: "Perintah transaksi tidak boleh berada di dalam SQL migrasi."}}, false, nil
	}
	t.resetTx(ctx)
	tx, err := t.conn.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7310002)`); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public._dbmp_migrations (
			version BIGINT PRIMARY KEY,
			migration_id BIGINT NOT NULL UNIQUE,
			sql_text TEXT NOT NULL,
			executed_by BIGINT NOT NULL,
			executed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			status TEXT NOT NULL DEFAULT 'APPLIED' CHECK (status = 'APPLIED')
		)`); err != nil {
		return nil, false, err
	}
	var existing int64
	err = tx.QueryRow(ctx, `SELECT version FROM public._dbmp_migrations WHERE migration_id = $1`, migration.ID).Scan(&existing)
	if err == nil {
		if existing != migration.Version {
			return nil, false, admin.ErrConflict
		}
		return nil, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM public._dbmp_migrations`).Scan(&next); err != nil {
		return nil, false, err
	}
	if next != migration.Version {
		return nil, false, admin.ErrConflict
	}
	migEv := t.sqlEvent(migration.SQL, false)
	migEv.MigrationID = migration.ID
	migCtx, span, start := observability.StartSQL(ctx, nil, migEv)
	var results []explorer.StatementResult
	var totalRows int64
	var firstErr error
	mrr := tx.Conn().PgConn().Exec(migCtx, migration.SQL)
	for mrr.NextResult() {
		res, rc, err := collectWithMeta(mrr.ResultReader(), maxRows)
		totalRows += rc
		if err != nil && firstErr == nil {
			firstErr = err
		}
		results = append(results, res)
	}
	if err := mrr.Close(); err != nil {
		if firstErr == nil {
			firstErr = err
		}
		if !hasError(results) {
			results = append(results, explorer.StatementResult{Error: describeError(err)})
		}
	}
	observability.EndSQL(migCtx, nil, span, start, migEv, &totalRows, firstErr)
	if hasError(results) {
		rbErr := tx.Rollback(context.WithoutCancel(migCtx))
		observability.LogTxOutcome(migCtx, nil, migEv, false, rbErr)
		return results, false, nil
	}
	if tx.Conn().PgConn().TxStatus() != 'T' {
		rbErr := tx.Rollback(context.WithoutCancel(migCtx))
		observability.LogTxOutcome(migCtx, nil, migEv, false, rbErr)
		return append(results, explorer.StatementResult{Error: "SQL migrasi mengakhiri transaksi."}), false, nil
	}
	if _, err := tx.Exec(migCtx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by)
		VALUES ($1, $2, $3, $4)`, migration.Version, migration.ID, migration.SQL, userID); err != nil {
		rbErr := tx.Rollback(context.WithoutCancel(migCtx))
		observability.LogTxOutcome(migCtx, nil, migEv, false, rbErr)
		return nil, false, err
	}
	if err := tx.Commit(migCtx); err != nil {
		observability.LogTxOutcome(migCtx, nil, migEv, true, err)
		return nil, false, err
	}
	observability.LogTxOutcome(migCtx, nil, migEv, true, nil)
	return results, false, nil
}

func (t *pgTarget) resetTx(ctx context.Context) {
	if t.conn.PgConn().TxStatus() != 'I' {
		_, _ = t.conn.Exec(ctx, "ROLLBACK")
	}
}

func collect(rr *pgconn.ResultReader, maxRows int) explorer.StatementResult {
	res, _, _ := collectWithMeta(rr, maxRows)
	return res
}

func collectWithMeta(rr *pgconn.ResultReader, maxRows int) (explorer.StatementResult, int64, error) {
	var result explorer.StatementResult
	for _, field := range rr.FieldDescriptions() {
		result.Columns = append(result.Columns, field.Name)
	}
	var seenRows int64
	for rr.NextRow() {
		seenRows++
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			continue
		}
		values := rr.Values()
		row := make([]explorer.Cell, len(values))
		for i, v := range values {
			if v == nil {
				row[i] = explorer.Cell{Null: true}
			} else {
				row[i] = explorer.Cell{Value: string(v)}
			}
		}
		result.Rows = append(result.Rows, row)
	}
	tag, err := rr.Close()
	if err != nil {
		result.Error = describeError(err)
		return result, seenRows, err
	}
	result.Command = tag.String()
	rowCount := tag.RowsAffected()
	if rowCount == 0 && seenRows > 0 {
		rowCount = seenRows
	}
	return result, rowCount, nil
}

func hasError(results []explorer.StatementResult) bool {
	for _, r := range results {
		if r.Error != "" {
			return true
		}
	}
	return false
}

func describeError(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s (SQLSTATE %s)", pgErr.Severity, pgErr.Message, pgErr.Code)
		if pgErr.Detail != "" {
			b.WriteString("\nDETAIL: " + pgErr.Detail)
		}
		if pgErr.Hint != "" {
			b.WriteString("\nHINT: " + pgErr.Hint)
		}
		if pgErr.Position > 0 {
			fmt.Fprintf(&b, "\nPOSITION: %d", pgErr.Position)
		}
		return b.String()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "Query dibatalkan: melebihi batas waktu."
	}
	return "Query gagal dijalankan."
}

func (s *AdminPostgresStore) CreateMigration(ctx context.Context, in explorer.MigrationInput) (explorer.Migration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return explorer.Migration{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, in.DatabaseID); err != nil {
		return explorer.Migration{}, adminError(err)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = migrationTitle(in.SQL)
	}
	var m explorer.Migration
	err = tx.QueryRowContext(ctx, `
		INSERT INTO migrations (database_id, version, title, sql, status, created_by)
		SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3, 'DRAFT', $4
		FROM migrations WHERE database_id = $1
		RETURNING id, database_id, version, title, sql, status, created_at`,
		in.DatabaseID, title, in.SQL, in.UserID).
		Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Title, &m.SQL, &m.Status, &m.CreatedAt)
	if err != nil {
		return explorer.Migration{}, adminError(err)
	}
	if err := tx.Commit(); err != nil {
		return explorer.Migration{}, err
	}
	return m, nil
}

func (s *AdminPostgresStore) UpdateMigration(ctx context.Context, in explorer.MigrationInput) (explorer.Migration, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = migrationTitle(in.SQL)
	}
	var m explorer.Migration
	err := s.db.QueryRowContext(ctx, `
		UPDATE migrations
		SET title = $3, sql = $4, updated_at = now()
		WHERE database_id = $1 AND version = $2 AND status <> 'SUCCESS'
		  AND NOT EXISTS (SELECT 1 FROM migration_attempts a WHERE a.migration_id = migrations.id AND a.status = 'RUNNING')
		RETURNING id, database_id, version, title, sql, status, COALESCE(error_message, ''), COALESCE(duration_ms, 0), created_at`,
		in.DatabaseID, in.Version, title, in.SQL).
		Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Title, &m.SQL, &m.Status, &m.Error, &m.DurationMs, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		chkErr := s.db.QueryRowContext(ctx, `SELECT status FROM migrations WHERE database_id = $1 AND version = $2`, in.DatabaseID, in.Version).Scan(&status)
		if errors.Is(chkErr, sql.ErrNoRows) {
			return explorer.Migration{}, admin.ErrNotFound
		}
		if chkErr == nil {
			return explorer.Migration{}, admin.ErrConflict
		}
	}
	if err != nil {
		return explorer.Migration{}, adminError(err)
	}
	return m, nil
}

func (s *AdminPostgresStore) GetMigration(ctx context.Context, dbID, version int64) (explorer.Migration, error) {
	var m explorer.Migration
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.database_id, m.version, m.title, m.sql, m.status,
		       COALESCE(m.error_message, ''), COALESCE(m.duration_ms, 0),
		       COALESCE(cu.username, ''), m.created_at,
		       COALESCE(eu.username, ''), COALESCE(m.executed_at, m.created_at)
		FROM migrations m
		LEFT JOIN users cu ON cu.id = m.created_by
		LEFT JOIN users eu ON eu.id = m.executed_by
		WHERE m.database_id = $1 AND m.version = $2`, dbID, version).
		Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Title, &m.SQL, &m.Status,
			&m.Error, &m.DurationMs, &m.CreatedBy, &m.CreatedAt, &m.ExecutedBy, &m.ExecutedAt)
	if err != nil {
		return explorer.Migration{}, adminError(err)
	}
	return m, nil
}

func (s *AdminPostgresStore) BeginMigrationAttempt(ctx context.Context, migration explorer.Migration, userID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var currentSQL, status string
	err = tx.QueryRowContext(ctx, `
		SELECT sql, status FROM migrations WHERE id = $1 AND database_id = $2 FOR UPDATE`, migration.ID, migration.DatabaseID).
		Scan(&currentSQL, &status)
	if err != nil {
		return 0, adminError(err)
	}
	if status == "SUCCESS" || currentSQL != migration.SQL {
		return 0, admin.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE migration_attempts
		SET status = 'UNKNOWN', error_message = 'Previous execution outcome requires reconciliation', finished_at = now()
		WHERE migration_id = $1 AND status = 'RUNNING' AND started_at < now() - interval '30 seconds'`, migration.ID); err != nil {
		return 0, err
	}
	var running bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM migration_attempts WHERE migration_id = $1 AND status = 'RUNNING')`, migration.ID).Scan(&running); err != nil {
		return 0, err
	}
	if running {
		return 0, admin.ErrConflict
	}
	var attemptID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO migration_attempts (migration_id, database_id, user_id, sql_snapshot)
		VALUES ($1, $2, $3, $4) RETURNING id`, migration.ID, migration.DatabaseID, userID, migration.SQL).Scan(&attemptID)
	if err != nil {
		return 0, adminError(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return attemptID, nil
}

func (s *AdminPostgresStore) FinishMigrationAttempt(ctx context.Context, attemptID int64, exec explorer.MigrationExecution) error {
	return s.recordMigrationExecution(ctx, attemptID, exec)
}

func (s *AdminPostgresStore) RecordMigrationExecution(ctx context.Context, exec explorer.MigrationExecution) error {
	return s.recordMigrationExecution(ctx, 0, exec)
}

func (s *AdminPostgresStore) recordMigrationExecution(ctx context.Context, attemptID int64, exec explorer.MigrationExecution) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var errMsg any
	if exec.Error != "" {
		errMsg = exec.Error
	}
	if attemptID > 0 {
		result, err := tx.ExecContext(ctx, `
			UPDATE migration_attempts
			SET status = $2, error_message = $3, finished_at = now()
			WHERE id = $1 AND migration_id = $4 AND database_id = $5 AND status = 'RUNNING'`,
			attemptID, exec.Status, errMsg, exec.Migration.ID, exec.DatabaseID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return admin.ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE migrations
		SET status = $3, error_message = $4, duration_ms = $5, executed_by = $6, executed_at = now(), updated_at = now()
		WHERE database_id = $1 AND version = $2 AND status <> 'SUCCESS'`,
		exec.DatabaseID, exec.Migration.Version, exec.Status, errMsg, exec.DurationMs, exec.UserID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO database_change_log (database_id, migration_id, migration_version, user_id, source, sql, status, error_message, duration_ms)
		VALUES ($1, $2, $3, $4, 'MIGRATION', $5, $6, $7, $8)`,
		exec.DatabaseID, exec.Migration.ID, exec.Migration.Version, exec.UserID, exec.Migration.SQL, exec.Status, errMsg, exec.DurationMs)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) RecordMigration(ctx context.Context, r explorer.MigrationRecord) error {
	var errMsg any
	if r.Error != "" {
		errMsg = r.Error
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO database_change_log (database_id, user_id, source, sql, status, error_message, duration_ms)
		VALUES ($1, $2, 'SQL_EDITOR', $3, $4, $5, $6)`,
		r.DatabaseID, r.UserID, r.SQL, r.Status, errMsg, r.DurationMs)
	return err
}

func (s *AdminPostgresStore) RecentMigrations(ctx context.Context, dbID int64, limit int) ([]explorer.Migration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.database_id, m.version, m.title, m.sql, m.status,
		       COALESCE(m.error_message, ''), COALESCE(m.duration_ms, 0),
		       COALESCE(cu.username, ''), m.created_at,
		       COALESCE(eu.username, ''), COALESCE(m.executed_at, m.created_at)
		FROM migrations m
		LEFT JOIN users cu ON cu.id = m.created_by
		LEFT JOIN users eu ON eu.id = m.executed_by
		WHERE m.database_id = $1 ORDER BY m.version DESC LIMIT $2`, dbID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var migrations []explorer.Migration
	for rows.Next() {
		var m explorer.Migration
		if err := rows.Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Title, &m.SQL, &m.Status,
			&m.Error, &m.DurationMs, &m.CreatedBy, &m.CreatedAt, &m.ExecutedBy, &m.ExecutedAt); err != nil {
			return nil, err
		}
		migrations = append(migrations, m)
	}
	return migrations, rows.Err()
}

func (s *AdminPostgresStore) RecentChanges(ctx context.Context, dbID int64, limit int) ([]explorer.ChangeLog, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, COALESCE(c.migration_version, 0), c.source, c.sql, c.status,
		       COALESCE(c.error_message, ''), c.duration_ms, COALESCE(u.username, ''), c.created_at
		FROM database_change_log c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE c.database_id = $1 ORDER BY c.created_at DESC, c.id DESC LIMIT $2`, dbID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []explorer.ChangeLog
	for rows.Next() {
		var c explorer.ChangeLog
		if err := rows.Scan(&c.ID, &c.MigrationVersion, &c.Source, &c.SQL, &c.Status,
			&c.Error, &c.DurationMs, &c.Username, &c.CreatedAt); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

func migrationTitle(sql string) string {
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > 80 {
				return string(r[:77]) + "..."
			}
			return line
		}
	}
	return ""
}
