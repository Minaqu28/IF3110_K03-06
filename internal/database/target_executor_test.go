package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
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
