package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/jackc/pgx/v5"
)

const (
	execA = "aaaaaaaa-0000-4000-8000-000000000001"
	execB = "aaaaaaaa-0000-4000-8000-000000000002"
	execC = "aaaaaaaa-0000-4000-8000-000000000003"
	execD = "aaaaaaaa-0000-4000-8000-000000000004"
)

type targetHarness struct {
	platform   *sql.DB
	store      *database.AdminPostgresStore
	executor   *database.TargetExecutor
	target     *pgx.Conn
	databaseID int64
	userID     int64
}

func newTargetHarness(t *testing.T) *targetHarness {
	t.Helper()
	rootDSN := os.Getenv("TEST_POSTGRES_DSN")
	if rootDSN == "" {
		t.Skip("set TEST_POSTGRES_DSN to run target integration")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dbmp_exec_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := root.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		root.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `DROP DATABASE `+ident+` WITH (FORCE)`)
		_ = root.Close(context.Background())
	})
	config, err := pgx.ParseConfig(rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close(context.Background()) })

	platform := integrationDB(t)
	store, err := database.NewAdminPostgresStore(platform, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.CreateConnection(ctx, domain.Connection{
		Name: "exec target", Host: config.Host, Port: int64(config.Port), DBname: name, DBuser: config.User, SSLMode: "disable",
	}, config.Password)
	if err != nil {
		t.Fatal(err)
	}
	return &targetHarness{
		platform: platform, store: store, target: target, databaseID: conn.ID,
		executor: database.NewTargetExecutor(store, database.WorkerTargetOptions(time.Minute), nil),
		userID:   insertUser(t, platform, "runner", "runner-long-password", false).ID,
	}
}

func (h *targetHarness) migration(t *testing.T, version int, up, down string) domain.MigrationRevision {
	t.Helper()
	var id int64
	if err := h.platform.QueryRowContext(context.Background(), `
		INSERT INTO migrations (database_id, version, sql, created_by) VALUES ($1, $2, $3, $4) RETURNING id`,
		h.databaseID, version, up, h.userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return domain.MigrationRevision{
		ID: id * 10, MigrationID: id, DatabaseID: h.databaseID, Revision: 1,
		UpSQL: up, DownSQL: down, Reversible: down != "",
	}
}

func (h *targetHarness) request(rev domain.MigrationRevision, action domain.MigrationAction, execution string, target string) domain.ExecutionRequest {
	req := domain.ExecutionRequest{
		ExecutionID: execution, DatabaseID: h.databaseID, MigrationID: rev.MigrationID, RevisionID: rev.ID,
		ActorID: h.userID, Action: action, Mode: domain.ExecutionModeNormal,
	}
	if target != "" {
		req.TargetExecutionID = &target
	}
	return req
}

func (h *targetHarness) run(t *testing.T, req domain.ExecutionRequest, rev domain.MigrationRevision) domain.ExecutionResult {
	t.Helper()
	result, err := h.executor.Execute(context.Background(), req, rev)
	if err != nil {
		t.Fatalf("Execute(%s %s): %v", req.Action, req.ExecutionID, err)
	}
	return result
}

func (h *targetHarness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.target.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *targetHarness) ledger(t *testing.T, migrationID int64) (status, execution string) {
	t.Helper()
	if err := h.target.QueryRow(context.Background(), `
		SELECT status, COALESCE(execution_id::text, '') FROM public._dbmp_migrations WHERE migration_id = $1`, migrationID).
		Scan(&status, &execution); err != nil {
		t.Fatal(err)
	}
	return status, execution
}

func TestTargetExecutorApplyPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `CREATE TABLE items (id int); INSERT INTO items VALUES (1), (2), (3)`, "")
	req := h.request(rev, domain.MigrationActionApply, execA, "")

	result := h.run(t, req, rev)
	if result.Status != domain.ExecutionStatusSucceeded || result.ExecutionID != execA || result.AffectedRows != 3 ||
		result.StartedAt.IsZero() || result.FinishedAt == nil || result.ErrorCode != "" {
		t.Fatalf("result = %+v", result)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}

	again := h.run(t, req, rev)
	if again.Status != domain.ExecutionStatusSucceeded || h.count(t, `SELECT count(*) FROM items`) != 3 {
		t.Fatalf("replay = %+v; the SQL must not run twice", again)
	}

	other := h.run(t, h.request(rev, domain.MigrationActionApply, execB, ""), rev)
	if other.Status != domain.ExecutionStatusFailed || other.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("second execution of an applied migration = %+v", other)
	}
	if h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 1 {
		t.Fatal("a refused apply wrote a ledger row")
	}
}

func TestTargetExecutorApplyFailuresPostgres(t *testing.T) {
	h := newTargetHarness(t)
	broken := h.migration(t, 1, `CREATE TABLE half (id int); SELECT * FROM missing_table`, "")
	result := h.run(t, h.request(broken, domain.MigrationActionApply, execA, ""), broken)
	if result.Status != domain.ExecutionStatusFailed || result.ErrorCode != "42P01" || result.ErrorMessage == "" || result.FinishedAt == nil {
		t.Fatalf("sql error result = %+v", result)
	}
	if h.count(t, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'half'`) != 0 ||
		h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a failed apply left changes behind")
	}

	tcl := h.migration(t, 2, `CREATE TABLE x (id int); COMMIT`, "")
	if r := h.run(t, h.request(tcl, domain.MigrationActionApply, execB, ""), tcl); r.ErrorCode != database.ErrorCodeTransactionControl {
		t.Fatalf("transaction control result = %+v", r)
	}

	fixed := broken
	fixed.UpSQL = `CREATE TABLE half (id int)`
	if r := h.run(t, h.request(fixed, domain.MigrationActionApply, execC, ""), fixed); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("a new execution after a failure = %+v", r)
	}

	ctx := context.Background()
	batch := h.request(fixed, domain.MigrationActionApply, execD, "")
	batch.Mode = domain.ExecutionModeBatch
	if _, err := h.executor.Execute(ctx, batch, fixed); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("batch mode: err = %v, want invalid input", err)
	}
	mismatched := h.request(fixed, domain.MigrationActionApply, execD, "")
	mismatched.RevisionID++
	if _, err := h.executor.Execute(ctx, mismatched, fixed); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("revision mismatch: err = %v, want invalid input", err)
	}
	unknown := fixed
	unknown.MigrationID += 1000
	if _, err := h.executor.Execute(ctx, h.request(unknown, domain.MigrationActionApply, execD, ""), unknown); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown migration: err = %v, want not found", err)
	}
}

func TestTargetExecutorConcurrentReplayPostgres(t *testing.T) {
	h := newTargetHarness(t)
	if _, err := h.target.Exec(context.Background(), `CREATE TABLE hits (n int)`); err != nil {
		t.Fatal(err)
	}
	rev := h.migration(t, 1, `INSERT INTO hits VALUES (1)`, "")
	req := h.request(rev, domain.MigrationActionApply, execA, "")
	var wg sync.WaitGroup
	results := make(chan domain.ExecutionResult, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := h.executor.Execute(context.Background(), req, rev)
			if err != nil {
				t.Error(err)
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.Status != domain.ExecutionStatusSucceeded {
			t.Fatalf("concurrent delivery = %+v", r)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM hits`); n != 1 {
		t.Fatalf("the migration ran %d times, want once", n)
	}
}

const batchUp = `UPDATE public.orders SET v = v + 100 / d WHERE id > $1 AND id <= $2`

func (h *targetHarness) seedOrders(t *testing.T, rows int) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `CREATE TABLE public.orders (id bigint PRIMARY KEY, d int NOT NULL DEFAULT 1, v int NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.target.Exec(ctx, `INSERT INTO public.orders (id) SELECT g FROM generate_series(1, $1::int) g`, rows); err != nil {
		t.Fatal(err)
	}
}

func (h *targetHarness) batchExecution(t *testing.T, version int, up string, batch domain.BatchConfig, scopeMin, scopeMax int64) (domain.MigrationRevision, domain.ExecutionRequest) {
	t.Helper()
	return h.reversibleBatchExecution(t, version, up, "", batch, scopeMin, scopeMax)
}

func (h *targetHarness) reversibleBatchExecution(t *testing.T, version int, up, down string, batch domain.BatchConfig, scopeMin, scopeMax int64) (domain.MigrationRevision, domain.ExecutionRequest) {
	t.Helper()
	ctx := context.Background()
	rev := h.migration(t, version, up, down)
	rev.Batch = &batch
	var downSQL, reason any
	if down != "" {
		downSQL = down
	} else {
		reason, rev.IrreversibleReason = "test", "test"
	}
	rev.DefinitionHash = domain.ComputeDefinitionHash(rev)
	if err := h.platform.QueryRowContext(ctx, `
		INSERT INTO migration_revisions
		    (migration_id, database_id, revision, up_sql, down_sql, reversible, irreversible_reason, status, definition_hash,
		     created_by, locked_at, batch_table, batch_cursor_column, batch_size)
		VALUES ($1, $2, 1, $3, $4, $5, $6, 'APPROVED', $7, $8, now(), $9, $10, $11) RETURNING id`,
		rev.MigrationID, h.databaseID, up, downSQL, down != "", reason, rev.DefinitionHash, h.userID,
		batch.Table, batch.CursorColumn, batch.BatchSize).Scan(&rev.ID); err != nil {
		t.Fatal(err)
	}
	var review int64
	if err := h.platform.QueryRowContext(ctx, `
		INSERT INTO review_requests (revision_id, definition_hash, status, requested_by, resolved_at)
		VALUES ($1, $2, 'APPROVED', $3, now()) RETURNING id`, rev.ID, rev.DefinitionHash, h.userID).Scan(&review); err != nil {
		t.Fatal(err)
	}
	req := h.request(rev, domain.MigrationActionApply, "", "")
	req.Mode, req.ApprovalRequestID = domain.ExecutionModeBatch, review
	if err := h.platform.QueryRowContext(ctx, `
		INSERT INTO migration_executions
		    (database_id, migration_id, revision_id, approval_request_id, action, mode, scope_min, scope_max, requested_by)
		VALUES ($1, $2, $3, $4, 'APPLY', 'BATCH', $5, $6, $7) RETURNING id`,
		h.databaseID, rev.MigrationID, rev.ID, review, scopeMin, scopeMax, h.userID).Scan(&req.ExecutionID); err != nil {
		t.Fatal(err)
	}
	return rev, req
}

func (h *targetHarness) ordersChanged(t *testing.T) (changed, sum int) {
	t.Helper()
	if err := h.target.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE v > 0), coalesce(sum(v), 0) FROM public.orders`).Scan(&changed, &sum); err != nil {
		t.Fatal(err)
	}
	return changed, sum
}

func TestTargetExecutorBatchPartialThenResumePostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 10000)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 6500`); err != nil {
		t.Fatal(err)
	}
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "public.orders", CursorColumn: "id", BatchSize: 1000}, 0, 10000)
	if _, err := h.target.Exec(ctx, `INSERT INTO public.orders (id) VALUES (10001)`); err != nil {
		t.Fatal(err)
	}

	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusPartial || r.ErrorCode != "22012" || r.AppliedBatches != 6 || r.AffectedRows != 6000 {
		t.Fatalf("first attempt = %+v, want PARTIAL after 6 batches", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 6000 ||
		h.count(t, `SELECT count(*) FROM public.orders WHERE id > 6000 AND v <> 0`) != 0 {
		t.Fatalf("%d rows changed; rows 6001-10000 must be untouched", changed)
	}
	if h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a partial run wrote the ledger")
	}
	var firstCommits time.Time
	if err := h.target.QueryRow(ctx, `SELECT max(committed_at) FROM public._dbmp_batch_checkpoints`).Scan(&firstCommits); err != nil {
		t.Fatal(err)
	}

	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 6500`); err != nil {
		t.Fatal(err)
	}
	resumed := h.run(t, req, rev)
	if resumed.Status != domain.ExecutionStatusSucceeded || resumed.AppliedBatches != 10 || resumed.AffectedRows != 10000 || resumed.ErrorCode != "" {
		t.Fatalf("resume = %+v", resumed)
	}
	if changed, sum := h.ordersChanged(t); changed != 10000 || sum != 1000000 {
		t.Fatalf("after resume: %d rows changed, sum %d; every row in scope exactly once", changed, sum)
	}
	if h.count(t, `SELECT count(*) FROM public._dbmp_batch_checkpoints WHERE batch_no <= 6 AND committed_at <= $1`, firstCommits) != 6 ||
		h.count(t, `SELECT count(*) FROM public._dbmp_batch_checkpoints`) != 10 {
		t.Fatal("resume rewrote or skipped checkpoints")
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != req.ExecutionID {
		t.Fatalf("ledger = %s / %s", status, execution)
	}

	again := h.run(t, req, rev)
	if again.Status != domain.ExecutionStatusSucceeded || again.AppliedBatches != 10 || again.AffectedRows != 10000 {
		t.Fatalf("replay after success = %+v", again)
	}
	if _, sum := h.ordersChanged(t); sum != 1000000 {
		t.Fatal("a replay ran batches again")
	}
}

func TestTargetExecutorBatchUncertainCommitThenReconcilePostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	commits := 0
	h.executor.SetBeforeCommit(func(pid uint32) {
		if commits++; commits == 3 {
			_, _ = h.target.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, int64(pid))
		}
	})
	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusUnknown || r.ErrorCode != database.ErrorCodeCommitUncertain || r.AppliedBatches != 2 {
		t.Fatalf("lost batch commit = %+v", r)
	}
	h.executor.SetBeforeCommit(nil)
	resumed := h.run(t, req, rev)
	if resumed.Status != domain.ExecutionStatusSucceeded || resumed.AppliedBatches != 5 {
		t.Fatalf("reconciling resume = %+v", resumed)
	}
	if changed, sum := h.ordersChanged(t); changed != 50 || sum != 5000 {
		t.Fatalf("%d rows changed, sum %d; no batch may commit twice", changed, sum)
	}
}

func TestTargetExecutorBatchEdgeCasesPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 0)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `CREATE TABLE public.loose (id int)`); err != nil {
		t.Fatal(err)
	}

	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 0)
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 0 {
		t.Fatalf("empty scope = %+v", r)
	}
	if status, _ := h.ledger(t, rev.MigrationID); status != "APPLIED" {
		t.Fatal("an empty batch run must still record the migration")
	}

	loose, looseReq := h.batchExecution(t, 2, `UPDATE public.loose SET id = id WHERE id > $1 AND id <= $2`,
		domain.BatchConfig{Table: "loose", CursorColumn: "id", BatchSize: 10}, 0, 10)
	if r := h.run(t, looseReq, loose); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeInvalidBatch {
		t.Fatalf("cursor without a unique index = %+v", r)
	}
	unbound, unboundReq := h.batchExecution(t, 3, `UPDATE public.orders SET v = 1`,
		domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 10)
	if r := h.run(t, unboundReq, unbound); r.ErrorCode != database.ErrorCodeInvalidBatch {
		t.Fatalf("SQL without $1/$2 = %+v", r)
	}

	rollback := unboundReq
	target := req.ExecutionID
	rollback.Action, rollback.TargetExecutionID = domain.MigrationActionRollback, &target
	if r := h.run(t, rollback, unbound); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeIrreversible {
		t.Fatalf("batch rollback of an irreversible revision = %+v", r)
	}
	rollback.TargetExecutionID = nil
	if _, err := h.executor.Execute(ctx, rollback, unbound); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("batch rollback without a target: err = %v, want invalid input", err)
	}
	normal := unboundReq
	normal.Mode = domain.ExecutionModeNormal
	if _, err := h.executor.Execute(ctx, normal, unbound); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("normal mode on a batch revision: err = %v, want invalid input", err)
	}
	unscoped := unboundReq
	unscoped.ExecutionID = "cccccccc-0000-4000-8000-000000000001"
	if _, err := h.executor.Execute(ctx, unscoped, unbound); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown execution: err = %v, want not found", err)
	}
}

func (h *targetHarness) tableExists(t *testing.T, name string) bool {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`, name) == 1
}

func TestTargetExecutorRollbackAndForwardPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `CREATE TABLE gadgets (id int)`, `DROP TABLE gadgets`)
	if r := h.run(t, h.request(rev, domain.MigrationActionApply, execA, ""), rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("apply = %+v", r)
	}

	rollback := h.request(rev, domain.MigrationActionRollback, execB, execA)
	r := h.run(t, rollback, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.RevertsExecutionID == nil || *r.RevertsExecutionID != execA {
		t.Fatalf("rollback = %+v", r)
	}
	if h.tableExists(t, "gadgets") {
		t.Fatal("down SQL did not run")
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execB {
		t.Fatalf("ledger after rollback = %s / %s", status, execution)
	}
	if r := h.run(t, rollback, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("rollback replay = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execC, execA), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rolling back the same apply twice = %+v", r)
	}

	forward := h.request(rev, domain.MigrationActionForward, execC, "")
	if r := h.run(t, forward, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("forward = %+v", r)
	}
	if !h.tableExists(t, "gadgets") {
		t.Fatal("forward did not re-run the up SQL")
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != execC {
		t.Fatalf("ledger after forward = %s / %s", status, execution)
	}
	if r := h.run(t, forward, rev); r.Status != domain.ExecutionStatusSucceeded || !h.tableExists(t, "gadgets") {
		t.Fatalf("forward replay = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionForward, execD, ""), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("forward of an applied migration = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execD, execA), rev); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rollback aimed at the superseded apply = %+v", r)
	}
	if r := h.run(t, h.request(rev, domain.MigrationActionRollback, execD, execC), rev); r.Status != domain.ExecutionStatusSucceeded || h.tableExists(t, "gadgets") {
		t.Fatalf("rollback of the forward = %+v", r)
	}
}

func TestTargetExecutorRollbackRefusalsPostgres(t *testing.T) {
	h := newTargetHarness(t)
	irreversible := h.migration(t, 1, `CREATE TABLE logs (id int)`, "")
	h.run(t, h.request(irreversible, domain.MigrationActionApply, execA, ""), irreversible)
	if r := h.run(t, h.request(irreversible, domain.MigrationActionRollback, execB, execA), irreversible); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeIrreversible {
		t.Fatalf("irreversible rollback = %+v", r)
	}

	other := h.migration(t, 2, `CREATE TABLE notes (id int)`, `DROP TABLE notes`)
	if r := h.run(t, h.request(other, domain.MigrationActionRollback, execB, execA), other); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("rollback aimed at another migration's execution = %+v", r)
	}
	if r := h.run(t, h.request(other, domain.MigrationActionForward, execB, ""), other); r.ErrorCode != database.ErrorCodeLedgerConflict {
		t.Fatalf("forward of a never-applied migration = %+v", r)
	}

	failing := h.migration(t, 3, `CREATE TABLE kept (id int)`, `DROP TABLE nope`)
	h.run(t, h.request(failing, domain.MigrationActionApply, execC, ""), failing)
	if r := h.run(t, h.request(failing, domain.MigrationActionRollback, execD, execC), failing); r.Status != domain.ExecutionStatusFailed || r.ErrorCode != "42P01" {
		t.Fatalf("failing down SQL = %+v", r)
	}
	if status, execution := h.ledger(t, failing.MigrationID); status != "APPLIED" || execution != execC || !h.tableExists(t, "kept") {
		t.Fatalf("a failed rollback changed the ledger: %s / %s", status, execution)
	}
	if status, _ := h.ledger(t, irreversible.MigrationID); status != "APPLIED" {
		t.Fatal("the refused rollback changed the ledger")
	}
}

const batchDown = `UPDATE public.orders SET v = v - 100 / d WHERE id > $1 AND id <= $2`

func revertOf(apply domain.ExecutionRequest, execution string) domain.ExecutionRequest {
	req := apply
	target := apply.ExecutionID
	req.Action, req.ExecutionID, req.TargetExecutionID = domain.MigrationActionRollback, execution, &target
	return req
}

func (h *targetHarness) downOrder(t *testing.T, source string) []int {
	t.Helper()
	rows, err := h.target.Query(context.Background(), `
		SELECT batch_no FROM public._dbmp_batch_checkpoints
		WHERE source_execution_id = $1 AND direction = 'DOWN' ORDER BY committed_at, batch_no`, source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		order = append(order, n)
	}
	return order
}

func TestTargetExecutorBatchRevertInReverseOrderPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	if r := h.run(t, apply, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 {
		t.Fatalf("apply = %+v", r)
	}
	log := &progressLog{}
	h.withProgress(log)

	revert := revertOf(apply, execA)
	r := h.run(t, revert, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 || r.AffectedRows != 50 ||
		r.RevertsExecutionID == nil || *r.RevertsExecutionID != apply.ExecutionID {
		t.Fatalf("revert = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatalf("%d rows still changed after the revert", changed)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3, 2, 1}) {
		t.Fatalf("DOWN checkpoints in order %v, want 5..1", order)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
	completed, cursors := log.summary()
	if !slices.Equal(completed, []int{1, 2, 3, 4, 5}) || cursors[0] != "40" || cursors[4] != "0" {
		t.Fatalf("revert reported %v / %v", completed, cursors)
	}
	for _, c := range log.calls {
		if c.Status != domain.BatchStatusCompensated {
			t.Fatalf("revert progress status = %s", c.Status)
		}
	}

	if again := h.run(t, revert, rev); again.Status != domain.ExecutionStatusSucceeded || again.AppliedBatches != 5 {
		t.Fatalf("revert replay = %+v", again)
	}
	if _, sum := h.ordersChanged(t); sum != 0 {
		t.Fatal("a replayed revert ran down SQL again")
	}
}

func TestTargetExecutorBatchRevertOfAPartialApplyPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	if r := h.run(t, apply, rev); r.Status != domain.ExecutionStatusPartial || r.AppliedBatches != 2 {
		t.Fatalf("apply = %+v", r)
	}
	r := h.run(t, revertOf(apply, execA), rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 2 || r.AffectedRows != 20 {
		t.Fatalf("revert of a partial apply = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a reverted partial apply left changes or a ledger row")
	}
}

func TestTargetExecutorBatchRevertPartialThenResumePostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	h.run(t, apply, rev)
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 15`); err != nil {
		t.Fatal(err)
	}

	revert := revertOf(apply, execA)
	r := h.run(t, revert, rev)
	if r.Status != domain.ExecutionStatusPartiallyReverted || r.ErrorCode != "22012" || r.AppliedBatches != 3 || r.AffectedRows != 30 {
		t.Fatalf("revert failing at batch 2 = %+v, want PARTIALLY_REVERTED after 3 batches", r)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3}) {
		t.Fatalf("DOWN checkpoints %v, want 5, 4, 3", order)
	}
	if status, _ := h.ledger(t, rev.MigrationID); status != "APPLIED" {
		t.Fatal("a partial revert must leave the ledger APPLIED")
	}
	if h.count(t, `SELECT count(*) FROM public.orders WHERE id <= 20 AND v = 100`) != 20 {
		t.Fatal("batches 1 and 2 must stay applied")
	}

	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 15`); err != nil {
		t.Fatal(err)
	}
	log := &progressLog{}
	h.withProgress(log)
	resumed := h.run(t, revert, rev)
	if resumed.Status != domain.ExecutionStatusSucceeded || resumed.AppliedBatches != 5 || resumed.AffectedRows != 50 {
		t.Fatalf("resumed revert = %+v", resumed)
	}
	if order := h.downOrder(t, apply.ExecutionID); !slices.Equal(order, []int{5, 4, 3, 2, 1}) {
		t.Fatalf("DOWN checkpoints after resume %v", order)
	}
	if changed, sum := h.ordersChanged(t); changed != 0 || sum != 0 {
		t.Fatalf("after the resumed revert: %d rows changed, sum %d", changed, sum)
	}
	if completed, _ := log.summary(); !slices.Equal(completed, []int{3, 4, 5}) {
		t.Fatalf("resumed revert reported %v; want the 3 already reverted, then 4 and 5", completed)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "REVERTED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
}

func TestTargetExecutorRefusesUnsafeBatchRevertsPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 30)
	rev, apply := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 30)
	h.run(t, apply, rev)
	later := h.migration(t, 2, `CREATE TABLE later (id int)`, `DROP TABLE later`)
	if r := h.run(t, h.request(later, domain.MigrationActionApply, execB, ""), later); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("later migration = %+v", r)
	}

	refusedWith := func(name string, req domain.ExecutionRequest, rv domain.MigrationRevision) {
		t.Helper()
		r := h.run(t, req, rv)
		if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeUnsafeRevert ||
			!strings.Contains(r.ErrorMessage, migrationengine.ErrUnsafeRevertMessage) || r.AppliedBatches != 0 {
			t.Fatalf("%s = %+v, want an UNSAFE_REVERT refusal", name, r)
		}
		if changed, sum := h.ordersChanged(t); changed != 30 || sum != 3000 {
			t.Fatalf("%s changed the data: %d rows, sum %d", name, changed, sum)
		}
	}

	refusedWith("revert while a later migration is applied", revertOf(apply, execA), rev)

	other := rev
	other.ID++
	wrongRevision := revertOf(apply, execA)
	wrongRevision.RevisionID = other.ID
	refusedWith("revert with another revision's down SQL", wrongRevision, other)

	unknown := revertOf(apply, execA)
	missing := "dddddddd-0000-4000-8000-000000000001"
	unknown.TargetExecutionID = &missing
	refusedWith("revert of an unknown execution", unknown, rev)

	if r := h.run(t, h.request(later, domain.MigrationActionRollback, execC, execB), later); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("rolling back the later migration = %+v", r)
	}
	if r := h.run(t, revertOf(apply, execA), rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 3 {
		t.Fatalf("revert once the later migration is rolled back = %+v", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatalf("%d rows still changed", changed)
	}
}

func TestTargetExecutorRefusesIrreversibleBatchRevertPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 20)
	rev, apply := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 20)
	h.run(t, apply, rev)
	r := h.run(t, revertOf(apply, execA), rev)
	if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != database.ErrorCodeIrreversible || r.ErrorMessage != migrationengine.ErrUnsafeRevertMessage {
		t.Fatalf("irreversible revert = %+v", r)
	}
	if status, _ := h.ledger(t, rev.MigrationID); status != "APPLIED" {
		t.Fatal("a refused revert changed the ledger")
	}
}

func TestTargetExecutorLostCommitIsUnknownPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `CREATE TABLE widgets (id int)`, "")
	req := h.request(rev, domain.MigrationActionApply, execA, "")
	h.executor.SetBeforeCommit(func(pid uint32) {
		var ok bool
		if err := h.target.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, int64(pid)).Scan(&ok); err != nil || !ok {
			t.Errorf("terminate backend %d: ok = %v, err = %v", pid, ok, err)
		}
	})

	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusUnknown || r.ErrorCode != database.ErrorCodeCommitUncertain || r.FinishedAt == nil ||
		!strings.Contains(r.ErrorMessage, migrationengine.ErrCommitUncertain.Error()) {
		t.Fatalf("lost commit = %+v", r)
	}
	if h.tableExists(t, "widgets") || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("the terminated transaction left changes behind")
	}

	h.executor.SetBeforeCommit(nil)
	again := h.run(t, req, rev)
	if again.Status != domain.ExecutionStatusSucceeded || !h.tableExists(t, "widgets") {
		t.Fatalf("reconciling retry = %+v", again)
	}
	if status, execution := h.ledger(t, rev.MigrationID); status != "APPLIED" || execution != execA {
		t.Fatalf("ledger = %s / %s", status, execution)
	}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AffectedRows != 0 {
		t.Fatalf("second retry should only reconcile: %+v", r)
	}
}

func TestTargetExecutorRejectedCommitIsFailedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	rev := h.migration(t, 1, `
		CREATE TABLE parents (id int PRIMARY KEY);
		CREATE TABLE children (parent int REFERENCES parents DEFERRABLE INITIALLY DEFERRED);
		INSERT INTO children VALUES (1)`, "")
	r := h.run(t, h.request(rev, domain.MigrationActionApply, execA, ""), rev)
	if r.Status != domain.ExecutionStatusFailed || r.ErrorCode != "23503" {
		t.Fatalf("a commit the server refused = %+v, want FAILED 23503", r)
	}
	if h.tableExists(t, "parents") || h.count(t, `SELECT count(*) FROM public._dbmp_migrations`) != 0 {
		t.Fatal("a refused commit left changes behind")
	}
}

type progressLog struct {
	mu     sync.Mutex
	ids    []string
	calls  []domain.BatchProgress
	failAt int
}

func (p *progressLog) sink(_ context.Context, executionID string, progress domain.BatchProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, executionID)
	p.calls = append(p.calls, progress)
	if len(p.calls) == p.failAt {
		return errors.New("platform database unavailable")
	}
	return nil
}

func (p *progressLog) summary() (completed []int, cursors []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		completed = append(completed, c.Completed)
		cursors = append(cursors, c.LastCursor)
	}
	return completed, cursors
}

func (h *targetHarness) withProgress(p *progressLog) {
	h.executor = database.NewTargetExecutor(h.store, database.WorkerTargetOptions(time.Minute), p.sink)
}

func TestTargetExecutorReportsEveryCommittedBatchPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	log := &progressLog{}
	h.withProgress(log)

	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusPartial || r.AppliedBatches != 2 {
		t.Fatalf("first attempt = %+v", r)
	}
	completed, cursors := log.summary()
	if !slices.Equal(completed, []int{1, 2}) || cursors[1] != "20" {
		t.Fatalf("first attempt reported %v / %v, want one call per committed batch", completed, cursors)
	}
	for i, c := range log.calls {
		if c.Status != domain.BatchStatusCommitted || c.AffectedRows != int64(10*(i+1)) || log.ids[i] != req.ExecutionID {
			t.Fatalf("call %d = %+v for %s", i, c, log.ids[i])
		}
	}

	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	*log = progressLog{}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("resume = %+v", r)
	}
	completed, cursors = log.summary()
	if !slices.Equal(completed, []int{2, 3, 4, 5}) || cursors[0] != "20" || cursors[3] != "50" {
		t.Fatalf("resume reported %v / %v; want the reconciled checkpoint first, then one call per batch", completed, cursors)
	}
}

func TestTargetExecutorStopsWhenProgressCannotBeSavedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	h.withProgress(&progressLog{failAt: 2})

	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusPartial || r.ErrorCode != database.ErrorCodeInterrupted || r.AppliedBatches != 2 {
		t.Fatalf("progress failure = %+v, want PARTIAL after the batch that could not be reported", r)
	}
	if changed, _ := h.ordersChanged(t); changed != 20 {
		t.Fatalf("%d rows changed; the run must stop right after the failed report", changed)
	}
	h.withProgress(&progressLog{})
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 5 {
		t.Fatalf("resume = %+v", r)
	}
}

func TestBatchScopePostgres(t *testing.T) {
	rootDSN := os.Getenv("TEST_POSTGRES_DSN")
	if rootDSN == "" {
		t.Skip("set TEST_POSTGRES_DSN to run target integration")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dbmp_scope_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := root.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		root.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `DROP DATABASE `+ident+` WITH (FORCE)`)
		_ = root.Close(context.Background())
	})
	config, err := pgx.ParseConfig(rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(ctx, `
		CREATE SCHEMA app;
		CREATE TABLE app."Orders" (id bigint PRIMARY KEY);
		INSERT INTO app."Orders" SELECT g FROM generate_series(5, 20) g;
		CREATE TABLE public.empty (id bigint PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	_ = target.Close(ctx)

	platform := integrationDB(t)
	store, err := database.NewAdminPostgresStore(platform, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.CreateConnection(ctx, domain.Connection{
		Name: "scope target", Host: config.Host, Port: int64(config.Port), DBname: name, DBuser: config.User, SSLMode: "disable",
	}, config.Password)
	if err != nil {
		t.Fatal(err)
	}

	scope, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "app.Orders", CursorColumn: "id", BatchSize: 5})
	if err != nil || scope != (workflow.BatchScope{Min: 4, Max: 20}) {
		t.Fatalf("scope = %+v, err = %v; want 4..20", scope, err)
	}
	empty, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "empty", CursorColumn: "id", BatchSize: 5})
	if err != nil || empty != (workflow.BatchScope{}) {
		t.Fatalf("empty table scope = %+v, err = %v; want 0..0", empty, err)
	}
	for _, bad := range []domain.BatchConfig{
		{Table: "app.missing", CursorColumn: "id"},
		{Table: "app.Orders", CursorColumn: "nope"},
		{Table: "a.b.c", CursorColumn: "id"},
		{Table: "app.", CursorColumn: "id"},
		{Table: "app.Orders", CursorColumn: ""},
		{Table: `app.Orders"; DROP TABLE app."Orders`, CursorColumn: "id"},
	} {
		if _, err := store.BatchScope(ctx, conn.ID, bad); !errors.Is(err, admin.ErrInvalidInput) {
			t.Errorf("BatchScope(%+v): err = %v, want invalid input", bad, err)
		}
	}
	if scope, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "app.Orders", CursorColumn: "id"}); err != nil || scope.Max != 20 {
		t.Fatalf("table survived the injection attempt? scope = %+v, err = %v", scope, err)
	}
}

const targetLockKey = 7310002

func TestTargetExecutorSecondBatchRunOnSameDatabaseIsBusyPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 30)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 30)
	other := database.NewTargetExecutor(h.store, database.WorkerTargetOptions(time.Minute), nil)

	var secondErr error
	calls := 0
	h.executor.SetBeforeCommit(func(uint32) {
		if calls++; calls == 2 {
			_, secondErr = other.Execute(context.Background(), req, rev)
		}
	})
	r := h.run(t, req, rev)
	if r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 3 {
		t.Fatalf("first executor = %+v", r)
	}
	if !errors.Is(secondErr, migrationengine.ErrTargetBusy) || migrationengine.ClassifyError(secondErr) != migrationengine.ErrorTransient {
		t.Fatalf("second executor between batch commits: err = %v, want a transient busy error", secondErr)
	}
	if changed, sum := h.ordersChanged(t); changed != 30 || sum != 3000 {
		t.Fatalf("%d rows changed, sum %d; the second executor must not run a batch", changed, sum)
	}
}

func TestTargetExecutorBusyUntilTheSessionLockIsReleasedPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 20)
	ctx := context.Background()
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 20)
	if _, err := h.target.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(targetLockKey)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := h.executor.Execute(ctx, req, rev); !errors.Is(err, migrationengine.ErrTargetBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("a busy target took %v to report; it must not wait for the lock", waited)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 || h.tableExists(t, "_dbmp_batch_checkpoints") {
		t.Fatal("a busy run touched the target before taking the lock")
	}
	if _, err := h.target.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(targetLockKey)); err != nil {
		t.Fatal(err)
	}
	if r := h.run(t, req, rev); r.Status != domain.ExecutionStatusSucceeded || r.AppliedBatches != 2 {
		t.Fatalf("after release = %+v", r)
	}
	var held int
	if err := h.target.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1`, targetLockKey).Scan(&held); err != nil || held != 0 {
		t.Fatalf("advisory locks still held after the run: %d, err = %v", held, err)
	}
}

func TestEngineRunsAndRecordsEveryAttemptPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 50)
	ctx := context.Background()
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 0 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	_, req := h.reversibleBatchExecution(t, 1, batchUp, batchDown, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 50)
	history := database.NewHistoryPostgresStore(h.platform)
	engine := migrationengine.NewEngine(database.NewRevisionPostgresStore(h.platform), history, h.executor)

	first, err := engine.Execute(ctx, req)
	if err != nil || first.Status != domain.ExecutionStatusPartial || first.AppliedBatches != 2 {
		t.Fatalf("first run = %+v, err = %v", first, err)
	}
	if _, err := h.target.Exec(ctx, `UPDATE public.orders SET d = 1 WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	second, err := engine.Execute(ctx, req)
	if err != nil || second.Status != domain.ExecutionStatusSucceeded || second.AppliedBatches != 5 {
		t.Fatalf("resume = %+v, err = %v", second, err)
	}

	attempts, err := history.ListAttempts(ctx, h.databaseID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
	for i, want := range []domain.ExecutionStatus{domain.ExecutionStatusPartial, domain.ExecutionStatusSucceeded} {
		a := attempts[i]
		if a.ExecutionID != req.ExecutionID || a.AttemptNumber != i+1 || a.Status != want || a.SQLSnapshot != batchUp || a.FinishedAt == nil {
			t.Fatalf("attempt %d = %+v", i+1, a)
		}
	}
	if attempts[0].ErrorCode != "22012" || attempts[1].ErrorCode != "" {
		t.Fatalf("error codes = %q, %q", attempts[0].ErrorCode, attempts[1].ErrorCode)
	}
}

func TestEngineRefusesATamperedRevisionPostgres(t *testing.T) {
	h := newTargetHarness(t)
	h.seedOrders(t, 10)
	rev, req := h.batchExecution(t, 1, batchUp, domain.BatchConfig{Table: "orders", CursorColumn: "id", BatchSize: 10}, 0, 10)
	execSQL(t, h.platform, `ALTER TABLE migration_revisions DISABLE TRIGGER USER`)
	execSQL(t, h.platform, `UPDATE migration_revisions SET up_sql = 'UPDATE public.orders SET v = 999 WHERE id > $1 AND id <= $2' WHERE id = $1`, rev.ID)
	execSQL(t, h.platform, `ALTER TABLE migration_revisions ENABLE TRIGGER USER`)

	history := database.NewHistoryPostgresStore(h.platform)
	engine := migrationengine.NewEngine(database.NewRevisionPostgresStore(h.platform), history, h.executor)
	res, err := engine.Execute(context.Background(), req)
	if err != nil || res.Status != domain.ExecutionStatusFailed || res.ErrorCode != migrationengine.ErrorCodeDefinitionChanged {
		t.Fatalf("tampered revision = %+v, err = %v", res, err)
	}
	if changed, _ := h.ordersChanged(t); changed != 0 {
		t.Fatal("tampered SQL reached the target")
	}
	attempts, err := history.ListAttempts(context.Background(), h.databaseID)
	if err != nil || len(attempts) != 1 || attempts[0].ErrorCode != migrationengine.ErrorCodeDefinitionChanged {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
}
