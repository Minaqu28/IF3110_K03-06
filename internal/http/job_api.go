package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
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

type spaJobResponse struct {
	domain.Job
	workflow.JobActions
	MigrationID       int64                  `json:"migration_id,omitempty"`
	RevisionID        int64                  `json:"revision_id,omitempty"`
	ApprovalRequestID int64                  `json:"approval_request_id,omitempty"`
	Action            domain.MigrationAction `json:"action,omitempty"`
	Mode              domain.ExecutionMode   `json:"mode,omitempty"`
	TargetExecutionID *string                `json:"target_execution_id,omitempty"`
}

type JobAPI struct {
	auth    *AuthenticationHandler
	jobs    JobReader
	submit  JobSubmitter
	actions JobActionAdvisor
}

type apiServices struct {
	auth       *AuthenticationHandler
	migrations MigrationQueryService
	workflow   WorkflowJobService
	actions    JobActionAdvisor
}

func (h *apiServices) ready(w http.ResponseWriter, service any) bool {
	if service == nil {
		jsonError(w, 503, "NOT_READY", "Service fitur ini belum tersedia.")
		return false
	}
	return true
}

func (h *apiServices) session(w http.ResponseWriter, r *http.Request, mutation bool) (domain.User, bool) {
	session, ok := h.auth.spaSession(w, r)
	if !ok {
		return domain.User{}, false
	}
	if mutation && (session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken)) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return domain.User{}, false
	}
	return session.User, true
}

func (h *apiServices) revisions(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok || !h.ready(w, h.migrations) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), id, "")
	result, err := h.migrations.ListMigrationRevisions(ctx, user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) createRevision(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	svc, ok := h.workflow.(RevisionWorkflowService)
	if !ok {
		jsonError(w, 503, "NOT_READY", "Pembuatan revisi belum tersedia.")
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	var input struct {
		MigrationID        int64               `json:"migration_id"`
		Title              string              `json:"title"`
		UpSQL              string              `json:"up_sql"`
		DownSQL            string              `json:"down_sql"`
		Reversible         bool                `json:"reversible"`
		IrreversibleReason string              `json:"irreversible_reason"`
		Batch              *domain.BatchConfig `json:"batch"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), id, "")
	created, err := svc.CreateRevision(ctx, user, domain.MigrationRevision{
		DatabaseID:         id,
		MigrationID:        input.MigrationID,
		Title:              input.Title,
		UpSQL:              input.UpSQL,
		DownSQL:            input.DownSQL,
		Reversible:         input.Reversible,
		IrreversibleReason: input.IrreversibleReason,
		Batch:              input.Batch,
	})
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, http.StatusCreated, created)
}

func (h *apiServices) submitReview(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	revID, err := positiveID(r.PathValue("revisionId"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID revisi tidak valid.")
		return
	}
	var input struct {
		Action            domain.MigrationAction `json:"action"`
		TargetExecutionID *string                `json:"target_execution_id"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&input); err != nil && !errors.Is(err, io.EOF) {
			jsonError(w, 400, "INVALID_JSON", "Body JSON tidak valid.")
			return
		}
	}
	if input.Action == "" {
		input.Action = domain.MigrationActionApply
	}
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), dbID, "")
	var review domain.ReviewRequest
	if actionSvc, ok := h.workflow.(ActionReviewWorkflowService); ok {
		review, err = actionSvc.SubmitActionForReview(ctx, user, dbID, revID, input.Action, input.TargetExecutionID)
	} else if revSvc, ok := h.workflow.(RevisionWorkflowService); ok && input.Action == domain.MigrationActionApply {
		review, err = revSvc.SubmitForReview(ctx, user, dbID, revID)
	} else {
		jsonError(w, 503, "NOT_READY", "Pengajuan review belum tersedia.")
		return
	}
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, http.StatusCreated, review)
}

func (h *apiServices) history(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok || !h.ready(w, h.migrations) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("hide_rollback"))
	hideRollback := strings.EqualFold(raw, "true") || raw == "1"
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), id, "")
	result, err := h.migrations.ListExecutionHistory(ctx, user, id, hideRollback)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) reviews(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), id, "")
	result, err := h.workflow.ListReviewRequests(ctx, user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) reviewDecisions(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	svc, ok := h.workflow.(ReviewDecisionService)
	if !ok {
		jsonError(w, 503, "NOT_READY", "Daftar keputusan review belum tersedia.")
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	reviewID, err := positiveID(r.PathValue("reviewId"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID review tidak valid.")
		return
	}
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), dbID, "")
	decisions, err := svc.ListReviewDecisions(ctx, user, dbID, reviewID)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, decisions)
}

func (h *apiServices) decide(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID review tidak valid.")
		return
	}
	var input struct {
		Status  domain.ApprovalStatus `json:"status"`
		Comment string                `json:"comment"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Status != domain.ApprovalStatusApproved && input.Status != domain.ApprovalStatusRejected {
		jsonError(w, 400, "INVALID_STATUS", "Keputusan review tidak valid.")
		return
	}
	ctx := observability.WithActor(r.Context(), user.ID, user.Username)
	result, err := h.workflow.DecideApproval(ctx, user, id, input.Status, input.Comment)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) submit(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	var input domain.ExecutionRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	h.submitRequest(w, r, user, input)
}

func (h *apiServices) submitForDatabase(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	var input domain.ExecutionRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	input.DatabaseID = dbID
	h.submitRequest(w, r, user, input)
}

func (h *apiServices) submitRequest(w http.ResponseWriter, r *http.Request, user domain.User, input domain.ExecutionRequest) {
	if input.DatabaseID <= 0 || input.MigrationID <= 0 || input.RevisionID <= 0 || input.ApprovalRequestID <= 0 {
		jsonError(w, 400, "INVALID_INPUT", "Permintaan eksekusi tidak valid.")
		return
	}
	input.ActorID = user.ID
	input.RequestedAt = time.Now().UTC()
	ctx := observability.WithDatabase(observability.WithActor(r.Context(), user.ID, user.Username), input.DatabaseID, "")
	job, err := h.workflow.SubmitExecution(ctx, user, input)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
	jsonData(w, http.StatusAccepted, job)
}

func (h *apiServices) job(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok || !h.ready(w, h.workflow) {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 {
		jsonError(w, 400, "INVALID_ID", "ID job tidak valid.")
		return
	}
	ctx := observability.WithActor(r.Context(), user.ID, user.Username)
	result, err := h.workflow.GetJob(ctx, user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	response := spaJobResponse{Job: result}
	if h.actions != nil {
		actions, err := h.actions.Actions(ctx, user, result)
		if err != nil {
			slog.WarnContext(ctx, "could not work out the job's next actions", "job_id", result.ID, "error", err)
			actions = workflow.JobActions{}
		}
		response.JobActions = actions
	}
	if execSvc, ok := h.workflow.(ExecutionDetailService); ok && result.ExecutionID != "" {
		if execReq, _, err := execSvc.GetExecution(ctx, user, result.ExecutionID); err == nil {
			response.MigrationID = execReq.MigrationID
			response.RevisionID = execReq.RevisionID
			response.ApprovalRequestID = execReq.ApprovalRequestID
			response.Action = execReq.Action
			response.Mode = execReq.Mode
			response.TargetExecutionID = execReq.TargetExecutionID
		}
	}
	jsonData(w, 200, response)
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
