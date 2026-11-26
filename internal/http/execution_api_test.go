package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var jsonCSRF = map[string]string{"Content-Type": "application/json", csrfHeader: apiCSRF}

const submitJSON = `{"job_id":"11111111-1111-4111-8111-111111111111","migration_id":9,"revision_id":90,"approval_request_id":3,"action":"ROLLBACK","mode":"BATCH","target_execution_id":"exec-0"}`

func submitRouter(t *testing.T, submit func(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error)) (http.Handler, *int) {
	t.Helper()
	calls := 0
	auth := apiAuth(t, nil)
	service := &mockWorkflowJobService{submitExecution: func(ctx context.Context, u domain.User, r domain.ExecutionRequest) (domain.Job, error) {
		calls++
		return submit(ctx, u, r)
	}}
	return jobRouter(auth, NewJobAPI(auth, service).WithSubmit(service)), &calls
}

func TestSubmitExecutionAcceptsAndReturnsTheJob(t *testing.T) {
	var got domain.ExecutionRequest
	var actor domain.User
	router, _ := submitRouter(t, func(_ context.Context, u domain.User, r domain.ExecutionRequest) (domain.Job, error) {
		actor, got = u, r
		return domain.Job{ID: r.JobID, ExecutionID: "exec-9", DatabaseID: r.DatabaseID, Status: domain.JobStatusQueued}, nil
	})
	start := time.Now()
	rec := serveAPI(router, http.MethodPost, "/api/v1/databases/4/executions", submitJSON, jsonCSRF)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/v1/jobs/11111111-1111-4111-8111-111111111111" {
		t.Fatalf("status %d, location %q, body %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("submit took %v", elapsed)
	}
	var job domain.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || job.Status != domain.JobStatusQueued || job.ExecutionID != "exec-9" {
		t.Fatalf("job = %+v, err = %v", job, err)
	}
	if actor.ID != apiUser.ID || got.DatabaseID != 4 || got.MigrationID != 9 || got.RevisionID != 90 || got.ApprovalRequestID != 3 ||
		got.Action != domain.MigrationActionRollback || got.Mode != domain.ExecutionModeBatch || got.TargetExecutionID == nil || *got.TargetExecutionID != "exec-0" ||
		got.ActorID != 0 || !got.RequestedAt.IsZero() {
		t.Fatalf("request = %+v", got)
	}
}

func TestSubmitExecutionRefusals(t *testing.T) {
	ok := func(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error) {
		return domain.Job{ID: "j"}, nil
	}
	cases := []struct {
		name    string
		path    string
		body    string
		headers map[string]string
		code    int
	}{
		{"no CSRF header", "/api/v1/databases/4/executions", submitJSON, map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		{"wrong CSRF header", "/api/v1/databases/4/executions", submitJSON, map[string]string{"Content-Type": "application/json", csrfHeader: "nope"}, http.StatusForbidden},
		{"form post", "/api/v1/databases/4/executions", "action=APPLY", map[string]string{"Content-Type": "application/x-www-form-urlencoded", csrfHeader: apiCSRF}, http.StatusUnsupportedMediaType},
		{"bad database id", "/api/v1/databases/x/executions", submitJSON, jsonCSRF, http.StatusBadRequest},
		{"malformed JSON", "/api/v1/databases/4/executions", `{"action":`, jsonCSRF, http.StatusBadRequest},
		{"unknown field", "/api/v1/databases/4/executions", `{"action":"APPLY","actor_id":1}`, jsonCSRF, http.StatusBadRequest},
		{"two documents", "/api/v1/databases/4/executions", `{"action":"APPLY"} {}`, jsonCSRF, http.StatusBadRequest},
		{"too large", "/api/v1/databases/4/executions", `{"job_id":"` + strings.Repeat("a", maxSubmitBodyBytes) + `"}`, jsonCSRF, http.StatusBadRequest},
	}
	for _, c := range cases {
		router, calls := submitRouter(t, ok)
		rec := serveAPI(router, http.MethodPost, c.path, c.body, c.headers)
		if rec.Code != c.code || *calls != 0 {
			t.Fatalf("%s: status %d (want %d), service called %d times", c.name, rec.Code, c.code, *calls)
		}
	}
}

func TestSubmitExecutionMapsServiceErrors(t *testing.T) {
	cases := []struct {
		err     error
		code    int
		message string
	}{
		{admin.ErrForbidden, http.StatusForbidden, "forbidden"},
		{workflow.ErrDatabaseHeld, http.StatusConflict, "another migration on this database is unfinished"},
		{workflow.ErrIrreversible, http.StatusConflict, "pembatalan tidak aman; buat migration perbaikan baru"},
		{admin.ErrInvalidInput, http.StatusBadRequest, admin.ErrInvalidInput.Error()},
	}
	for _, c := range cases {
		router, _ := submitRouter(t, func(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error) {
			return domain.Job{}, c.err
		})
		rec := serveAPI(router, http.MethodPost, "/api/v1/databases/4/executions", submitJSON, jsonCSRF)
		var body apiError
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.code || body.Error != c.message {
			t.Fatalf("%v: status %d, error %q; want %d %q", c.err, rec.Code, body.Error, c.code, c.message)
		}
	}
}

func TestSubmitExecutionIsOffWithoutTheFlag(t *testing.T) {
	auth := apiAuth(t, nil)
	router := jobRouter(auth, NewJobAPI(auth, &mockWorkflowJobService{}))
	if rec := serveAPI(router, http.MethodPost, "/api/v1/databases/4/executions", submitJSON, jsonCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 while ASYNC_MIGRATIONS is off", rec.Code)
	}
}

func TestSubmitExecutionRefusesAStaleSession(t *testing.T) {
	h, err := NewAuthenticationHandler(&sessionService{session: authentication.Session{ID: "s", User: apiUser, CSRFToken: apiCSRF, Previous: true}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	service := &mockWorkflowJobService{}
	rec := serveAPI(jobRouter(h, NewJobAPI(h, service).WithSubmit(service)), http.MethodPost, "/api/v1/databases/4/executions", submitJSON, jsonCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
}
