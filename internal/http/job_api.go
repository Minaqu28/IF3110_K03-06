package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

const jobRequestTimeout = 5 * time.Second

type JobReader interface {
	GetJob(ctx context.Context, actor domain.User, jobID string) (domain.Job, error)
}

type JobSubmitter interface {
	SubmitExecution(ctx context.Context, actor domain.User, req domain.ExecutionRequest) (domain.Job, error)
}

type JobActionAdvisor interface {
	Actions(ctx context.Context, actor domain.User, job domain.Job) (workflow.JobActions, error)
}

type jobResponse struct {
	domain.Job
	workflow.JobActions
}

type JobAPI struct {
	auth    *AuthenticationHandler
	jobs    JobReader
	submit  JobSubmitter
	actions JobActionAdvisor
}

func NewJobAPI(auth *AuthenticationHandler, jobs JobReader) *JobAPI {
	return &JobAPI{auth: auth, jobs: jobs}
}

func (a *JobAPI) WithSubmit(submit JobSubmitter) *JobAPI {
	a.submit = submit
	return a
}

func (a *JobAPI) WithActions(actions JobActionAdvisor) *JobAPI {
	a.actions = actions
	return a
}

func (a *JobAPI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/jobs/{id}", a.GetJob)
	if a.submit != nil {
		mux.HandleFunc("POST /api/v1/databases/{id}/executions", a.SubmitExecution)
	}
}

func (a *JobAPI) GetJob(w http.ResponseWriter, r *http.Request) {
	session, ok := a.auth.apiSession(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobRequestTimeout)
	defer cancel()
	job, err := a.jobs.GetJob(ctx, session.User, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response := jobResponse{Job: job}
	if a.actions != nil {
		actions, err := a.actions.Actions(ctx, session.User, job)
		if err != nil {
			slog.Warn("could not work out the job's next actions", "job_id", job.ID, "error", err)
			actions = workflow.JobActions{}
		}
		response.JobActions = actions
	}
	writeJSON(w, http.StatusOK, response)
}
