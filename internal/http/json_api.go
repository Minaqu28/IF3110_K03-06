package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
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

func (h *AuthenticationHandler) spaSession(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
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
		slog.WarnContext(r.Context(), "api login failed",
			slog.String("event", "auth.login"),
			slog.String("actor", input.Username),
			slog.String("status", "unauthorized"),
		)
		jsonError(w, 401, "INVALID_CREDENTIALS", "Username atau password salah.")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "api login error",
			slog.String("event", "auth.login"),
			slog.String("actor", input.Username),
			slog.String("status", "error"),
			slog.String("error", err.Error()),
		)
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return
	}
	slog.InfoContext(r.Context(), "api login succeeded",
		slog.String("event", "auth.login"),
		slog.String("actor", input.Username),
		slog.String("status", "ok"),
	)
	h.clearCookie(w, loginCSRFCookie, "/api/v1/auth/login")
	h.setCookie(w, sessionCookie, identifier, "/", 0)
	jsonData(w, 200, map[string]bool{"authenticated": true})
}

func (h *AuthenticationHandler) apiCurrentSession(w http.ResponseWriter, r *http.Request) {
	session, ok := h.spaSession(w, r)
	if !ok {
		return
	}
	jsonData(w, 200, map[string]any{"user": map[string]any{"id": session.User.ID, "username": session.User.Username, "is_platform_admin": session.User.IsPlatformAdmin}, "csrf_token": session.CSRFToken})
}

func (h *AuthenticationHandler) apiLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := h.spaSession(w, r)
	if !ok {
		return
	}
	if session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	ctx := observability.WithActor(r.Context(), session.User.ID, session.User.Username)
	if err := h.service.Logout(ctx, cookie.Value); err != nil {
		slog.ErrorContext(ctx, "api logout failed",
			slog.String("event", "auth.logout"),
			slog.String("status", "error"),
			slog.String("error", err.Error()),
		)
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
		return
	}
	slog.InfoContext(ctx, "api logout succeeded",
		slog.String("event", "auth.logout"),
		slog.String("status", "ok"),
	)
	h.clearCookie(w, sessionCookie, "/")
	w.WriteHeader(http.StatusNoContent)
}

func apiServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrForbidden), errors.Is(err, explorer.ErrReadOnly):
		jsonError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, admin.ErrNotFound):
		jsonError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, admin.ErrInvalidInput), errors.Is(err, explorer.ErrInvalidPlanQuery):
		jsonError(w, 400, "INVALID_INPUT", publicMessage(err, admin.ErrInvalidInput))
	case errors.Is(err, workflow.ErrGroupMissingLeader):
		jsonError(w, 409, "MISSING_GROUP_LEADER", "Setiap grup yang memiliki akses ke database harus memiliki leader terlebih dahulu.")
	case errors.Is(err, workflow.ErrNoApprovers):
		jsonError(w, 409, "NO_APPROVERS", "Belum ada group leader yang ditetapkan untuk database ini.")
	case errors.Is(err, admin.ErrConflict):
		jsonError(w, 409, "CONFLICT", publicMessage(err, admin.ErrConflict))
	default:
		slog.Error("spa api request failed", slog.String("event", "api.error"), slog.String("error", err.Error()))
		jsonError(w, 500, "INTERNAL_ERROR", "Terjadi kesalahan internal.")
	}
}

func connectionJSON(c domain.Connection) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name}
}

func adminConnectionJSON(c domain.Connection) map[string]any {
	return map[string]any{
		"id":       c.ID,
		"name":     c.Name,
		"host":     c.Host,
		"port":     c.Port,
		"db_name":  c.DBname,
		"db_user":  c.DBuser,
		"ssl_mode": c.SSLMode,
	}
}

func (h *AdminHandler) apiDatabases(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.spaSession(w, r)
	if !ok {
		return
	}
	ctx := observability.WithActor(r.Context(), session.User.ID, session.User.Username)
	connections, err := h.service.ListAccessibleConnections(ctx, session.User)
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
	session, ok := h.auth.spaSession(w, r)
	if !ok {
		return
	}
	if !authorization.CanManagePlatform(session.User) {
		jsonError(w, 403, "FORBIDDEN", "Akses admin ditolak.")
		return
	}
	ctx, actor := observability.WithActor(r.Context(), session.User.ID, session.User.Username), session.User
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
		groupData = append(groupData, map[string]any{
			"id":              g.ID,
			"name":            g.Name,
			"leader_id":       g.LeaderID,
			"leader_username": g.LeaderUsername,
		})
	}
	membershipData := make([]map[string]any, 0, len(memberships))
	for _, m := range memberships {
		membershipData = append(membershipData, map[string]any{"group_id": m.GroupID, "group_name": m.GroupName, "user_id": m.UserID, "username": m.Username})
	}
	connectionData := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		connectionData = append(connectionData, adminConnectionJSON(c))
	}
	grantData := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		grantData = append(grantData, map[string]any{"group_id": g.GroupID, "group_name": g.GroupName, "database_id": g.DatabaseID, "database_name": g.DatabaseName, "level": g.Level})
	}
	data := map[string]any{"users": userData, "groups": groupData, "memberships": membershipData, "connections": connectionData, "grants": grantData}
	jsonData(w, 200, data)
}

func (h *AdminHandler) apiAdminAction(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.spaSession(w, r)
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
		LeaderID   *int64                    `json:"leader_id"`
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
	actor := session.User
	ctx := observability.WithActor(r.Context(), actor.ID, actor.Username)
	userID := in.UserID
	if userID == 0 {
		userID = in.ID
	}
	groupID := in.GroupID
	if groupID == 0 {
		groupID = in.ID
	}
	dbID := in.DatabaseID
	if dbID == 0 {
		dbID = in.ID
	}
	var err error
	renew := false
	switch in.Action {
	case "create_user":
		_, err = h.service.CreateUser(ctx, actor, admin.CreateUserInput{Username: in.Username, Password: in.Password, IsAdmin: in.IsAdmin})
	case "update_user":
		err = h.service.UpdateUser(ctx, actor, userID, admin.UpdateUserInput{Username: in.Username, IsAdmin: in.IsAdmin})
		renew = userID == actor.ID
	case "set_user_active":
		err = h.service.SetUserActive(ctx, actor, userID, in.Active)
	case "reset_password":
		err = h.service.ResetPassword(ctx, actor, userID, in.Password)
		renew = userID == actor.ID
	case "delete_user":
		err = h.service.DeleteUser(ctx, actor, userID)
	case "create_group":
		_, err = h.service.CreateGroup(ctx, actor, in.Name)
	case "rename_group":
		err = h.service.RenameGroup(ctx, actor, groupID, in.Name)
	case "set_group_leader":
		var leaderID *int64
		if in.LeaderID != nil && *in.LeaderID > 0 {
			id := *in.LeaderID
			leaderID = &id
		}
		err = h.service.SetGroupLeader(ctx, actor, groupID, leaderID)
		if err == nil {
			var lid int64
			if leaderID != nil {
				lid = *leaderID
			}
			slog.InfoContext(ctx, "group leader updated via api",
				slog.String("event", "admin.group.set_leader"),
				slog.Int64("group_id", groupID),
				slog.Int64("leader_id", lid),
				slog.String("status", "ok"),
			)
		}
	case "delete_group":
		err = h.service.DeleteGroup(ctx, actor, groupID)
		renew = true
	case "add_member":
		err = h.service.AddMember(ctx, actor, groupID, userID)
		renew = userID == actor.ID
	case "remove_member":
		err = h.service.RemoveMember(ctx, actor, groupID, userID)
		renew = userID == actor.ID
	case "grant":
		err = h.service.GrantAccess(ctx, actor, groupID, dbID, in.Level)
		renew = true
	case "revoke":
		err = h.service.RevokeAccess(ctx, actor, groupID, dbID)
		renew = true
	case "create_connection":
		_, err = h.service.CreateConnection(ctx, actor, admin.CreateConnectionInput{Connection: domain.Connection{Name: in.Name, Host: in.Host, Port: in.Port, DBname: in.DBName, DBuser: in.DBUser, SSLMode: in.SSLMode}, Password: in.Password})
	case "update_connection":
		err = h.service.UpdateConnection(ctx, actor, admin.CreateConnectionInput{Connection: domain.Connection{ID: dbID, Name: in.Name, Host: in.Host, Port: in.Port, DBname: in.DBName, DBuser: in.DBUser, SSLMode: in.SSLMode}, Password: in.Password})
		renew = true
	case "delete_connection":
		err = h.service.DeleteConnection(ctx, actor, dbID)
		renew = true
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

func statementResultJSON(res explorer.StatementResult) map[string]any {
	rows := make([][]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		cells := make([]map[string]any, 0, len(row))
		for _, cell := range row {
			cells = append(cells, map[string]any{"value": cell.Value, "null": cell.Null})
		}
		rows = append(rows, cells)
	}
	return map[string]any{
		"command":   res.Command,
		"columns":   res.Columns,
		"rows":      rows,
		"truncated": res.Truncated,
		"error":     res.Error,
	}
}

func (h *DatabaseHandler) apiDatabase(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.spaSession(w, r)
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
	ctx, cancel := context.WithTimeout(
		observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), id, ""),
		databaseRequestTimeout,
	)
	defer cancel()
	view, err := h.service.BrowseTable(ctx, session.User, id, r.URL.Query().Get("schema"), r.URL.Query().Get("table"), int(page))
	if err != nil {
		apiServiceError(w, err)
		return
	}
	if view.Unavailable {
		jsonError(w, 502, "TARGET_UNAVAILABLE", view.Error)
		return
	}
	tables := make([]map[string]any, 0, len(view.Tables))
	for _, table := range view.Tables {
		tables = append(tables, map[string]any{"schema": table.Schema, "name": table.Name, "type": table.Type, "rows": table.Rows})
	}
	var preview any
	if view.Preview != nil {
		preview = statementResultJSON(*view.Preview)
	}
	var details any
	if view.Details != nil {
		cols := make([]map[string]any, 0, len(view.Details.Columns))
		for _, c := range view.Details.Columns {
			cols = append(cols, map[string]any{"name": c.Name, "type": c.Type, "nullable": c.Nullable, "default": c.Default})
		}
		constraints := make([]map[string]any, 0, len(view.Details.Constraints))
		for _, c := range view.Details.Constraints {
			constraints = append(constraints, map[string]any{"name": c.Name, "type": c.Type, "definition": c.Definition})
		}
		indexes := make([]map[string]any, 0, len(view.Details.Indexes))
		for _, idx := range view.Details.Indexes {
			indexes = append(indexes, map[string]any{"name": idx.Name, "definition": idx.Definition})
		}
		details = map[string]any{"columns": cols, "constraints": constraints, "indexes": indexes}
	}
	changes := make([]map[string]any, 0, len(view.Changes))
	for _, ch := range view.Changes {
		changes = append(changes, map[string]any{
			"id": ch.ID, "migration_version": ch.MigrationVersion, "source": ch.Source,
			"sql": ch.SQL, "status": ch.Status, "error": ch.Error,
			"duration_ms": ch.DurationMs, "username": ch.Username, "created_at": ch.CreatedAt,
		})
	}
	jsonData(w, 200, map[string]any{
		"connection": connectionJSON(view.Connection),
		"level":      view.Level,
		"can_edit":   view.CanEdit(),
		"schemas":    view.Schemas,
		"schema":     view.Schema,
		"tables":     tables,
		"table":      view.Table,
		"details":    details,
		"preview":    preview,
		"page":       view.Page,
		"has_prev":   view.HasPrev,
		"has_next":   view.HasNext,
		"changes":    changes,
		"error":      view.Error,
	})
}

func (h *DatabaseHandler) apiQuery(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.spaSession(w, r)
	if !ok {
		return
	}
	if session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	var input struct {
		SQL string `json:"sql"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(
		observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), id, ""),
		databaseRequestTimeout,
	)
	defer cancel()
	view, err := h.service.Execute(ctx, session.User, id, input.SQL)
	if errors.Is(err, explorer.ErrReadOnly) {
		jsonError(w, 403, "READ_ONLY", view.Error)
		return
	}
	if err != nil {
		apiServiceError(w, err)
		return
	}
	if view.Unavailable {
		jsonError(w, 502, "TARGET_UNAVAILABLE", view.Error)
		return
	}
	results := make([]map[string]any, 0, len(view.Results))
	for _, res := range view.Results {
		results = append(results, statementResultJSON(res))
	}
	jsonData(w, 200, map[string]any{"results": results, "error": view.Error})
}

func (h *DatabaseHandler) apiExplain(w http.ResponseWriter, r *http.Request) {
	session, ok := h.auth.spaSession(w, r)
	if !ok {
		return
	}
	if session.Previous || !sameToken(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		jsonError(w, 403, "INVALID_CSRF", "Token CSRF tidak valid.")
		return
	}
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		jsonError(w, 400, "INVALID_ID", "ID database tidak valid.")
		return
	}
	var input struct {
		SQL  string `json:"sql"`
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(
		observability.WithDatabase(observability.WithActor(r.Context(), session.User.ID, session.User.Username), id, ""),
		databaseRequestTimeout,
	)
	defer cancel()
	view, err := h.service.Explain(ctx, session.User, id, input.SQL, input.Mode)
	if errors.Is(err, explorer.ErrInvalidPlanQuery) {
		jsonError(w, 400, "INVALID_PLAN_QUERY", view.Error)
		return
	}
	if err != nil {
		apiServiceError(w, err)
		return
	}
	if view.Unavailable {
		jsonError(w, 502, "TARGET_UNAVAILABLE", view.Error)
		return
	}
	jsonData(w, 200, map[string]any{"mode": view.PlanMode, "plan": view.Plan, "error": view.Error})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func publicMessage(err, kind error) string {
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, kind.Error()+": "); ok {
		return rest
	}
	return message
}
