package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ migrationengine.TargetExecutor = (*TargetExecutor)(nil)

const (
	ErrorCodeLedgerConflict     = "LEDGER_CONFLICT"
	ErrorCodeIrreversible       = "IRREVERSIBLE"
	ErrorCodeUnsafeRevert       = "UNSAFE_REVERT"
	ErrorCodeTransactionControl = "TRANSACTION_CONTROL"
	ErrorCodeTransactionEnded   = "TRANSACTION_ENDED"
	ErrorCodeCommitUncertain    = "COMMIT_UNCERTAIN"
	ErrorCodeInvalidBatch       = "INVALID_BATCH"
	ErrorCodeInterrupted        = "INTERRUPTED"
)

type TargetExecutor struct {
	store    *AdminPostgresStore
	options  TargetOptions
	progress migrationengine.ProgressSink
	now      func() time.Time

	beforeCommit func(pid uint32)
}

func NewTargetExecutor(store *AdminPostgresStore, options TargetOptions, progress migrationengine.ProgressSink) *TargetExecutor {
	return &TargetExecutor{store: store, options: options, progress: progress, now: time.Now}
}

func (e *TargetExecutor) Execute(ctx context.Context, req domain.ExecutionRequest, rev domain.MigrationRevision) (domain.ExecutionResult, error) {
	if req.ExecutionID == "" || !req.Action.Valid() || rev.ID != req.RevisionID || rev.MigrationID != req.MigrationID || rev.DatabaseID != req.DatabaseID {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	batch := req.Mode == domain.ExecutionModeBatch
	if (req.Mode != domain.ExecutionModeNormal && !batch) || batch != (rev.Batch != nil) {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
	started := e.now()
	var version int64
	if err := e.store.db.QueryRowContext(ctx, `SELECT version FROM migrations WHERE id = $1 AND database_id = $2`,
		req.MigrationID, req.DatabaseID).Scan(&version); err != nil {
		return domain.ExecutionResult{}, adminError(err)
	}
	revert := req.Action == domain.MigrationActionRollback
	var scope executionScope
	if batch && !revert {
		var err error
		if scope, err = e.loadScope(ctx, req); err != nil {
			return domain.ExecutionResult{}, err
		}
	}
	target, err := e.store.openTarget(ctx, req.DatabaseID, e.options)
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	defer target.Close(context.WithoutCancel(ctx))
	if batch {
		if err := lockTargetSession(ctx, target.conn); err != nil {
			return domain.ExecutionResult{}, err
		}
	}
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		return domain.ExecutionResult{}, err
	}
	result := domain.ExecutionResult{ExecutionID: req.ExecutionID, RevertsExecutionID: req.TargetExecutionID, StartedAt: started}
	switch {
	case batch && revert:
		result, err = e.runBatchRevert(ctx, target.conn, req, rev, version, result)
	case batch:
		result, err = e.runBatchMode(ctx, target.conn, req, rev, version, scope, result)
	default:
		result, err = e.runNormal(ctx, target.conn, req, rev, version, result)
	}
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	finished := e.now()
	result.FinishedAt = &finished
	return result, nil
}

func refused(result domain.ExecutionResult, code, message string) domain.ExecutionResult {
	result.Status, result.ErrorCode, result.ErrorMessage = domain.ExecutionStatusFailed, code, message
	if result.AppliedBatches > 0 {
		result.Status = partialStatus(result)
	}
	return result
}

func partialStatus(result domain.ExecutionResult) domain.ExecutionStatus {
	if result.RevertsExecutionID != nil {
		return domain.ExecutionStatusPartiallyReverted
	}
	return domain.ExecutionStatusPartial
}

func (e *TargetExecutor) commitResult(ctx context.Context, conn *pgx.Conn, tx pgx.Tx, result domain.ExecutionResult) (domain.ExecutionResult, bool) {
	if e.beforeCommit != nil {
		e.beforeCommit(conn.PgConn().PID())
	}
	if err := tx.Commit(ctx); err != nil {
		if pgErr, ok := commitRejected(err); ok {
			return refused(result, pgErr.Code, describeError(err)), false
		}
		return uncertain(result, err), false
	}
	return result, true
}

func uncertain(result domain.ExecutionResult, err error) domain.ExecutionResult {
	result.Status, result.ErrorCode = domain.ExecutionStatusUnknown, ErrorCodeCommitUncertain
	if !errors.Is(err, migrationengine.ErrCommitUncertain) {
		err = fmt.Errorf("%w: %w", migrationengine.ErrCommitUncertain, err)
	}
	result.ErrorMessage = err.Error()
	return result
}

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
	return &pgTarget{conn: conn, readOnly: opts.ReadOnly, dbID: c.ID, dbName: c.Name}, nil
}

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

type normalPlan struct {
	sql    string
	record func(ctx context.Context, tx pgx.Tx) error
}

type refusal struct {
	code    string
	message string
}

func conflict(message string) refusal {
	return refusal{code: ErrorCodeLedgerConflict, message: message}
}

func (e *TargetExecutor) runNormal(ctx context.Context, conn *pgx.Conn, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64, result domain.ExecutionResult) (domain.ExecutionResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(targetMigrationLockKey)); err != nil {
		return result, err
	}
	var recorded string
	err = tx.QueryRow(ctx, `SELECT status FROM public._dbmp_migrations WHERE execution_id = $1`, req.ExecutionID).Scan(&recorded)
	if err == nil {
		result.Status = domain.ExecutionStatusSucceeded
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	plan, refuse, err := planNormal(ctx, tx, req, rev, version)
	if err != nil {
		return result, err
	}
	if refuse.code != "" {
		return refused(result, refuse.code, refuse.message), nil
	}
	if explorer.HasTransactionControl(plan.sql) {
		return refused(result, ErrorCodeTransactionControl, "transaction control statements are not allowed in migration SQL"), nil
	}
	ev := observability.SQLEvent{
		DatabaseID:  req.DatabaseID,
		Query:       plan.sql,
		MigrationID: req.MigrationID,
		JobID:       req.JobID,
		ExecutionID: req.ExecutionID,
	}
	sqlCtx, span, start := observability.StartSQL(ctx, nil, ev)
	affected, err := runStatements(sqlCtx, tx, plan.sql)
	observability.EndSQL(sqlCtx, nil, span, start, ev, &affected, err)
	if err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return refused(result, pgErr.Code, describeError(err)), nil
		}
		return result, err
	}
	if tx.Conn().PgConn().TxStatus() != 'T' {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, nil)
		return refused(result, ErrorCodeTransactionEnded, "migration SQL ended the transaction"), nil
	}
	if err := plan.record(sqlCtx, tx); err != nil {
		observability.LogTxOutcome(sqlCtx, nil, ev, false, err)
		return result, err
	}
	result, ok := e.commitResult(sqlCtx, conn, tx, result)
	if !ok {
		observability.LogTxOutcome(sqlCtx, nil, ev, true, errors.New(result.ErrorMessage))
		return result, nil
	}
	observability.LogTxOutcome(sqlCtx, nil, ev, true, nil)
	result.Status, result.AffectedRows = domain.ExecutionStatusSucceeded, affected
	return result, nil
}

func planNormal(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64) (normalPlan, refusal, error) {
	switch req.Action {
	case domain.MigrationActionApply:
		return planApply(ctx, tx, req, rev, version)
	case domain.MigrationActionRollback:
		return planRollback(ctx, tx, req, rev)
	case domain.MigrationActionForward:
		return planForward(ctx, tx, req, rev)
	}
	return normalPlan{}, refusal{}, admin.ErrInvalidInput
}

func planApply(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision, version int64) (normalPlan, refusal, error) {
	var taken bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM public._dbmp_migrations WHERE migration_id = $1 OR version = $2)`,
		rev.MigrationID, version).Scan(&taken); err != nil {
		return normalPlan{}, refusal{}, err
	}
	if taken {
		return normalPlan{}, conflict("this migration or its version is already recorded on the target database"), nil
	}
	return normalPlan{sql: rev.UpSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id, status)
			VALUES ($1, $2, $3, $4, $5, 'APPLIED')`, version, rev.MigrationID, rev.UpSQL, req.ActorID, req.ExecutionID)
		return err
	}}, refusal{}, nil
}

func runStatements(ctx context.Context, tx pgx.Tx, sql string) (int64, error) {
	mrr := tx.Conn().PgConn().Exec(ctx, sql)
	var affected int64
	var firstErr error
	for mrr.NextResult() {
		tag, err := mrr.ResultReader().Close()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		affected += tag.RowsAffected()
	}
	if err := mrr.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return affected, firstErr
}

func commitRejected(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil, false
	}
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return pgErr, severity == "ERROR"
}

func planRollback(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision) (normalPlan, refusal, error) {
	if req.TargetExecutionID == nil || *req.TargetExecutionID == "" {
		return normalPlan{}, refusal{}, admin.ErrInvalidInput
	}
	target := *req.TargetExecutionID
	if !rev.Reversible || strings.TrimSpace(rev.DownSQL) == "" {
		return normalPlan{}, refusal{code: ErrorCodeIrreversible, message: "this revision has no safe down SQL"}, nil
	}
	var (
		migrationID int64
		status      string
	)
	err := tx.QueryRow(ctx, `
		SELECT migration_id, status FROM public._dbmp_migrations WHERE execution_id = $1 FOR UPDATE`, target).
		Scan(&migrationID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return normalPlan{}, conflict("the execution to roll back is not the one recorded as applied on the target database"), nil
	}
	if err != nil {
		return normalPlan{}, refusal{}, err
	}
	if migrationID != rev.MigrationID || status != "APPLIED" {
		return normalPlan{}, conflict("the execution to roll back is not currently applied on the target database"), nil
	}
	return normalPlan{sql: rev.DownSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public._dbmp_migrations
			SET status = 'REVERTED', execution_id = $2, executed_by = $3, executed_at = now()
			WHERE execution_id = $1`, target, req.ExecutionID, req.ActorID)
		return err
	}}, refusal{}, nil
}

func planForward(ctx context.Context, tx pgx.Tx, req domain.ExecutionRequest, rev domain.MigrationRevision) (normalPlan, refusal, error) {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT status FROM public._dbmp_migrations WHERE migration_id = $1 FOR UPDATE`, rev.MigrationID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return normalPlan{}, conflict("this migration was never applied on the target database; use APPLY"), nil
	}
	if err != nil {
		return normalPlan{}, refusal{}, err
	}
	if status != "REVERTED" {
		return normalPlan{}, conflict("only a reverted migration can be applied forward again"), nil
	}
	return normalPlan{sql: rev.UpSQL, record: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public._dbmp_migrations
			SET status = 'APPLIED', execution_id = $2, sql_text = $3, executed_by = $4, executed_at = now()
			WHERE migration_id = $1`, rev.MigrationID, req.ExecutionID, rev.UpSQL, req.ActorID)
		return err
	}}, refusal{}, nil
}
