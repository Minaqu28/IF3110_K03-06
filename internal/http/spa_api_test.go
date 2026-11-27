package httpserver

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type spaWorkflowMock struct {
	mockWorkflowJobService
	createRevision        func(context.Context, domain.User, domain.MigrationRevision) (domain.MigrationRevision, error)
	submitActionForReview func(context.Context, domain.User, int64, int64, domain.MigrationAction, *string) (domain.ReviewRequest, error)
	listDecisions         func(context.Context, domain.User, int64, int64) ([]domain.ApprovalDecision, error)
	getExecution          func(context.Context, domain.User, string) (domain.ExecutionRequest, domain.ExecutionResult, error)
}

func (m *spaWorkflowMock) CreateRevision(ctx context.Context, actor domain.User, rev domain.MigrationRevision) (domain.MigrationRevision, error) {
	return m.createRevision(ctx, actor, rev)
}

func (m *spaWorkflowMock) SubmitForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64) (domain.ReviewRequest, error) {
	return m.submitActionForReview(ctx, actor, databaseID, revisionID, domain.MigrationActionApply, nil)
}

func (m *spaWorkflowMock) SubmitActionForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64, action domain.MigrationAction, target *string) (domain.ReviewRequest, error) {
	return m.submitActionForReview(ctx, actor, databaseID, revisionID, action, target)
}

func (m *spaWorkflowMock) ListReviewDecisions(ctx context.Context, actor domain.User, databaseID, reviewID int64) ([]domain.ApprovalDecision, error) {
	return m.listDecisions(ctx, actor, databaseID, reviewID)
}

func (m *spaWorkflowMock) GetExecution(ctx context.Context, actor domain.User, executionID string) (domain.ExecutionRequest, domain.ExecutionResult, error) {
	return m.getExecution(ctx, actor, executionID)
}

func newTestSPAFS() fs.FS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>DBMP SPA</title>")},
		"src/app.js": {Data: []byte("console.log('spa');")},
	}
}

func newSPARouter(
	t *testing.T,
	authSvc AuthenticationService,
	adminStore *recordingAdminStore,
	dbStore *fakeStore,
	queries MigrationQueryService,
	wf WorkflowJobService,
	actions JobActionAdvisor,
) http.Handler {
	t.Helper()
	auth, err := NewAuthenticationHandler(authSvc, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var adminH *AdminHandler
	if adminStore != nil {
		adminH = NewAdminHandler(admin.NewService(adminStore, nil), auth, nil)
	}
	var dbH *DatabaseHandler
	if dbStore != nil {
		access := fakeAccess{levels: map[int64]authorization.AccessLevel{
			1: authorization.AccessViewEdit,
			2: authorization.AccessViewOnly,
		}}
		dbH = NewDatabaseHandler(explorer.NewService(access, dbStore), auth, nil)
	}
	return NewApplicationRouterWithSPA(auth, adminH, dbH, nil, queries, wf, actions, nil, newTestSPAFS(), func(context.Context) error { return nil })
}

func TestSPARoutingAndAssets(t *testing.T) {
	router := newSPARouter(t, fakeAuth{session: sessionFor(rootUser)}, &recordingAdminStore{}, &fakeStore{}, nil, nil, nil)

	for _, path := range []string{"/", "/login", "/dashboard", "/admin", "/databases", "/databases/1", "/databases/1/revisions", "/databases/1/reviews", "/databases/1/history", "/jobs/job-1"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "DBMP SPA") {
			t.Fatalf("GET %s: status=%d body=%q", path, rec.Code, rec.Body.String())
		}
	}

	asset := httptest.NewRecorder()
	router.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/app/src/app.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "spa") {
		t.Fatalf("GET /app/src/app.js: status=%d body=%q", asset.Code, asset.Body.String())
	}

	unknownAPI := httptest.NewRecorder()
	router.ServeHTTP(unknownAPI, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if unknownAPI.Code != http.StatusNotFound || !strings.Contains(unknownAPI.Body.String(), "NOT_FOUND") {
		t.Fatalf("unknown API: status=%d body=%q", unknownAPI.Code, unknownAPI.Body.String())
	}

	badMethod := httptest.NewRecorder()
	router.ServeHTTP(badMethod, httptest.NewRequest(http.MethodPost, "/unknown-spa-path", nil))
	if badMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /unknown-spa-path: status=%d, want 405", badMethod.Code)
	}
}

func TestSPAAuthEndpoints(t *testing.T) {
	authSvc := &scriptedAuth{
		loginID: "opaque-session",
		session: sessionFor(rootUser),
	}
	router := newSPARouter(t, authSvc, nil, nil, nil, nil, nil)

	// Unauthenticated session returns 401 JSON envelope
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("unauthed session: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Fetch login CSRF then login
	csrfRec := httptest.NewRecorder()
	router.ServeHTTP(csrfRec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login-csrf", nil))
	if csrfRec.Code != http.StatusOK {
		t.Fatalf("login-csrf status=%d", csrfRec.Code)
	}
	var csrfEnv struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(csrfRec.Body.Bytes(), &csrfEnv); err != nil || csrfEnv.Data.CSRFToken == "" {
		t.Fatalf("csrf envelope = %s (%v)", csrfRec.Body.String(), err)
	}
	csrfCookie := findCookie(csrfRec, loginCSRFCookie)
	if csrfCookie == nil {
		t.Fatal("missing login CSRF cookie")
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"root","password":"secret-password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-CSRF-Token", csrfEnv.Data.CSRFToken)
	loginReq.AddCookie(csrfCookie)
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK || findCookie(loginRec, sessionCookie) == nil {
		t.Fatalf("login status=%d body=%s", loginRec.Code, loginRec.Body.String())
	}

	// Session check with valid cookie
	sessReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	sessReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK || !strings.Contains(sessRec.Body.String(), `"username":"root"`) {
		t.Fatalf("session status=%d body=%s", sessRec.Code, sessRec.Body.String())
	}

	// Logout requires CSRF
	noCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	noCSRF.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	noCSRFRec := httptest.NewRecorder()
	router.ServeHTTP(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF status=%d, want 403", noCSRFRec.Code)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	logoutReq.Header.Set("X-CSRF-Token", adminCSRF)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204", logoutRec.Code)
	}
}

func TestSPAAdminAndDatabaseEndpoints(t *testing.T) {
	store := &recordingAdminStore{}
	dbStore := &fakeStore{}
	router := newSPARouter(t, fakeAuth{session: sessionFor(rootUser)}, store, dbStore, nil, nil, nil)

	// Admin GET
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"users"`) {
		t.Fatalf("GET /api/v1/admin status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Admin set_group_leader
	leaderReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/actions", strings.NewReader(`{"action":"set_group_leader","group_id":1,"leader_id":2}`))
	leaderReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	leaderReq.Header.Set("X-CSRF-Token", adminCSRF)
	leaderRec := httptest.NewRecorder()
	router.ServeHTTP(leaderRec, leaderReq)
	if leaderRec.Code != http.StatusOK {
		t.Fatalf("set_group_leader status=%d body=%s", leaderRec.Code, leaderRec.Body.String())
	}
	if len(store.calls) != 1 || store.calls[0] != "SetGroupLeader" {
		t.Fatalf("store calls = %v, want [SetGroupLeader]", store.calls)
	}

	// Non-admin refused on /api/v1/admin
	nonAdminRouter := newSPARouter(t, fakeAuth{session: sessionFor(regularUser)}, store, dbStore, nil, nil, nil)
	naReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin", nil)
	naReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	naRec := httptest.NewRecorder()
	nonAdminRouter.ServeHTTP(naRec, naReq)
	if naRec.Code != http.StatusForbidden {
		t.Fatalf("non-admin /api/v1/admin status=%d, want 403", naRec.Code)
	}

	// VIEW_ONLY query mutation forbidden; EXPLAIN allowed
	mutReq := httptest.NewRequest(http.MethodPost, "/api/v1/databases/2/query", strings.NewReader(`{"sql":"DELETE FROM users"}`))
	mutReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	mutReq.Header.Set("X-CSRF-Token", adminCSRF)
	mutRec := httptest.NewRecorder()
	router.ServeHTTP(mutRec, mutReq)
	if mutRec.Code != http.StatusForbidden {
		t.Fatalf("VIEW_ONLY mutation status=%d, want 403", mutRec.Code)
	}

	expReq := httptest.NewRequest(http.MethodPost, "/api/v1/databases/2/explain", strings.NewReader(`{"sql":"SELECT * FROM users","mode":"estimate"}`))
	expReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	expReq.Header.Set("X-CSRF-Token", adminCSRF)
	expRec := httptest.NewRecorder()
	router.ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusOK || !strings.Contains(expRec.Body.String(), `"Seq Scan"`) {
		t.Fatalf("EXPLAIN status=%d body=%s", expRec.Code, expRec.Body.String())
	}
}

func TestSPAWorkflowEndpoints(t *testing.T) {
	var gotAction domain.MigrationAction
	var gotTarget *string
	var gotHideRollback bool

	queries := &mockMigrationQueryService{
		listRevisions: func(_ context.Context, _ domain.User, dbID int64) ([]domain.MigrationRevision, error) {
			return []domain.MigrationRevision{{ID: 10, MigrationID: 3, DatabaseID: dbID, Revision: 1, Title: "v1", Status: domain.RevisionStatusDraft}}, nil
		},
		listHistory: func(_ context.Context, _ domain.User, _ int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
			gotHideRollback = hideRollback
			return []domain.ExecutionAttempt{{AttemptID: 1, ExecutionID: "exec-1", MigrationID: 3, Revision: 1, Action: domain.MigrationActionApply, ActorUsername: "alice", Status: domain.ExecutionStatusSucceeded}}, nil
		},
	}
	wf := &spaWorkflowMock{
		mockWorkflowJobService: mockWorkflowJobService{
			listReviews: func(context.Context, domain.User, int64) ([]domain.ReviewRequest, error) {
				return []domain.ReviewRequest{{ID: 5, RevisionID: 10, Action: domain.MigrationActionRollback, Status: domain.ReviewStatusPending}}, nil
			},
			decideApproval: func(_ context.Context, _ domain.User, reviewID int64, status domain.ApprovalStatus, _ string) (domain.ReviewRequest, error) {
				return domain.ReviewRequest{ID: reviewID, Status: domain.ReviewStatusApproved}, nil
			},
			submitExecution: func(_ context.Context, _ domain.User, req domain.ExecutionRequest) (domain.Job, error) {
				return domain.Job{ID: "job-7", ExecutionID: "exec-7", DatabaseID: req.DatabaseID, Status: domain.JobStatusQueued}, nil
			},
			getJob: func(_ context.Context, _ domain.User, jobID string) (domain.Job, error) {
				return domain.Job{ID: jobID, ExecutionID: "exec-7", DatabaseID: 1, Status: domain.JobStatusFailed}, nil
			},
		},
		createRevision: func(_ context.Context, _ domain.User, rev domain.MigrationRevision) (domain.MigrationRevision, error) {
			rev.ID = 10
			rev.Status = domain.RevisionStatusDraft
			return rev, nil
		},
		submitActionForReview: func(_ context.Context, _ domain.User, _, revID int64, action domain.MigrationAction, target *string) (domain.ReviewRequest, error) {
			gotAction, gotTarget = action, target
			return domain.ReviewRequest{ID: 5, RevisionID: revID, Action: action, TargetExecutionID: target, Status: domain.ReviewStatusPending}, nil
		},
		listDecisions: func(context.Context, domain.User, int64, int64) ([]domain.ApprovalDecision, error) {
			return []domain.ApprovalDecision{{ID: 1, ReviewID: 5, GroupID: 2, LeaderID: 9, Status: domain.ApprovalStatusPending}}, nil
		},
		getExecution: func(context.Context, domain.User, string) (domain.ExecutionRequest, domain.ExecutionResult, error) {
			return domain.ExecutionRequest{
				ExecutionID: "exec-7", DatabaseID: 1, MigrationID: 3, RevisionID: 10, ApprovalRequestID: 5,
				Action: domain.MigrationActionApply, Mode: domain.ExecutionModeBatch,
			}, domain.ExecutionResult{Status: domain.ExecutionStatusPartial}, nil
		},
	}
	actions := advisorFunc(func(context.Context, domain.User, domain.Job) (workflow.JobActions, error) {
		return workflow.JobActions{CanResume: true, CanRollback: true}, nil
	})
	router := newSPARouter(t, fakeAuth{session: sessionFor(rootUser)}, nil, nil, queries, wf, actions)

	// POST create revision
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/databases/1/revisions", strings.NewReader(`{"title":"add col","up_sql":"ALTER TABLE t ADD COLUMN c int","down_sql":"ALTER TABLE t DROP COLUMN c","reversible":true}`))
	createReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	createReq.Header.Set("X-CSRF-Token", adminCSRF)
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create revision status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	// POST submit ROLLBACK review
	revReq := httptest.NewRequest(http.MethodPost, "/api/v1/databases/1/revisions/10/review", strings.NewReader(`{"action":"ROLLBACK","target_execution_id":"exec-1"}`))
	revReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	revReq.Header.Set("X-CSRF-Token", adminCSRF)
	revRec := httptest.NewRecorder()
	router.ServeHTTP(revRec, revReq)
	if revRec.Code != http.StatusCreated || gotAction != domain.MigrationActionRollback || gotTarget == nil || *gotTarget != "exec-1" {
		t.Fatalf("submit rollback review status=%d action=%s target=%v body=%s", revRec.Code, gotAction, gotTarget, revRec.Body.String())
	}

	// GET history with hide_rollback=true returns rich attempt fields
	histReq := httptest.NewRequest(http.MethodGet, "/api/v1/databases/1/history?hide_rollback=true", nil)
	histReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	histRec := httptest.NewRecorder()
	router.ServeHTTP(histRec, histReq)
	if histRec.Code != http.StatusOK || !gotHideRollback || !strings.Contains(histRec.Body.String(), `"actor_username":"alice"`) {
		t.Fatalf("history status=%d hide=%v body=%s", histRec.Code, gotHideRollback, histRec.Body.String())
	}

	// GET job returns job + JobActions + execution metadata for 1-click Resume
	jobReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-7", nil)
	jobReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	jobRec := httptest.NewRecorder()
	router.ServeHTTP(jobRec, jobReq)
	if jobRec.Code != http.StatusOK || !strings.Contains(jobRec.Body.String(), `"can_resume":true`) || !strings.Contains(jobRec.Body.String(), `"approval_request_id":5`) {
		t.Fatalf("job status=%d body=%s", jobRec.Code, jobRec.Body.String())
	}
}

type advisorFunc func(context.Context, domain.User, domain.Job) (workflow.JobActions, error)

func (f advisorFunc) Actions(ctx context.Context, u domain.User, j domain.Job) (workflow.JobActions, error) {
	return f(ctx, u, j)
}
