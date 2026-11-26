package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type advisorFunc func(context.Context, domain.User, domain.Job) (workflow.JobActions, error)

func (f advisorFunc) Actions(ctx context.Context, u domain.User, j domain.Job) (workflow.JobActions, error) {
	return f(ctx, u, j)
}

func getJobWithActions(t *testing.T, advisor JobActionAdvisor) map[string]any {
	t.Helper()
	auth := apiAuth(t, nil)
	jobs := &mockWorkflowJobService{getJob: func(context.Context, domain.User, string) (domain.Job, error) {
		return domain.Job{ID: "job-1", ExecutionID: "exec-1", Status: domain.JobStatusFailed}, nil
	}}
	api := NewJobAPI(auth, jobs)
	if advisor != nil {
		api.WithActions(advisor)
	}
	rec := serveAPI(jobRouter(auth, api), http.MethodGet, "/api/v1/jobs/job-1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGetJobCarriesTheAvailableActions(t *testing.T) {
	var seen domain.Job
	body := getJobWithActions(t, advisorFunc(func(_ context.Context, u domain.User, j domain.Job) (workflow.JobActions, error) {
		seen = j
		return workflow.JobActions{CanResume: true, CanRollback: true}, nil
	}))
	if body["id"] != "job-1" || body["can_resume"] != true || body["can_rollback"] != true || body["can_forward"] != false {
		t.Fatalf("body = %v", body)
	}
	if seen.ExecutionID != "exec-1" {
		t.Fatalf("advisor got %+v", seen)
	}
}

func TestGetJobHidesActionsWhenTheyCannotBeWorkedOut(t *testing.T) {
	body := getJobWithActions(t, advisorFunc(func(context.Context, domain.User, domain.Job) (workflow.JobActions, error) {
		return workflow.JobActions{CanResume: true}, errors.New("platform down")
	}))
	if body["status"] != "FAILED" || body["can_resume"] != false {
		t.Fatalf("body = %v; the job must still be returned, with no actions offered", body)
	}
}
