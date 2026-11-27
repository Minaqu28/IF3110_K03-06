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

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type JobActionAdvisor interface {
	Actions(ctx context.Context, actor domain.User, job domain.Job) (workflow.JobActions, error)
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

// WorkflowHandler exposes JSON API endpoints for revisions, reviews, execution history, and async jobs.
type WorkflowHandler struct {
	auth     *AuthenticationHandler
	queries  MigrationQueryService
	workflow WorkflowJobService
}

func NewWorkflowHandler(auth *AuthenticationHandler, queries MigrationQueryService, workflow WorkflowJobService) *WorkflowHandler {
	return &WorkflowHandler{auth: auth, queries: queries, workflow: workflow}
}

func (h *WorkflowHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid database ID")
		return
	}
	ctx, cancel := context.WithTimeout(observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), dbID, ""), databaseRequestTimeout)
	defer cancel()

	revisions, err := h.queries.ListMigrationRevisions(ctx, session.User, dbID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (h *WorkflowHandler) ListHistory(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid database ID")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("hide_rollback"))
	hideRollback := strings.EqualFold(raw, "true") || raw == "1"
	ctx, cancel := context.WithTimeout(observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), dbID, ""), databaseRequestTimeout)
	defer cancel()

	history, err := h.queries.ListExecutionHistory(ctx, session.User, dbID, hideRollback)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *WorkflowHandler) ListReviews(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid database ID")
		return
	}
	ctx, cancel := context.WithTimeout(observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), dbID, ""), databaseRequestTimeout)
	defer cancel()

	reviews, err := h.workflow.ListReviewRequests(ctx, session.User, dbID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviews)
}

type decideApprovalRequest struct {
	Status  domain.ApprovalStatus `json:"status"`
	Comment string                `json:"comment"`
}

func (h *WorkflowHandler) DecideApproval(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireJSONMutation(w, r)
	if !ok {
		return
	}
	reviewID, err := positiveID(r.PathValue("reviewId"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid review ID")
		return
	}
	var body decideApprovalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ctx, cancel := context.WithTimeout(observability.WithActor(r.Context(), session.User.ID, session.User.Username), databaseRequestTimeout)
	defer cancel()

	review, err := h.workflow.DecideApproval(ctx, session.User, reviewID, body.Status, body.Comment)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, review)
}

func (h *WorkflowHandler) SubmitExecution(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireJSONMutation(w, r)
	if !ok {
		return
	}
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid database ID")
		return
	}
	var req domain.ExecutionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.DatabaseID = dbID
	ctx, cancel := context.WithTimeout(observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), dbID, ""), databaseRequestTimeout)
	defer cancel()

	job, err := h.workflow.SubmitExecution(ctx, session.User, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *WorkflowHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	jobID := strings.TrimSpace(r.PathValue("jobId"))
	if jobID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid job ID")
		return
	}
	ctx, cancel := context.WithTimeout(observability.WithActor(r.Context(), session.User.ID, session.User.Username), databaseRequestTimeout)
	defer cancel()

	job, err := h.workflow.GetJob(ctx, session.User, jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *WorkflowHandler) requireJSONMutation(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return authentication.Session{}, false
	}
	if session.Previous {
		writeJSONError(w, http.StatusForbidden, "stale session identifier")
		return authentication.Session{}, false
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if !sameToken(csrf, session.CSRFToken) {
		writeJSONError(w, http.StatusForbidden, "invalid CSRF token")
		return authentication.Session{}, false
	}
	return session, true
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrInvalidInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, admin.ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, admin.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, admin.ErrConflict):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

type MigrationQueryService interface {
	ListMigrationRevisions(context.Context, domain.User, int64) ([]domain.MigrationRevision, error)
	ListExecutionHistory(context.Context, domain.User, int64, bool) ([]domain.ExecutionAttempt, error)
}

type WorkflowJobService interface {
	ListReviewRequests(context.Context, domain.User, int64) ([]domain.ReviewRequest, error)
	DecideApproval(context.Context, domain.User, int64, domain.ApprovalStatus, string) (domain.ReviewRequest, error)
	SubmitExecution(context.Context, domain.User, domain.ExecutionRequest) (domain.Job, error)
	GetJob(context.Context, domain.User, string) (domain.Job, error)
}

type RevisionWorkflowService interface {
	CreateRevision(context.Context, domain.User, domain.MigrationRevision) (domain.MigrationRevision, error)
	SubmitForReview(context.Context, domain.User, int64, int64) (domain.ReviewRequest, error)
}

type ActionReviewWorkflowService interface {
	SubmitActionForReview(context.Context, domain.User, int64, int64, domain.MigrationAction, *string) (domain.ReviewRequest, error)
}

type ReviewDecisionService interface {
	ListReviewDecisions(context.Context, domain.User, int64, int64) ([]domain.ApprovalDecision, error)
}

type ExecutionDetailService interface {
	GetExecution(context.Context, domain.User, string) (domain.ExecutionRequest, domain.ExecutionResult, error)
}
