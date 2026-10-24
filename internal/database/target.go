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
	_ explorer.TransactionalTarget = (*pgTarget)(nil)
)

// TargetDSN builds a postgres:// URL for a managed connection. Credentials and
// database name are URL-escaped so passwords containing '@', '/', ':' etc. work.
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

// OpenTarget decrypts the stored credential and opens a single connection to the
// managed database. Read-only connections run every transaction as READ ONLY and
// only accept one statement per query (extended protocol).
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

// describeConnectError classifies connection and authentication failures into
// safe, human-readable reasons without leaking sensitive credentials.
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
	t.resetTx(ctx)
	rows, err := t.conn.Query(ctx, `
		SELECT table_name, table_type FROM information_schema.tables
		WHERE table_schema = 'public' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (explorer.Table, error) {
		var table explorer.Table
		err := row.Scan(&table.Name, &table.Type)
		return table, err
	})
	if err != nil {
		return nil, err
	}
	for i := range tables {
		ident := pgx.Identifier{"public", tables[i].Name}.Sanitize()
		if err := t.conn.QueryRow(ctx, `SELECT count(*) FROM `+ident).Scan(&tables[i].Rows); err != nil {
			tables[i].Rows = -1
		}
	}
	return tables, nil
}

func (t *pgTarget) Preview(ctx context.Context, table string, limit int) (explorer.StatementResult, error) {
	t.resetTx(ctx)
	var exists bool
	err := t.conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
	if err != nil {
		return explorer.StatementResult{}, err
	}
	if !exists {
		return explorer.StatementResult{}, explorer.ErrTableNotFound
	}
	query := `SELECT * FROM ` + pgx.Identifier{"public", table}.Sanitize() + ` LIMIT ` + strconv.Itoa(limit)
	result := collect(t.conn.PgConn().ExecParams(ctx, query, nil, nil, nil, nil), limit)
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

// Run executes user SQL. Read-only targets use the extended protocol, which the
// server restricts to exactly one statement. Editable targets use the simple
// protocol so multi-statement scripts run as one implicit transaction.
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

// RunInTx executes a slice of migration statements inside an explicit database
// transaction (BEGIN ... COMMIT on success, ROLLBACK on failure).
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

// resetTx rolls back a transaction the user script left open (e.g. BEGIN
// without COMMIT, or an aborted explicit transaction).
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
		RETURNING id, database_id, version, title, sql, status, COALESCE(error_message, ''), COALESCE(duration_ms, 0), created_at`,
		in.DatabaseID, in.Version, title, in.SQL).
		Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Title, &m.SQL, &m.Status, &m.Error, &m.DurationMs, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		chkErr := s.db.QueryRowContext(ctx, `SELECT status FROM migrations WHERE database_id = $1 AND version = $2`, in.DatabaseID, in.Version).Scan(&status)
		if errors.Is(chkErr, sql.ErrNoRows) {
			return explorer.Migration{}, admin.ErrNotFound
		}
		if status == "SUCCESS" {
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

func (s *AdminPostgresStore) RecordMigrationExecution(ctx context.Context, exec explorer.MigrationExecution) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var errMsg any
	if exec.Error != "" {
		errMsg = exec.Error
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE migrations
		SET status = $3, error_message = $4, duration_ms = $5, executed_by = $6, executed_at = now(), updated_at = now()
		WHERE database_id = $1 AND version = $2`,
		exec.DatabaseID, exec.Migration.Version, exec.Status, errMsg, exec.DurationMs, exec.UserID)
	if err != nil {
		return err
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the connection row so concurrent executions get distinct versions.
	if _, err := tx.ExecContext(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, r.DatabaseID); err != nil {
		return err
	}
	var errMsg any
	if r.Error != "" {
		errMsg = r.Error
	}
	var migID, version int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO migrations (database_id, version, title, sql, status, error_message, duration_ms, created_by, executed_by, executed_at)
		SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3, $4, $5, $6, $7, $7, now()
		FROM migrations WHERE database_id = $1
		RETURNING id, version`,
		r.DatabaseID, migrationTitle(r.SQL), r.SQL, r.Status, errMsg, r.DurationMs, r.UserID).
		Scan(&migID, &version)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO database_change_log (database_id, migration_id, migration_version, user_id, source, sql, status, error_message, duration_ms)
		VALUES ($1, $2, $3, $4, 'SQL_EDITOR', $5, $6, $7, $8)`,
		r.DatabaseID, migID, version, r.UserID, r.SQL, r.Status, errMsg, r.DurationMs)
	if err != nil {
		return err
	}
	return tx.Commit()
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

// migrationTitle uses the first non-empty line of the script, capped at 80 runes.
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
