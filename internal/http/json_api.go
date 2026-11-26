package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

func jsonData(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
}

func jsonError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		jsonError(w, 400, "INVALID_JSON", "Body JSON tidak valid.")
		return false
	}
	return true
}

func (h *AuthenticationHandler) apiSession(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
	session, err := h.session(w, r)
	if errors.Is(err, authentication.ErrUnauthenticated) {
		jsonError(w, 401, "UNAUTHENTICATED", "Silakan masuk terlebih dahulu.")
		return authentication.Session{}, false
	}
	if err != nil {
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return authentication.Session{}, false
	}
	return session, true
}

func (h *AuthenticationHandler) apiLoginCSRF(w http.ResponseWriter, _ *http.Request) {
	token, err := h.newLoginCSRF()
	if err != nil {
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return
	}
	h.setCookie(w, loginCSRFCookie, token, "/api/v1/auth/login", 600)
	jsonData(w, 200, map[string]string{"csrf_token": token})
}

func (h *AuthenticationHandler) apiLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	cookie, err := r.Cookie(loginCSRFCookie)
	if err != nil || !sameToken(cookie.Value, r.Header.Get("X-CSRF-Token")) || !h.validLoginCSRF(cookie.Value) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	identifier, err := h.service.Login(r.Context(), input.Username, input.Password)
	if errors.Is(err, authentication.ErrInvalidCredentials) {
		jsonError(w, 401, "INVALID_CREDENTIALS", "Username atau password salah.")
		return
	}
	if err != nil {
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return
	}
	h.clearCookie(w, loginCSRFCookie, "/api/v1/auth/login")
	h.setCookie(w, sessionCookie, identifier, "/", 0)
	jsonData(w, 200, map[string]bool{"authenticated": true})
}

func (h *AuthenticationHandler) apiCurrentSession(w http.ResponseWriter, r *http.Request) {
	session, ok := h.apiSession(w, r)
	if !ok {
		return
	}
	jsonData(w, 200, map[string]any{"user": map[string]any{"id": session.User.ID, "username": session.User.Username, "is_platform_admin": session.User.IsPlatformAdmin}, "csrf_token": session.CSRFToken})
}

func (h *AuthenticationHandler) apiLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := h.apiSession(w, r)
	if !ok {
		return
	}
	if session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	if err := h.service.Logout(r.Context(), cookie.Value); err != nil {
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return
	}
	h.clearCookie(w, sessionCookie, "/")
	w.WriteHeader(http.StatusNoContent)
}

func apiServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrForbidden), errors.Is(err, explorer.ErrReadOnly):
		jsonError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, admin.ErrNotFound):
		jsonError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, admin.ErrInvalidInput):
		jsonError(w, 400, "INVALID_INPUT", "Input tidak valid.")
	case errors.Is(err, admin.ErrConflict):
		jsonError(w, 409, "CONFLICT", "Operasi bertentangan dengan kondisi saat ini.")
	default:
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
	}
}

func connectionJSON(c domain.Connection) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name}
}

func (h *AdminHandler) apiDatabases(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.apiSession(w, r)
	if !ok {
		return
	}
	connections, err := h.service.ListAccessibleConnections(r.Context(), session.User)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		result = append(result, connectionJSON(c))
	}
	jsonData(w, 200, result)
}

func (h *AdminHandler) apiAdmin(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.apiSession(w, r)
	if !ok {
		return
	}
	if !authorization.CanManagePlatform(session.User) {
		jsonError(w, 403, "FORBIDDEN", "Akses admin ditolak.")
		return
	}
	ctx, actor := r.Context(), session.User
	users, err := h.service.ListUsers(ctx, actor)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	groups, err := h.service.ListGroups(ctx, actor)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	memberships, err := h.service.ListMemberships(ctx, actor)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	connections, err := h.service.ListConnections(ctx, actor)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	grants, err := h.service.ListGrants(ctx, actor)
	if err != nil {
		apiServiceError(w, err)
		return
	}
	userData := make([]map[string]any, 0, len(users))
	for _, u := range users {
		userData = append(userData, map[string]any{"id": u.ID, "username": u.Username, "is_platform_admin": u.IsPlatformAdmin, "is_active": u.IsActive})
	}
	groupData := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		groupData = append(groupData, map[string]any{"id": g.ID, "name": g.Name})
	}
	membershipData := make([]map[string]any, 0, len(memberships))
	for _, m := range memberships {
		membershipData = append(membershipData, map[string]any{"group_id": m.GroupID, "group_name": m.GroupName, "user_id": m.UserID, "username": m.Username})
	}
	connectionData := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		connectionData = append(connectionData, connectionJSON(c))
	}
	grantData := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		grantData = append(grantData, map[string]any{"group_id": g.GroupID, "group_name": g.GroupName, "database_id": g.DatabaseID, "database_name": g.DatabaseName, "level": g.Level})
	}
	data := map[string]any{"users": userData, "groups": groupData, "memberships": membershipData, "connections": connectionData, "grants": grantData}
	jsonData(w, 200, data)
}

func (h *AdminHandler) apiAdminAction(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.apiSession(w, r)
	if !ok {
		return
	}
	if !authorization.CanManagePlatform(session.User) {
		jsonError(w, 403, "FORBIDDEN", "Akses admin ditolak.")
		return
	}
	if session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	var in struct {
		Action     string                    `json:"action"`
		ID         int64                     `json:"id"`
		GroupID    int64                     `json:"group_id"`
		UserID     int64                     `json:"user_id"`
		DatabaseID int64                     `json:"database_id"`
		Username   string                    `json:"username"`
		Password   string                    `json:"password"`
		IsAdmin    bool                      `json:"is_admin"`
		Active     bool                      `json:"active"`
		Name       string                    `json:"name"`
		Host       string                    `json:"host"`
		Port       int64                     `json:"port"`
		DBName     string                    `json:"db_name"`
		DBUser     string                    `json:"db_user"`
		SSLMode    string                    `json:"ssl_mode"`
		Level      authorization.AccessLevel `json:"level"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	actor, ctx := session.User, r.Context()
	var err error
	renew := false
	switch in.Action {
	case "create_user":
		_, err = h.service.CreateUser(ctx, actor, admin.CreateUserInput{Username: in.Username, Password: in.Password, IsAdmin: in.IsAdmin})
	case "create_group":
		_, err = h.service.CreateGroup(ctx, actor, in.Name)
	case "add_member":
		err = h.service.AddMember(ctx, actor, in.GroupID, in.UserID)
		renew = in.UserID == actor.ID
	case "remove_member":
		err = h.service.RemoveMember(ctx, actor, in.GroupID, in.UserID)
		renew = in.UserID == actor.ID
	case "grant":
		err = h.service.GrantAccess(ctx, actor, in.GroupID, in.DatabaseID, in.Level)
		renew = true
	case "create_connection":
		_, err = h.service.CreateConnection(ctx, actor, admin.CreateConnectionInput{Connection: domain.Connection{Name: in.Name, Host: in.Host, Port: in.Port, DBname: in.DBName, DBuser: in.DBUser, SSLMode: in.SSLMode}, Password: in.Password})
	default:
		jsonError(w, 400, "INVALID_ACTION", "Operasi admin belum didukung.")
		return
	}
	if err != nil {
		apiServiceError(w, err)
		return
	}
	if renew {
		if err = h.auth.renewAfterPrivilegeChange(w, r, session); err != nil {
			h.auth.clearCookie(w, sessionCookie, "/")
			jsonError(w, 500, "INTERNAL_ERROR", "Gagal memperbarui sesi.")
			return
		}
	}
	jsonData(w, 200, map[string]bool{"ok": true})
}

func (h *DatabaseHandler) apiDatabase(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.apiSession(w, r)
	if !ok {
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	page := int64(1)
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = positiveID(raw)
		if err != nil || page > 10000 {
			jsonError(w, 400, "INVALID_PAGE", "Halaman tidak valid.")
			return
		}
	}
	view, err := h.service.BrowseTable(r.Context(), session.User, id, r.URL.Query().Get("schema"), r.URL.Query().Get("table"), int(page))
	if err != nil {
		apiServiceError(w, err)
		return
	}
	if view.Unavailable {
		jsonError(w, 502, "TARGET_UNAVAILABLE", "Database target tidak tersedia.")
		return
	}
	tables := make([]map[string]any, 0, len(view.Tables))
	for _, table := range view.Tables {
		tables = append(tables, map[string]any{"schema": table.Schema, "name": table.Name, "type": table.Type, "rows": table.Rows})
	}
	var preview any
	if view.Preview != nil {
		rows := make([][]map[string]any, 0, len(view.Preview.Rows))
		for _, row := range view.Preview.Rows {
			cells := make([]map[string]any, 0, len(row))
			for _, cell := range row {
				cells = append(cells, map[string]any{"value": cell.Value, "null": cell.Null})
			}
			rows = append(rows, cells)
		}
		preview = map[string]any{"columns": view.Preview.Columns, "rows": rows, "truncated": view.Preview.Truncated}
	}
	jsonData(w, 200, map[string]any{"connection": connectionJSON(view.Connection), "level": view.Level, "schemas": view.Schemas, "schema": view.Schema, "tables": tables, "table": view.Table, "preview": preview, "page": view.Page, "has_prev": view.HasPrev, "has_next": view.HasNext})
}
