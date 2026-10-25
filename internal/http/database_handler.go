package httpserver

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

const databaseRequestTimeout = 12 * time.Second

// DatabaseHandler serves the table viewer, SQL editor, and migration manager of a single database.
type DatabaseHandler struct {
	service *explorer.Service
	auth    *AuthenticationHandler
	pages   *template.Template
}

func NewDatabaseHandler(service *explorer.Service, auth *AuthenticationHandler, pages *template.Template) *DatabaseHandler {
	return &DatabaseHandler{service: service, auth: auth, pages: pages}
}

type databasePage struct {
	Session authentication.Session
	explorer.View
}

// Show renders GET /databases/{id}?schema=<name>&table=<name>&page=<number>.
func (h *DatabaseHandler) Show(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid database ID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()

	if migRaw := r.URL.Query().Get("migration"); migRaw != "" {
		version, err := positiveID(migRaw)
		if err != nil {
			http.Error(w, "invalid migration version", http.StatusBadRequest)
			return
		}
		view, err := h.service.ShowMigration(ctx, session.User, id, version)
		if err != nil {
			adminHTTPError(w, err)
			return
		}
		h.renderDatabaseView(w, session, view)
		return
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := positiveID(raw)
		if err != nil || parsed > 10000 {
			http.Error(w, "invalid page", http.StatusBadRequest)
			return
		}
		page = int(parsed)
	}
	view, err := h.service.BrowseTable(ctx, session.User, id, r.URL.Query().Get("schema"), r.URL.Query().Get("table"), page)
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	h.renderDatabaseView(w, session, view)
}

func (h *DatabaseHandler) renderDatabaseView(w http.ResponseWriter, session authentication.Session, view explorer.View) {
	status := http.StatusOK
	if view.Unavailable {
		status = http.StatusBadGateway
	}
	renderPageStatus(w, h.pages, "database.html", databasePage{Session: session, View: view}, status)
}

func (h *DatabaseHandler) requireMutationForm(w http.ResponseWriter, r *http.Request) (authentication.Session, int64, bool) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return authentication.Session{}, 0, false
	}
	if session.Previous {
		http.Error(w, "stale session identifier", http.StatusForbidden)
		return authentication.Session{}, 0, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*explorer.MaxSQLBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return authentication.Session{}, 0, false
	}
	if !sameToken(r.PostForm.Get("csrf_token"), session.CSRFToken) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return authentication.Session{}, 0, false
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid database ID", http.StatusBadRequest)
		return authentication.Session{}, 0, false
	}
	return session, id, true
}

func (h *DatabaseHandler) renderViewOrError(w http.ResponseWriter, session authentication.Session, view explorer.View, err error) {
	status := http.StatusOK
	if errors.Is(err, explorer.ErrReadOnly) {
		status, err = http.StatusForbidden, nil
	}
	if errors.Is(err, explorer.ErrAuditFailed) {
		status, err = http.StatusInternalServerError, nil
	}
	if errors.Is(err, explorer.ErrInvalidPlanQuery) {
		status, err = http.StatusBadRequest, nil
	}
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	if view.Unavailable {
		status = http.StatusBadGateway
	}
	renderPageStatus(w, h.pages, "database.html", databasePage{Session: session, View: view}, status)
}

func (h *DatabaseHandler) redirectMigration(w http.ResponseWriter, r *http.Request, view explorer.View, err error) bool {
	if err != nil || view.Error != "" || view.SelectedMigration == nil {
		return false
	}
	for _, result := range view.Results {
		if result.Error != "" {
			return false
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/databases/%d?migration=%d", view.Connection.ID, view.SelectedMigration.Version), http.StatusSeeOther)
	return true
}

func (h *DatabaseHandler) Explain(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.Explain(ctx, session.User, id, r.PostForm.Get("plan_sql"), r.PostForm.Get("mode"))
	h.renderViewOrError(w, session, view, err)
}

// Execute handles POST /databases/{id}/query.
func (h *DatabaseHandler) Execute(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.Execute(ctx, session.User, id, r.PostForm.Get("sql"))
	h.renderViewOrError(w, session, view, err)
}

// PreviewMigration handles POST /databases/{id}/migrations/preview.
func (h *DatabaseHandler) PreviewMigration(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	var version int64
	if raw := r.PostForm.Get("version"); raw != "" {
		v, err := positiveID(raw)
		if err != nil {
			http.Error(w, "invalid migration version", http.StatusBadRequest)
			return
		}
		version = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.PreviewMigration(ctx, session.User, id, version, r.PostForm.Get("title"), r.PostForm.Get("migration_sql"))
	h.renderViewOrError(w, session, view, err)
}

// CreateMigration handles POST /databases/{id}/migrations.
func (h *DatabaseHandler) CreateMigration(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.CreateMigration(ctx, session.User, id, r.PostForm.Get("title"), r.PostForm.Get("migration_sql"))
	if h.redirectMigration(w, r, view, err) {
		return
	}
	h.renderViewOrError(w, session, view, err)
}

// UpdateMigration handles POST /databases/{id}/migrations/{version}/update.
func (h *DatabaseHandler) UpdateMigration(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	version, err := positiveID(r.PathValue("version"))
	if err != nil {
		http.Error(w, "invalid migration version", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.UpdateMigration(ctx, session.User, id, version, r.PostForm.Get("title"), r.PostForm.Get("migration_sql"))
	if h.redirectMigration(w, r, view, err) {
		return
	}
	h.renderViewOrError(w, session, view, err)
}

// ExecuteMigration handles POST /databases/{id}/migrations/{version}/execute.
func (h *DatabaseHandler) ExecuteMigration(w http.ResponseWriter, r *http.Request) {
	session, id, ok := h.requireMutationForm(w, r)
	if !ok {
		return
	}
	version, err := positiveID(r.PathValue("version"))
	if err != nil {
		http.Error(w, "invalid migration version", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	view, err := h.service.ExecuteMigration(ctx, session.User, id, version)
	if h.redirectMigration(w, r, view, err) {
		return
	}
	h.renderViewOrError(w, session, view, err)
}
