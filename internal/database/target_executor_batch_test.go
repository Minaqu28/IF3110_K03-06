package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

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
	if err := h.platform.QueryRowContext(ctx, `
		INSERT INTO migration_revisions
		    (migration_id, database_id, revision, up_sql, down_sql, reversible, irreversible_reason, status, definition_hash,
		     created_by, locked_at, batch_table, batch_cursor_column, batch_size)
		VALUES ($1, $2, 1, $3, $4, $5, $6, 'APPROVED', $7, $8, now(), $9, $10, $11) RETURNING id`,
		rev.MigrationID, h.databaseID, up, downSQL, down != "", reason, selectOneHash, h.userID,
		batch.Table, batch.CursorColumn, batch.BatchSize).Scan(&rev.ID); err != nil {
		t.Fatal(err)
	}
	var review int64
	if err := h.platform.QueryRowContext(ctx, `
		INSERT INTO review_requests (revision_id, definition_hash, status, requested_by, resolved_at)
		VALUES ($1, $2, 'APPROVED', $3, now()) RETURNING id`, rev.ID, selectOneHash, h.userID).Scan(&review); err != nil {
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
