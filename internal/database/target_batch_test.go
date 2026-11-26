package database

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

const batchSource = "bbbbbbbb-0000-4000-8000-000000000001"

func batchFixture(t *testing.T) (*pgTarget, batchSpec) {
	t.Helper()
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatal(err)
	}
	if _, err := target.conn.Exec(ctx, `
		CREATE TABLE items (id bigint PRIMARY KEY, v int NOT NULL DEFAULT 0);
		INSERT INTO items (id) SELECT g FROM generate_series(1, 12) g;`); err != nil {
		t.Fatal(err)
	}
	bt, err := inspectBatchTarget(ctx, target.conn, domain.BatchConfig{Table: "items", CursorColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	return target, batchSpec{
		target: bt, sql: `UPDATE items SET v = v + 1 WHERE id > $1 AND id <= $2`,
		sourceID: batchSource, executionID: batchSource, direction: domain.DirectionUp,
		batchNo: 1, from: 0, scopeMax: 10, size: 4,
	}
}

func itemsChanged(t *testing.T, target *pgTarget) (changed, total int) {
	t.Helper()
	if err := target.conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE v > 0), coalesce(sum(v), 0) FROM items`).Scan(&changed, &total); err != nil {
		t.Fatal(err)
	}
	return changed, total
}

func TestRunBatchCommitsOneBatchWithItsCheckpointIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	ctx := context.Background()
	e := &TargetExecutor{}

	out, err := e.runBatch(ctx, target.conn, spec)
	if err != nil || out.done || out.to != 4 || out.rows != 4 {
		t.Fatalf("first batch = %+v, err = %v", out, err)
	}
	var from, to, rows int64
	if err := target.conn.QueryRow(ctx, `
		SELECT cursor_from, cursor_to, row_count FROM public._dbmp_batch_checkpoints
		WHERE source_execution_id = $1 AND batch_no = 1 AND direction = 'UP'`, batchSource).Scan(&from, &to, &rows); err != nil {
		t.Fatal(err)
	}
	if from != 0 || to != 4 || rows != 4 {
		t.Fatalf("checkpoint = (%d, %d] rows %d", from, to, rows)
	}

	_, err = e.runBatch(ctx, target.conn, spec)
	requireTargetSQLState(t, err, "23505")
	if changed, total := itemsChanged(t, target); changed != 4 || total != 4 {
		t.Fatalf("after replaying batch 1: %d rows changed, sum %d; want the data changed once", changed, total)
	}

	spec.batchNo, spec.from = 2, 4
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || out.to != 8 {
		t.Fatalf("second batch = %+v, err = %v", out, err)
	}
	spec.batchNo, spec.from = 3, 8
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || out.to != 10 || out.rows != 2 {
		t.Fatalf("last batch stops at the scope: %+v, err = %v", out, err)
	}
	spec.batchNo, spec.from = 4, 10
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || !out.done {
		t.Fatalf("past the scope = %+v, err = %v; want done", out, err)
	}
	if changed, _ := itemsChanged(t, target); changed != 10 {
		t.Fatalf("%d rows changed; rows 11 and 12 are outside the scope", changed)
	}
}

func TestRunBatchFailureRollsBackOnlyThatBatchIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	ctx := context.Background()
	e := &TargetExecutor{}
	if _, err := e.runBatch(ctx, target.conn, spec); err != nil {
		t.Fatal(err)
	}
	spec.batchNo, spec.from = 2, 4
	spec.sql = `UPDATE items SET v = v + 1 / (id - 6) WHERE id > $1 AND id <= $2`
	_, err := e.runBatch(ctx, target.conn, spec)
	requireTargetSQLState(t, err, "22012")
	if changed, _ := itemsChanged(t, target); changed != 4 {
		t.Fatalf("%d rows changed; only batch 1 should be committed", changed)
	}
	var checkpoints int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_checkpoints`).Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatalf("checkpoints = %d, err = %v", checkpoints, err)
	}
}

func TestRunBatchLostCommitIsUncertainIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	killer := openTestTarget(t, false)
	e := &TargetExecutor{beforeCommit: func(pid uint32) {
		_, _ = killer.conn.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, int64(pid))
	}}
	_, err := e.runBatch(context.Background(), target.conn, spec)
	if !errors.Is(err, migrationengine.ErrCommitUncertain) {
		t.Fatalf("err = %v, want an uncertain commit", err)
	}
}
