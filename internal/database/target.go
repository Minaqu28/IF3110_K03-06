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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	targetConnectTimeout   = 5 * time.Second
	targetStatementTimeout = "10s"
)

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
	var (
		c         domain.Connection
		encrypted []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT `+connectionColumns+`, password_encrypted
		FROM database_connections WHERE id = $1`, dbID).
		Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DBname, &c.DBuser, &c.SSLMode, &encrypted)
	if err != nil {
		return nil, adminError(err)
	}
	password, err := s.DecryptPassword(encrypted)
	if err != nil {
		return nil, &explorer.ConnectError{Reason: "kredensial tersimpan tidak valid.", Err: err}
	}
	config, err := pgx.ParseConfig(TargetDSN(c, password))
	if err != nil {
		return nil, &explorer.ConnectError{Reason: "konfigurasi koneksi database tidak valid.", Err: err}
	}
	config.ConnectTimeout = targetConnectTimeout
	config.RuntimeParams["application_name"] = "if3110-explorer"
	config.RuntimeParams["statement_timeout"] = targetStatementTimeout
	if readOnly {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, describeConnectError(err)
	}
	return &pgTarget{conn: conn, readOnly: readOnly}, nil
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
}

func (t *pgTarget) Close(ctx context.Context) error { return t.conn.Close(ctx) }

func (t *pgTarget) Tables(ctx context.Context) ([]explorer.Table, error) {
	return t.TablesInSchema(ctx, "public")
}

func (t *pgTarget) Schemas(ctx context.Context) ([]string, error) {
	t.resetTx(ctx)
	rows, err := t.conn.Query(ctx, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name <> 'information_schema' AND schema_name NOT LIKE 'pg_%'
		ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var schema string
		err := row.Scan(&schema)
		return schema, err
	})
}

func (t *pgTarget) TablesInSchema(ctx context.Context, schema string) ([]explorer.Table, error) {
	t.resetTx(ctx)
	rows, err := t.conn.Query(ctx, `
		SELECT table_name, table_type FROM information_schema.tables
		WHERE table_schema = $1 ORDER BY table_name`, schema)
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Table, error) {
		table := explorer.Table{Schema: schema}
		err := row.Scan(&table.Name, &table.Type)
		return table, err
	})
	if err != nil {
		return nil, err
	}
	for i := range tables {
		ident := pgx.Identifier{schema, tables[i].Name}.Sanitize()
		if err := t.conn.QueryRow(ctx, `SELECT count(*) FROM `+ident).Scan(&tables[i].Rows); err != nil {
			tables[i].Rows = -1
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
	rows, err := t.conn.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, schema, table)
	if err != nil {
		return details, err
	}
	details.Columns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Column, error) {
		var column explorer.Column
		err := row.Scan(&column.Name, &column.Type, &column.Nullable, &column.Default)
		return column, err
	})
	if err != nil {
		return details, err
	}
	rows, err = t.conn.Query(ctx, `
		SELECT con.conname, con.contype::text, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY con.conname`, schema, table)
	if err != nil {
		return details, err
	}
	details.Constraints, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Constraint, error) {
		var constraint explorer.Constraint
		err := row.Scan(&constraint.Name, &constraint.Type, &constraint.Definition)
		return constraint, err
	})
	if err != nil {
		return details, err
	}
	rows, err = t.conn.Query(ctx, `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2
		ORDER BY indexname`, schema, table)
	if err != nil {
		return details, err
	}
	details.Indexes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Index, error) {
		var index explorer.Index
		err := row.Scan(&index.Name, &index.Definition)
		return index, err
	})
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
	result := collect(t.conn.PgConn().ExecParams(ctx, query, nil, nil, nil, nil), limit)
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
	pg := t.conn.PgConn()
	if t.readOnly {
		return []explorer.StatementResult{collect(pg.ExecParams(ctx, sql, nil, nil, nil, nil), maxRows)}
	}
	var results []explorer.StatementResult
	mrr := pg.Exec(ctx, sql)
	for mrr.NextResult() {
		results = append(results, collect(mrr.ResultReader(), maxRows))
	}
	if err := mrr.Close(); err != nil && !hasError(results) {
		results = append(results, explorer.StatementResult{Error: describeError(err)})
	}
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
		res := collect(tx.Conn().PgConn().ExecParams(ctx, stmt, nil, nil, nil, nil), maxRows)
		results = append(results, res)
		if res.Error != "" {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return results
		}
	}
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		results = append(results, explorer.StatementResult{Error: describeError(err)})
	}
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
	var results []explorer.StatementResult
	mrr := tx.Conn().PgConn().Exec(ctx, migration.SQL)
	for mrr.NextResult() {
		results = append(results, collect(mrr.ResultReader(), maxRows))
	}
	if err := mrr.Close(); err != nil && !hasError(results) {
		results = append(results, explorer.StatementResult{Error: describeError(err)})
	}
	if hasError(results) {
		return results, false, nil
	}
	if tx.Conn().PgConn().TxStatus() != 'T' {
		return append(results, explorer.StatementResult{Error: "SQL migrasi mengakhiri transaksi."}), false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by)
		VALUES ($1, $2, $3, $4)`, migration.Version, migration.ID, migration.SQL, userID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return results, false, nil
}

func (t *pgTarget) resetTx(ctx context.Context) {
	if t.conn.PgConn().TxStatus() != 'I' {
		_, _ = t.conn.Exec(ctx, "ROLLBACK")
	}
}

func collect(rr *pgconn.ResultReader, maxRows int) explorer.StatementResult {
	var result explorer.StatementResult
	for _, field := range rr.FieldDescriptions() {
		result.Columns = append(result.Columns, field.Name)
	}
	for rr.NextRow() {
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
		return result
	}
	result.Command = tag.String()
	return result
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
