package database

import (
	"context"
	"strconv"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/jackc/pgx/v5"
)

const (
	viewerStatementTimeout       = 10 * time.Second
	DefaultMigrationBatchTimeout = 5 * time.Minute
	workerLockTimeout            = 10 * time.Second
)

type TargetOptions struct {
	ReadOnly         bool
	ApplicationName  string
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

func ViewerTargetOptions(readOnly bool) TargetOptions {
	return TargetOptions{ReadOnly: readOnly, ApplicationName: "if3110-explorer", StatementTimeout: viewerStatementTimeout}
}

func WorkerTargetOptions(statementTimeout time.Duration) TargetOptions {
	if statementTimeout <= 0 {
		statementTimeout = DefaultMigrationBatchTimeout
	}
	return TargetOptions{ApplicationName: "if3110-migration-worker", StatementTimeout: statementTimeout, LockTimeout: workerLockTimeout}
}

func (o TargetOptions) runtimeParams() map[string]string {
	params := map[string]string{"application_name": o.ApplicationName}
	if o.StatementTimeout > 0 {
		params["statement_timeout"] = durationParam(o.StatementTimeout)
	}
	if o.LockTimeout > 0 {
		params["lock_timeout"] = durationParam(o.LockTimeout)
	}
	if o.ReadOnly {
		params["default_transaction_read_only"] = "on"
	}
	return params
}

func durationParam(d time.Duration) string {
	return strconv.FormatInt(max(d.Milliseconds(), 1), 10) + "ms"
}

func (s *AdminPostgresStore) openTarget(ctx context.Context, dbID int64, opts TargetOptions) (*pgTarget, error) {
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
	for key, value := range opts.runtimeParams() {
		config.RuntimeParams[key] = value
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, describeConnectError(err)
	}
	return &pgTarget{conn: conn, readOnly: opts.ReadOnly}, nil
}
