package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
)

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
