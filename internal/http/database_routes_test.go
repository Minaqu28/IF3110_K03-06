package httpserver

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

// --- fakes -----------------------------------------------------------------

type fakeAuth struct{ session authentication.Session }

func (f fakeAuth) Login(context.Context, string, string) (string, error) { return "", nil }
func (f fakeAuth) Logout(context.Context, string) error                  { return nil }
func (f fakeAuth) Validate(_ context.Context, id string) (authentication.Session, error) {
	if id != "valid" {
		return authentication.Session{}, authentication.ErrUnauthenticated
	}
	return f.session, nil
}

type fakeAccess struct {
	levels map[int64]authorization.AccessLevel
}

func (f fakeAccess) ConnectionWithAccess(_ context.Context, _ domain.User, id int64) (domain.Connection, authorization.AccessLevel, error) {
	level, ok := f.levels[id]
	if !ok {
		return domain.Connection{}, authorization.AccessNone, admin.ErrForbidden
	}
	return domain.Connection{ID: id, Name: "app"}, level, nil
}

type fakeTarget struct{ ran *[]string }

func (f fakeTarget) Tables(context.Context) ([]explorer.Table, error) {
	return []explorer.Table{{Name: "users", Type: "BASE TABLE", Rows: 2}}, nil
}
func (f fakeTarget) Preview(_ context.Context, table string, _ int) (explorer.StatementResult, error) {
	if table != "users" {
		return explorer.StatementResult{}, explorer.ErrTableNotFound
	}
	return explorer.StatementResult{Columns: []string{"id"}, Rows: [][]explorer.Cell{{{Value: "42"}}}}, nil
}
func (f fakeTarget) Run(_ context.Context, sql string, _ int) []explorer.StatementResult {
	*f.ran = append(*f.ran, sql)
	return []explorer.StatementResult{{Command: "EXECUTED"}}
}
func (f fakeTarget) Explain(_ context.Context, query string, analyze bool) (explorer.ExecutionPlan, error) {
	mode := explorer.PlanModeEstimate
	if analyze {
		mode = explorer.PlanModeAnalyze
	}
	*f.ran = append(*f.ran, "PLAN "+mode+" "+query)
	return explorer.ExecutionPlan{
		PlanningTimeMs: 0.12,
		Root: explorer.PlanNode{
			NodeType:     "Seq Scan",
			RelationName: "users",
			PlanRows:     2,
		},
	}, nil
}
func (f fakeTarget) Close(context.Context) error { return nil }

type fakeStore struct {
	ran        []string
	migrations []explorer.Migration
}

func (f *fakeStore) OpenTarget(context.Context, int64, bool) (explorer.Target, error) {
	return fakeTarget{ran: &f.ran}, nil
}
func (f *fakeStore) RecordMigration(context.Context, explorer.MigrationRecord) error { return nil }
func (f *fakeStore) RecentMigrations(context.Context, int64, int) ([]explorer.Migration, error) {
	return f.migrations, nil
}
func (f *fakeStore) CreateMigration(_ context.Context, in explorer.MigrationInput) (explorer.Migration, error) {
	m := explorer.Migration{ID: 1, DatabaseID: in.DatabaseID, Version: int64(len(f.migrations) + 1), Title: in.Title, SQL: in.SQL, Status: "DRAFT"}
	f.migrations = append(f.migrations, m)
	return m, nil
}
func (f *fakeStore) UpdateMigration(_ context.Context, in explorer.MigrationInput) (explorer.Migration, error) {
	for i := range f.migrations {
		if f.migrations[i].Version == in.Version {
			f.migrations[i].Title = in.Title
			f.migrations[i].SQL = in.SQL
			return f.migrations[i], nil
		}
	}
	return explorer.Migration{}, admin.ErrNotFound
}
func (f *fakeStore) GetMigration(_ context.Context, _ int64, version int64) (explorer.Migration, error) {
	for _, m := range f.migrations {
		if m.Version == version {
			return m, nil
		}
	}
	return explorer.Migration{}, admin.ErrNotFound
}
func (f *fakeStore) RecordMigrationExecution(_ context.Context, exec explorer.MigrationExecution) error {
	for i := range f.migrations {
		if f.migrations[i].Version == exec.Migration.Version {
			f.migrations[i].Status = exec.Status
		}
	}
	return nil
}
func (f *fakeStore) RecentChanges(context.Context, int64, int) ([]explorer.ChangeLog, error) {
	return nil, nil
}

// --- helpers ---------------------------------------------------------------

const testCSRF = "csrf-token"

func newTestRouter(t *testing.T, session authentication.Session) (http.Handler, *fakeStore) {
	t.Helper()
	pages, err := template.ParseFS(frontend.Files, "pages/*.html")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticationHandler(fakeAuth{session: session}, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	access := fakeAccess{levels: map[int64]authorization.AccessLevel{
		1: authorization.AccessViewEdit,
		2: authorization.AccessViewOnly,
	}}
	handler := NewDatabaseHandler(explorer.NewService(access, store), auth, pages)
	router := NewApplicationRouter(auth, nil, handler, nil, func(context.Context) error { return nil })
	return router, store
}

func activeSession() authentication.Session {
	return authentication.Session{User: domain.User{ID: 1, IsActive: true}, CSRFToken: testCSRF}
}

func do(router http.Handler, method, target string, form url.Values, cookie bool) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if cookie {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func query(sql, csrf string) url.Values { return url.Values{"sql": {sql}, "csrf_token": {csrf}} }

// --- tests -----------------------------------------------------------------

func TestShowRequiresLogin(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	rec := do(router, http.MethodGet, "/databases/1", nil, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("got %d %q, want redirect to /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestShowForbiddenWithoutGrant(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	if rec := do(router, http.MethodGet, "/databases/99", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestShowInvalidID(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	if rec := do(router, http.MethodGet, "/databases/abc", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestShowInvalidPage(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	for _, path := range []string{"/databases/1?page=0", "/databases/1?page=abc", "/databases/1?page=10001"} {
		if rec := do(router, http.MethodGet, path, nil, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestShowListsTablesAndPreview(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	rec := do(router, http.MethodGet, "/databases/2?table=users", nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "users") || !strings.Contains(body, "42") {
		t.Fatalf("status = %d, body missing table/preview:\n%s", rec.Code, body)
	}
	if strings.Contains(body, `name="sql"`) {
		t.Error("VIEW_ONLY page must not contain the SQL editor")
	}
}

func TestShowUnknownTable(t *testing.T) {
	router, _ := newTestRouter(t, activeSession())
	rec := do(router, http.MethodGet, "/databases/1?table=nope", nil, true)
	if !strings.Contains(rec.Body.String(), "Tabel tidak ditemukan") {
		t.Fatal("expected table-not-found message")
	}
}

func TestExecuteRejectsMissingCSRF(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	rec := do(router, http.MethodPost, "/databases/1/query", query("SELECT 1", "wrong"), true)
	if rec.Code != http.StatusForbidden || len(store.ran) != 0 {
		t.Fatalf("status = %d, ran = %v; want 403 and nothing executed", rec.Code, store.ran)
	}
}

func TestExecuteRejectsStaleSession(t *testing.T) {
	session := activeSession()
	session.Previous = true
	router, store := newTestRouter(t, session)
	rec := do(router, http.MethodPost, "/databases/1/query", query("SELECT 1", testCSRF), true)
	if rec.Code != http.StatusForbidden || len(store.ran) != 0 {
		t.Fatalf("status = %d, ran = %v; want 403", rec.Code, store.ran)
	}
}

func TestExecuteViewOnlyMutationForbidden(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	rec := do(router, http.MethodPost, "/databases/2/query", query("DROP TABLE users", testCSRF), true)
	if rec.Code != http.StatusForbidden || len(store.ran) != 0 {
		t.Fatalf("status = %d, ran = %v; want 403 and nothing executed", rec.Code, store.ran)
	}
	if !strings.Contains(rec.Body.String(), "VIEW_ONLY") {
		t.Error("expected read-only explanation in page")
	}
}

func TestExecuteViewOnlySelectAllowed(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	rec := do(router, http.MethodPost, "/databases/2/query", query("SELECT * FROM users", testCSRF), true)
	if rec.Code != http.StatusOK || len(store.ran) != 1 {
		t.Fatalf("status = %d, ran = %v", rec.Code, store.ran)
	}
}

func TestExecuteViewEditRunsMutation(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	rec := do(router, http.MethodPost, "/databases/1/query", query("DELETE FROM users", testCSRF), true)
	if rec.Code != http.StatusOK || len(store.ran) != 1 || store.ran[0] != "DELETE FROM users" {
		t.Fatalf("status = %d, ran = %v", rec.Code, store.ran)
	}
	if !strings.Contains(rec.Body.String(), "EXECUTED") {
		t.Error("expected execution result in page")
	}
}

func TestExecuteEmptySQL(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	rec := do(router, http.MethodPost, "/databases/1/query", query("   ", testCSRF), true)
	if len(store.ran) != 0 || !strings.Contains(rec.Body.String(), "SQL tidak boleh kosong") {
		t.Fatalf("ran = %v, want empty-SQL error", store.ran)
	}
}

func TestExplainRouteForViewOnly(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	form := url.Values{
		"plan_sql":   {"SELECT * FROM users"},
		"mode":       {explorer.PlanModeEstimate},
		"csrf_token": {testCSRF},
	}
	rec := do(router, http.MethodPost, "/databases/2/explain", form, true)
	if rec.Code != http.StatusOK || len(store.ran) != 1 || store.ran[0] != "PLAN estimate SELECT * FROM users" {
		t.Fatalf("status = %d, ran = %v", rec.Code, store.ran)
	}
	for _, want := range []string{"Estimated Execution Plan", "Seq Scan", "users"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("plan page missing %q", want)
		}
	}
}

func TestExplainRouteRejectsCSRFAndMutation(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	wrongCSRF := url.Values{"plan_sql": {"SELECT 1"}, "mode": {explorer.PlanModeEstimate}, "csrf_token": {"wrong"}}
	if rec := do(router, http.MethodPost, "/databases/2/explain", wrongCSRF, true); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF status = %d, want 403", rec.Code)
	}
	mutation := url.Values{"plan_sql": {"DELETE FROM users"}, "mode": {explorer.PlanModeAnalyze}, "csrf_token": {testCSRF}}
	if rec := do(router, http.MethodPost, "/databases/2/explain", mutation, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("mutation status = %d, want 400", rec.Code)
	}
	crossTarget := url.Values{"plan_sql": {"SELECT 1"}, "mode": {explorer.PlanModeEstimate}, "csrf_token": {testCSRF}}
	if rec := do(router, http.MethodPost, "/databases/99/explain", crossTarget, true); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-target status = %d, want 403", rec.Code)
	}
	if len(store.ran) != 0 {
		t.Fatalf("invalid plan query ran: %v", store.ran)
	}
}

func TestMigrationRoutesPermissionsAndFlow(t *testing.T) {
	router, store := newTestRouter(t, activeSession())
	migForm := url.Values{"title": {"create items"}, "migration_sql": {"CREATE TABLE items (id int); INSERT INTO items VALUES (1);"}, "csrf_token": {testCSRF}}

	// VIEW_ONLY user on database 2 cannot create a migration
	if rec := do(router, http.MethodPost, "/databases/2/migrations", migForm, true); rec.Code != http.StatusForbidden {
		t.Fatalf("VIEW_ONLY create migration status = %d, want 403", rec.Code)
	}

	// VIEW_EDIT user on database 1 can preview, create, update, and execute a migration
	prev := do(router, http.MethodPost, "/databases/1/migrations/preview", migForm, true)
	if prev.Code != http.StatusOK || !strings.Contains(prev.Body.String(), "Preview Operasi SQL") {
		t.Fatalf("preview status = %d", prev.Code)
	}
	create := do(router, http.MethodPost, "/databases/1/migrations", migForm, true)
	if create.Code != http.StatusSeeOther || create.Header().Get("Location") != "/databases/1?migration=1" || len(store.migrations) != 1 {
		t.Fatalf("create status = %d, migrations = %+v", create.Code, store.migrations)
	}
	exec := do(router, http.MethodPost, "/databases/1/migrations/1/execute", url.Values{"csrf_token": {testCSRF}}, true)
	if exec.Code != http.StatusSeeOther || exec.Header().Get("Location") != "/databases/1?migration=1" || len(store.ran) != 1 {
		t.Fatalf("execute status = %d, ran = %v", exec.Code, store.ran)
	}

	// VIEW_ONLY user can view migration detail when granted access
	detail := do(router, http.MethodGet, "/databases/2?migration=1", nil, true)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "CREATE TABLE items") {
		t.Fatalf("VIEW_ONLY migration detail status = %d", detail.Code)
	}
}
