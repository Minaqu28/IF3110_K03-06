package explorer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestIsReadOnlyStatement(t *testing.T) {
	cases := map[string]bool{
		"SELECT 1":             true,
		"  select * from t;  ": true,
		"-- comment\nSELECT 1": true,
		"/* c */ WITH x AS (SELECT 1) SELECT * FROM x": true,
		"(SELECT 1)":               true,
		"EXPLAIN SELECT 1":         true,
		"SELECT ';' AS semi":       true,
		"SELECT $$a;b$$":           true,
		"SELECT 1; -- trailing":    true,
		"SELECT 1; DELETE FROM t":  false,
		"DELETE FROM t":            false,
		"insert into t values (1)": false,
		"DROP TABLE t":             false,
		"ALTER TABLE t ADD c int":  false,
		"UPDATE t SET a = 1":       false,
		"-- SELECT\nDELETE FROM t": false,
		"":                         false,
		"SELECTX 1":                false,
	}
	for sql, want := range cases {
		if got := IsReadOnlyStatement(sql); got != want {
			t.Errorf("IsReadOnlyStatement(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	sql := "-- header\nCREATE TABLE a (id int);\nINSERT INTO a VALUES (1); /* trailing comment */"
	got := SplitStatements(sql)
	want := []string{"-- header\nCREATE TABLE a (id int)", "INSERT INTO a VALUES (1)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitStatements = %#v, want %#v", got, want)
	}
}

func TestIsExplainableQuery(t *testing.T) {
	cases := map[string]bool{
		"SELECT * FROM products": true,
		"-- report\nWITH expensive AS (SELECT 1) SELECT * FROM expensive;": true,
		"SELECT ';' AS value":           true,
		"EXPLAIN SELECT 1":              false,
		"UPDATE products SET price = 0": false,
		"SELECT 1; SELECT 2":            false,
		"":                              false,
	}
	for query, want := range cases {
		if got := IsExplainableQuery(query); got != want {
			t.Errorf("IsExplainableQuery(%q) = %v, want %v", query, got, want)
		}
	}
}

type fakeAccess struct {
	level authorization.AccessLevel
	err   error
}

func (f fakeAccess) ConnectionWithAccess(context.Context, domain.User, int64) (domain.Connection, authorization.AccessLevel, error) {
	return domain.Connection{ID: 1}, f.level, f.err
}

type fakeTarget struct {
	ran     []string
	txStmts [][]string
	failIdx int
	plans   []ExecutionPlan
}

func (f *fakeTarget) Tables(context.Context) ([]Table, error) { return []Table{{Name: "t"}}, nil }
func (f *fakeTarget) Preview(context.Context, string, int) (StatementResult, error) {
	return StatementResult{}, nil
}
func (f *fakeTarget) Run(_ context.Context, sql string, _ int) []StatementResult {
	f.ran = append(f.ran, sql)
	return []StatementResult{{Command: "OK"}}
}
func (f *fakeTarget) RunInTx(_ context.Context, stmts []string, _ int) []StatementResult {
	f.txStmts = append(f.txStmts, stmts)
	var out []StatementResult
	for i, s := range stmts {
		if f.failIdx > 0 && i+1 == f.failIdx {
			out = append(out, StatementResult{Error: "ERROR: failed at " + s})
			return out
		}
		out = append(out, StatementResult{Command: "OK"})
	}
	return out
}
func (f *fakeTarget) Explain(_ context.Context, query string, analyze bool) (ExecutionPlan, error) {
	f.ran = append(f.ran, query)
	actualRows := 1000.0
	plan := ExecutionPlan{Root: PlanNode{
		NodeType:     "Seq Scan",
		RelationName: "products",
		PlanRows:     10,
	}}
	if analyze {
		plan.Root.ActualRows = &actualRows
	}
	f.plans = append(f.plans, plan)
	return plan, nil
}
func (f *fakeTarget) Close(context.Context) error { return nil }

type fakeStore struct {
	target     *fakeTarget
	openErr    error
	readOnly   []bool
	recorded   []MigrationRecord
	migrations []Migration
	changes    []ChangeLog
	executions []MigrationExecution
}

func (f *fakeStore) OpenTarget(_ context.Context, _ int64, readOnly bool) (Target, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.readOnly = append(f.readOnly, readOnly)
	return f.target, nil
}
func (f *fakeStore) RecordMigration(_ context.Context, r MigrationRecord) error {
	f.recorded = append(f.recorded, r)
	return nil
}
func (f *fakeStore) RecentMigrations(context.Context, int64, int) ([]Migration, error) {
	return f.migrations, nil
}
func (f *fakeStore) CreateMigration(_ context.Context, in MigrationInput) (Migration, error) {
	m := Migration{
		ID:         int64(len(f.migrations) + 1),
		DatabaseID: in.DatabaseID,
		Version:    int64(len(f.migrations) + 1),
		Title:      in.Title,
		SQL:        in.SQL,
		Status:     "DRAFT",
	}
	f.migrations = append(f.migrations, m)
	return m, nil
}
func (f *fakeStore) UpdateMigration(_ context.Context, in MigrationInput) (Migration, error) {
	for i := range f.migrations {
		if f.migrations[i].Version == in.Version {
			if f.migrations[i].Status == "SUCCESS" {
				return Migration{}, admin.ErrConflict
			}
			f.migrations[i].Title = in.Title
			f.migrations[i].SQL = in.SQL
			return f.migrations[i], nil
		}
	}
	return Migration{}, admin.ErrNotFound
}
func (f *fakeStore) GetMigration(_ context.Context, _ int64, version int64) (Migration, error) {
	for _, m := range f.migrations {
		if m.Version == version {
			return m, nil
		}
	}
	return Migration{}, admin.ErrNotFound
}
func (f *fakeStore) RecordMigrationExecution(_ context.Context, exec MigrationExecution) error {
	f.executions = append(f.executions, exec)
	for i := range f.migrations {
		if f.migrations[i].Version == exec.Migration.Version {
			f.migrations[i].Status = exec.Status
			f.migrations[i].Error = exec.Error
			f.migrations[i].DurationMs = exec.DurationMs
		}
	}
	f.changes = append(f.changes, ChangeLog{
		MigrationVersion: exec.Migration.Version,
		Source:           "MIGRATION",
		SQL:              exec.Migration.SQL,
		Status:           exec.Status,
		Error:            exec.Error,
		DurationMs:       exec.DurationMs,
	})
	return nil
}
func (f *fakeStore) RecentChanges(context.Context, int64, int) ([]ChangeLog, error) {
	return f.changes, nil
}

func TestExecuteViewOnlyRejectsMutation(t *testing.T) {
	store := &fakeStore{target: &fakeTarget{}}
	svc := NewService(fakeAccess{level: authorization.AccessViewOnly}, store)
	_, err := svc.Execute(context.Background(), domain.User{ID: 1, IsActive: true}, 1, "DELETE FROM t")
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
	if len(store.target.ran) != 0 {
		t.Fatalf("mutation was executed: %v", store.target.ran)
	}
}

func TestExecuteViewOnlySelectUsesReadOnlySession(t *testing.T) {
	store := &fakeStore{target: &fakeTarget{}}
	svc := NewService(fakeAccess{level: authorization.AccessViewOnly}, store)
	if _, err := svc.Execute(context.Background(), domain.User{ID: 1, IsActive: true}, 1, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if len(store.readOnly) != 1 || !store.readOnly[0] {
		t.Fatalf("readOnly = %v, want [true]", store.readOnly)
	}
	if len(store.recorded) != 0 {
		t.Fatal("read query must not be recorded as migration")
	}
}

func TestExplainViewOnlyUsesReadOnlyTarget(t *testing.T) {
	target := &fakeTarget{}
	store := &fakeStore{target: target}
	svc := NewService(fakeAccess{level: authorization.AccessViewOnly}, store)
	view, err := svc.Explain(context.Background(), domain.User{ID: 1, IsActive: true}, 1, "SELECT * FROM products", PlanModeAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.readOnly) != 1 || !store.readOnly[0] {
		t.Fatalf("readOnly = %v, want [true]", store.readOnly)
	}
	if len(target.ran) != 1 || target.ran[0] != "SELECT * FROM products" {
		t.Fatalf("planned queries = %v", target.ran)
	}
	if view.Plan == nil || !view.Plan.Analyze || view.Plan.NodeCount != 1 || len(view.Plan.Warnings) != 2 {
		t.Fatalf("plan summary = %+v", view.Plan)
	}
}

func TestExplainRejectsMutationBeforeOpeningTarget(t *testing.T) {
	store := &fakeStore{target: &fakeTarget{}}
	svc := NewService(fakeAccess{level: authorization.AccessViewEdit}, store)
	view, err := svc.Explain(context.Background(), domain.User{ID: 1, IsActive: true}, 1, "DELETE FROM products", PlanModeEstimate)
	if !errors.Is(err, ErrInvalidPlanQuery) || !strings.Contains(view.Error, "SELECT") {
		t.Fatalf("view error = %q, err = %v", view.Error, err)
	}
	if len(store.readOnly) != 1 {
		// reload opens one read-only target to rebuild the database page; the
		// submitted mutation itself must never reach Target.Explain.
		t.Fatalf("reload opens = %v, want one read-only reload", store.readOnly)
	}
	if len(store.target.ran) != 0 {
		t.Fatalf("mutation reached target: %v", store.target.ran)
	}
}

func TestExecuteViewEditRecordsMigration(t *testing.T) {
	store := &fakeStore{target: &fakeTarget{}}
	svc := NewService(fakeAccess{level: authorization.AccessViewEdit}, store)
	if _, err := svc.Execute(context.Background(), domain.User{ID: 7, IsActive: true}, 1, "CREATE TABLE x (id int)"); err != nil {
		t.Fatal(err)
	}
	if len(store.readOnly) != 1 || store.readOnly[0] {
		t.Fatalf("readOnly = %v, want [false]", store.readOnly)
	}
	if len(store.recorded) != 1 || store.recorded[0].Status != "SUCCESS" || store.recorded[0].UserID != 7 {
		t.Fatalf("recorded = %+v", store.recorded)
	}
}

func TestNoAccessIsForbidden(t *testing.T) {
	store := &fakeStore{target: &fakeTarget{}}
	svc := NewService(fakeAccess{err: admin.ErrForbidden}, store)
	if _, err := svc.Browse(context.Background(), domain.User{ID: 1, IsActive: true}, 1, ""); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if len(store.readOnly) != 0 {
		t.Fatal("target must not be opened without access")
	}
}

func TestViewOnlyCanSeeMigrationHistoryAndDetailButCannotMutateMigrations(t *testing.T) {
	store := &fakeStore{
		target: &fakeTarget{},
		migrations: []Migration{
			{ID: 1, DatabaseID: 1, Version: 1, Title: "init", SQL: "CREATE TABLE t (id int); INSERT INTO t VALUES (1);", Status: "SUCCESS", DurationMs: 15},
		},
	}
	svc := NewService(fakeAccess{level: authorization.AccessViewOnly}, store)
	actor := domain.User{ID: 2, IsActive: true}

	view, err := svc.Browse(context.Background(), actor, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.History) != 1 || view.History[0].Version != 1 {
		t.Fatalf("VIEW_ONLY should see migration history, got %+v", view.History)
	}

	detail, err := svc.ShowMigration(context.Background(), actor, 1, 1)
	if err != nil || detail.SelectedMigration == nil || len(detail.SelectedMigration.Operations) != 2 {
		t.Fatalf("VIEW_ONLY should see migration detail & operations, got %+v, err=%v", detail.SelectedMigration, err)
	}

	if _, err := svc.CreateMigration(context.Background(), actor, 1, "m2", "DROP TABLE t"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("CreateMigration by VIEW_ONLY: err = %v, want ErrReadOnly", err)
	}
	if _, err := svc.UpdateMigration(context.Background(), actor, 1, 1, "m1", "DROP TABLE t"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateMigration by VIEW_ONLY: err = %v, want ErrReadOnly", err)
	}
	if _, err := svc.ExecuteMigration(context.Background(), actor, 1, 1); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("ExecuteMigration by VIEW_ONLY: err = %v, want ErrReadOnly", err)
	}
}

func TestMigrationLifecycleAndTransactionalExecution(t *testing.T) {
	target := &fakeTarget{}
	store := &fakeStore{target: target}
	svc := NewService(fakeAccess{level: authorization.AccessViewEdit}, store)
	actor := domain.User{ID: 5, IsActive: true}

	// 1. Preview SQL operations before creating
	prev, err := svc.PreviewMigration(context.Background(), actor, 1, 0, "add users", "CREATE TABLE u (id int); INSERT INTO u VALUES (1);")
	if err != nil || len(prev.PreviewOps) != 2 {
		t.Fatalf("PreviewMigration ops = %v, err = %v", prev.PreviewOps, err)
	}

	// 2. Create DRAFT migration
	created, err := svc.CreateMigration(context.Background(), actor, 1, "add users", "CREATE TABLE u (id int);")
	if err != nil || created.SelectedMigration == nil || created.SelectedMigration.Status != "DRAFT" {
		t.Fatalf("CreateMigration = %+v, err = %v", created.SelectedMigration, err)
	}

	// 3. Edit DRAFT migration before executing
	updated, err := svc.UpdateMigration(context.Background(), actor, 1, 1, "add users v2", "CREATE TABLE u (id int); INSERT INTO u VALUES (1);")
	if err != nil || updated.SelectedMigration.Title != "add users v2" || len(updated.PreviewOps) != 2 {
		t.Fatalf("UpdateMigration = %+v, err = %v", updated.SelectedMigration, err)
	}

	// 4. Execute migration in transaction (success -> commit + change log)
	executed, err := svc.ExecuteMigration(context.Background(), actor, 1, 1)
	if err != nil || executed.SelectedMigration.Status != "SUCCESS" {
		t.Fatalf("ExecuteMigration = %+v, err = %v", executed.SelectedMigration, err)
	}
	if len(target.txStmts) != 1 || len(target.txStmts[0]) != 2 {
		t.Fatalf("expected transactional execution of 2 statements, got %+v", target.txStmts)
	}
	if len(executed.Changes) != 1 || executed.Changes[0].Source != "MIGRATION" {
		t.Fatalf("expected change log entry, got %+v", executed.Changes)
	}
}

func TestConnectionAuthErrorMessage(t *testing.T) {
	store := &fakeStore{
		openErr: &ConnectError{Reason: "autentikasi database gagal (username atau password salah).", Err: errors.New("28P01")},
	}
	svc := NewService(fakeAccess{level: authorization.AccessViewOnly}, store)
	view, err := svc.Browse(context.Background(), domain.User{ID: 1, IsActive: true}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Error, "autentikasi database gagal") {
		t.Fatalf("unexpected error message: %q", view.Error)
	}
}
