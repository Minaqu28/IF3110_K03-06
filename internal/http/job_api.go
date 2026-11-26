package httpserver

import (
	"net/http"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func (h *apiServices) ready(w http.ResponseWriter, service any) bool {
	if service == nil {
		jsonError(w, 503, "NOT_READY", "Service fitur ini belum tersedia.")
		return false
	}
	return true
}

func (h *apiServices) session(w http.ResponseWriter, r *http.Request, mutation bool) (domain.User, bool) {
	session, ok := h.auth.apiSession(w, r)
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
	if !ok {
		return
	}
	if !h.ready(w, h.migrations) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	result, err := h.migrations.ListMigrationRevisions(r.Context(), user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) history(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok {
		return
	}
	if !h.ready(w, h.migrations) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	result, err := h.migrations.ListExecutionHistory(r.Context(), user, id, r.URL.Query().Get("hide_rollback") == "true")
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) reviews(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok {
		return
	}
	if !h.ready(w, h.workflow) {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	result, err := h.workflow.ListReviewRequests(r.Context(), user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) decide(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok {
		return
	}
	if !h.ready(w, h.workflow) {
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
	result, err := h.workflow.DecideApproval(r.Context(), user, id, input.Status, input.Comment)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}

func (h *apiServices) submit(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, true)
	if !ok {
		return
	}
	if !h.ready(w, h.workflow) {
		return
	}
	var input domain.ExecutionRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.DatabaseID <= 0 || input.MigrationID <= 0 || input.RevisionID <= 0 || input.ApprovalRequestID <= 0 {
		jsonError(w, 400, "INVALID_INPUT", "Permintaan eksekusi tidak valid.")
		return
	}
	input.JobID, input.ExecutionID, input.ActorID, input.RequestedAt = "", "", user.ID, time.Now().UTC()
	job, err := h.workflow.SubmitExecution(r.Context(), user, input)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, http.StatusAccepted, job)
}

func (h *apiServices) job(w http.ResponseWriter, r *http.Request) {
	user, ok := h.session(w, r, false)
	if !ok {
		return
	}
	if !h.ready(w, h.workflow) {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 {
		jsonError(w, 400, "INVALID_ID", "ID job tidak valid.")
		return
	}
	result, err := h.workflow.GetJob(r.Context(), user, id)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	jsonData(w, 200, result)
}
