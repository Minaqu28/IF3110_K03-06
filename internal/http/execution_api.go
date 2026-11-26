package httpserver

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	csrfHeader         = "X-CSRF-Token"
	maxSubmitBodyBytes = 16 << 10
)

type submitBody struct {
	JobID             string                 `json:"job_id"`
	ExecutionID       string                 `json:"execution_id"`
	MigrationID       int64                  `json:"migration_id"`
	RevisionID        int64                  `json:"revision_id"`
	ApprovalRequestID int64                  `json:"approval_request_id"`
	Action            domain.MigrationAction `json:"action"`
	Mode              domain.ExecutionMode   `json:"mode"`
	TargetExecutionID *string                `json:"target_execution_id"`
}

func (a *JobAPI) SubmitExecution(w http.ResponseWriter, r *http.Request) {
	session, ok := a.apiMutation(w, r)
	if !ok {
		return
	}
	databaseID, err := positiveID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid database ID"})
		return
	}
	var body submitBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmitBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid JSON body"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobRequestTimeout)
	defer cancel()
	job, err := a.submit.SubmitExecution(ctx, session.User, domain.ExecutionRequest{
		JobID: body.JobID, ExecutionID: body.ExecutionID, DatabaseID: databaseID, MigrationID: body.MigrationID,
		RevisionID: body.RevisionID, ApprovalRequestID: body.ApprovalRequestID, Action: body.Action, Mode: body.Mode,
		TargetExecutionID: body.TargetExecutionID,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *JobAPI) apiMutation(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
	session, ok := a.auth.apiSession(w, r)
	if !ok {
		return authentication.Session{}, false
	}
	if session.Previous {
		writeJSON(w, http.StatusForbidden, apiError{Error: "stale session identifier"})
		return authentication.Session{}, false
	}
	if !sameToken(r.Header.Get(csrfHeader), session.CSRFToken) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "invalid CSRF token"})
		return authentication.Session{}, false
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "use Content-Type: application/json"})
		return authentication.Session{}, false
	}
	return session, true
}
