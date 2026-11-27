package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestWorkflowHandlerEndpoints(t *testing.T) {
	authHandler, err := NewAuthenticationHandler(fakeAuth{session: authentication.Session{
		User:      domain.User{ID: 7, Username: "alice", IsActive: true},
		CSRFToken: "csrf-ok",
	}}, nil, false)
	if err != nil {
		t.Fatalf("NewAuthenticationHandler() error = %v", err)
	}

	var gotHideRollback bool
	queries := &mockMigrationQueryService{
		listRevisions: func(_ context.Context, _ domain.User, dbID int64) ([]domain.MigrationRevision, error) {
			return []domain.MigrationRevision{{ID: 1, DatabaseID: dbID, Revision: 1, Title: "init"}}, nil
		},
		listHistory: func(_ context.Context, _ domain.User, _ int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
			gotHideRollback = hideRollback
			return []domain.ExecutionAttempt{{AttemptID: 1, ExecutionID: "exec-1", Status: domain.ExecutionStatusSucceeded}}, nil
		},
	}
	wf := &mockWorkflowJobService{
		listReviews: func(context.Context, domain.User, int64) ([]domain.ReviewRequest, error) {
			return []domain.ReviewRequest{{ID: 3, RevisionID: 1, Status: domain.ReviewStatusPending}}, nil
		},
		decideApproval: func(_ context.Context, _ domain.User, reviewID int64, status domain.ApprovalStatus, comment string) (domain.ReviewRequest, error) {
			return domain.ReviewRequest{ID: reviewID, Status: domain.ReviewStatusApproved}, nil
		},
		submitExecution: func(_ context.Context, _ domain.User, req domain.ExecutionRequest) (domain.Job, error) {
			return domain.Job{ID: "job-99", ExecutionID: "exec-99", DatabaseID: req.DatabaseID, Status: domain.JobStatusQueued}, nil
		},
		getJob: func(_ context.Context, _ domain.User, jobID string) (domain.Job, error) {
			return domain.Job{ID: jobID, Status: domain.JobStatusRunning}, nil
		},
	}

	router := NewApplicationRouterWithSPA(authHandler, nil, nil, NewWorkflowHandler(authHandler, queries, wf), nil, nil, nil, nil, nil, func(context.Context) error { return nil })

	// GET history with hide_rollback=true
	req := httptest.NewRequest(http.MethodGet, "/api/databases/4/history?hide_rollback=true", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !gotHideRollback {
		t.Fatalf("history status=%d hideRollback=%v", rec.Code, gotHideRollback)
	}

	// POST execution requires CSRF and returns 202
	req = httptest.NewRequest(http.MethodPost, "/api/databases/4/executions", strings.NewReader(`{"migration_id":1,"revision_id":1,"approval_request_id":3,"action":"APPLY","mode":"NORMAL"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	req.Header.Set("X-CSRF-Token", "csrf-ok")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var job domain.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || job.ID != "job-99" {
		t.Fatalf("unexpected job response: %s (%v)", rec.Body.String(), err)
	}

	// POST decide without CSRF returns 403
	req = httptest.NewRequest(http.MethodPost, "/api/reviews/3/decide", strings.NewReader(`{"status":"APPROVED"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d, want 403", rec.Code)
	}

	// GET job returns 404 when not found
	wf.getJob = func(context.Context, domain.User, string) (domain.Job, error) {
		return domain.Job{}, admin.ErrNotFound
	}
	req = httptest.NewRequest(http.MethodGet, "/api/jobs/missing", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing job status=%d, want 404", rec.Code)
	}
}
