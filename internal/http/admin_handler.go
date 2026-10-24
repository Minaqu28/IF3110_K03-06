package httpserver

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type AdminHandler struct {
	service *admin.AdminService
	auth    *AuthenticationHandler
	pages   *template.Template
}

func NewAdminHandler(service *admin.AdminService, auth *AuthenticationHandler, pages *template.Template) *AdminHandler {
	return &AdminHandler{service: service, auth: auth, pages: pages}
}

type adminPage struct {
	Session     authentication.Session
	Users       []domain.User
	Groups      []domain.Group
	Memberships []admin.Membership
	Connections []domain.Connection
	Grants      []admin.Grant
}

func (h *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request, mutation bool) (authentication.Session, bool) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return authentication.Session{}, false
	}
	if !authorization.CanManagePlatform(session.User) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return authentication.Session{}, false
	}
	if mutation {
		if session.Previous {
			http.Error(w, "stale session identifier", http.StatusForbidden)
			return authentication.Session{}, false
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return authentication.Session{}, false
		}
		if !sameToken(r.PostForm.Get("csrf_token"), session.CSRFToken) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return authentication.Session{}, false
		}
	}
	return session, true
}

func (h *AdminHandler) Show(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	data := adminPage{Session: session}
	var err error
	if data.Users, err = h.service.ListUsers(ctx, session.User); err != nil {
		adminHTTPError(w, err)
		return
	}
	if data.Groups, err = h.service.ListGroups(ctx, session.User); err != nil {
		adminHTTPError(w, err)
		return
	}
	if data.Memberships, err = h.service.ListMemberships(ctx, session.User); err != nil {
		adminHTTPError(w, err)
		return
	}
	if data.Connections, err = h.service.ListConnections(ctx, session.User); err != nil {
		adminHTTPError(w, err)
		return
	}
	if data.Grants, err = h.service.ListGrants(ctx, session.User); err != nil {
		adminHTTPError(w, err)
		return
	}
	renderPage(w, h.pages, "admin.html", data)
}

func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	_, err := h.service.CreateUser(r.Context(), session.User, admin.CreateUserInput{
		Username: r.PostForm.Get("username"),
		Password: r.PostForm.Get("password"),
		IsAdmin:  r.PostForm.Get("is_admin") == "on",
	})
	adminResult(w, r, err)
}

func (h *AdminHandler) SetUserActive(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("user_id"))
	active := r.PostForm.Get("active")
	if err != nil || (active != "true" && active != "false") {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	adminResult(w, r, h.service.SetUserActive(r.Context(), session.User, id, active == "true"))
}

func (h *AdminHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("user_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	err = h.service.UpdateUser(r.Context(), session.User, id, admin.UpdateUserInput{
		Username: r.PostForm.Get("username"), IsAdmin: r.PostForm.Get("is_admin") == "on",
	})
	if err == nil && id == session.User.ID {
		h.auth.clearCookie(w, sessionCookie, "/")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	adminResult(w, r, err)
}

func (h *AdminHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("user_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	err = h.service.ResetPassword(r.Context(), session.User, id, r.PostForm.Get("password"))
	if err == nil && id == session.User.ID {
		h.auth.clearCookie(w, sessionCookie, "/")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	adminResult(w, r, err)
}

func (h *AdminHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("user_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	adminResult(w, r, h.service.DeleteUser(r.Context(), session.User, id))
}

func (h *AdminHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	_, err := h.service.CreateGroup(r.Context(), session.User, r.PostForm.Get("name"))
	adminResult(w, r, err)
}

func (h *AdminHandler) RenameGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("group_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	adminResult(w, r, h.service.RenameGroup(r.Context(), session.User, id, r.PostForm.Get("name")))
}

func (h *AdminHandler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("group_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	adminResult(w, r, h.service.DeleteGroup(r.Context(), session.User, id))
}

func (h *AdminHandler) Membership(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	groupID, errGroup := positiveID(r.PostForm.Get("group_id"))
	userID, errUser := positiveID(r.PostForm.Get("user_id"))
	if errGroup != nil || errUser != nil {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	var err error
	switch r.PostForm.Get("action") {
	case "add":
		err = h.service.AddMember(r.Context(), session.User, groupID, userID)
	case "remove":
		err = h.service.RemoveMember(r.Context(), session.User, groupID, userID)
	default:
		err = admin.ErrInvalidInput
	}
	adminResult(w, r, err)
}

func (h *AdminHandler) CreateConnection(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	port, err := strconv.ParseInt(r.PostForm.Get("port"), 10, 64)
	if err != nil {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	_, err = h.service.CreateConnection(r.Context(), session.User, admin.CreateConnectionInput{
		Connection: domain.Connection{
			Name: r.PostForm.Get("name"), Host: r.PostForm.Get("host"), Port: port,
			DBname: r.PostForm.Get("db_name"), DBuser: r.PostForm.Get("db_user"), SSLMode: r.PostForm.Get("ssl_mode"),
		},
		Password: r.PostForm.Get("password"),
	})
	adminResult(w, r, err)
}

func (h *AdminHandler) UpdateConnection(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, errID := positiveID(r.PostForm.Get("database_id"))
	port, errPort := strconv.ParseInt(r.PostForm.Get("port"), 10, 64)
	if errID != nil || errPort != nil {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	err := h.service.UpdateConnection(r.Context(), session.User, admin.CreateConnectionInput{
		Connection: domain.Connection{
			ID: id, Name: r.PostForm.Get("name"), Host: r.PostForm.Get("host"), Port: port,
			DBname: r.PostForm.Get("db_name"), DBuser: r.PostForm.Get("db_user"), SSLMode: r.PostForm.Get("ssl_mode"),
		},
		Password: r.PostForm.Get("password"),
	})
	adminResult(w, r, err)
}

func (h *AdminHandler) DeleteConnection(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id, err := positiveID(r.PostForm.Get("database_id"))
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	adminResult(w, r, h.service.DeleteConnection(r.Context(), session.User, id))
}

func (h *AdminHandler) Grant(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r, true)
	if !ok {
		return
	}
	groupID, errGroup := positiveID(r.PostForm.Get("group_id"))
	dbID, errDB := positiveID(r.PostForm.Get("database_id"))
	if errGroup != nil || errDB != nil {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	var err error
	switch r.PostForm.Get("action") {
	case "grant":
		err = h.service.GrantAccess(r.Context(), session.User, groupID, dbID, authorization.AccessLevel(r.PostForm.Get("level")))
	case "revoke":
		err = h.service.RevokeAccess(r.Context(), session.User, groupID, dbID)
	default:
		err = admin.ErrInvalidInput
	}
	adminResult(w, r, err)
}

func (h *AdminHandler) ListDatabases(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.requireSession(w, r)
	if !ok {
		return
	}
	connections, err := h.service.ListAccessibleConnections(r.Context(), session.User)
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	renderPage(w, h.pages, "databases.html", struct {
		Session     authentication.Session
		Connections []domain.Connection
	}{session, connections})
}

func positiveID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, admin.ErrInvalidInput
	}
	return id, nil
}

func adminResult(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		adminHTTPError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func adminHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, admin.ErrInvalidInput):
		http.Error(w, "invalid input", http.StatusBadRequest)
	case errors.Is(err, admin.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, admin.ErrConflict):
		http.Error(w, "conflict", http.StatusConflict)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func renderPage(w http.ResponseWriter, pages *template.Template, name string, data any) {
	renderPageStatus(w, pages, name, data, http.StatusOK)
}

func renderPageStatus(w http.ResponseWriter, pages *template.Template, name string, data any, status int) {
	var body bytes.Buffer
	if err := pages.ExecuteTemplate(&body, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}
