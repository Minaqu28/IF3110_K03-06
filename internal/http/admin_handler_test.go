package httpserver

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type recordingAdminStore struct {
	admin.AdminStore
	calls []string
}

func (s *recordingAdminStore) rec(n string) { s.calls = append(s.calls, n) }

func (s *recordingAdminStore) CreateUser(_ context.Context, name, _ string, isAdmin bool) (domain.User, error) {
	s.rec("CreateUser")
	return domain.User{ID: 10, Username: name, IsPlatformAdmin: isAdmin, IsActive: true}, nil
}
func (s *recordingAdminStore) SetUserActive(context.Context, int64, bool) error {
	s.rec("SetUserActive")
	return nil
}
func (s *recordingAdminStore) UpdateUser(context.Context, int64, string, bool) error {
	s.rec("UpdateUser")
	return nil
}
func (s *recordingAdminStore) ResetPassword(context.Context, int64, string) error {
	s.rec("ResetPassword")
	return nil
}
func (s *recordingAdminStore) DeleteUser(context.Context, int64) error {
	s.rec("DeleteUser")
	return nil
}
func (s *recordingAdminStore) CreateGroup(_ context.Context, n string) (domain.Group, error) {
	s.rec("CreateGroup")
	return domain.Group{ID: 1, Name: n}, nil
}
func (s *recordingAdminStore) DeleteGroup(context.Context, int64) error {
	s.rec("DeleteGroup")
	return nil
}
func (s *recordingAdminStore) GrantAccess(context.Context, int64, int64, authorization.AccessLevel) error {
	s.rec("GrantAccess")
	return nil
}
func (s *recordingAdminStore) RevokeAccess(context.Context, int64, int64) error {
	s.rec("RevokeAccess")
	return nil
}
func (s *recordingAdminStore) AddMember(context.Context, int64, int64) error {
	s.rec("AddMember")
	return nil
}
func (s *recordingAdminStore) RemoveMember(context.Context, int64, int64) error {
	s.rec("RemoveMember")
	return nil
}
func (s *recordingAdminStore) UpdateConnection(context.Context, domain.Connection, string) error {
	s.rec("UpdateConnection")
	return nil
}
func (s *recordingAdminStore) DeleteConnection(context.Context, int64) error {
	s.rec("DeleteConnection")
	return nil
}
func (s *recordingAdminStore) ListUsers(context.Context) ([]domain.User, error) {
	return []domain.User{{ID: 1, Username: "root", PasswordHash: "SECRET-HASH"}}, nil
}
func (s *recordingAdminStore) ListGroups(context.Context) ([]domain.Group, error) { return nil, nil }
func (s *recordingAdminStore) ListMemberships(context.Context) ([]admin.Membership, error) {
	return nil, nil
}
func (s *recordingAdminStore) ListConnections(context.Context) ([]domain.Connection, error) {
	return nil, nil
}
func (s *recordingAdminStore) ListGrants(context.Context) ([]admin.Grant, error) { return nil, nil }
func (s *recordingAdminStore) AccessibleConnections(context.Context, int64) ([]domain.Connection, error) {
	s.rec("AccessibleConnections")
	return []domain.Connection{{ID: 3, Name: "visible-db"}}, nil
}

const adminCSRF = "admin-csrf"

func newAdminRouter(t *testing.T, session authentication.Session) (http.Handler, *recordingAdminStore) {
	t.Helper()
	pages := template.Must(template.New("x").Parse(
		`{{define "login.html"}}login{{end}}{{define "dashboard.html"}}dash{{end}}` +
			`{{define "admin.html"}}admin-page{{range .Users}}[{{.Username}}|{{.PasswordHash}}]{{end}}{{end}}` +
			`{{define "databases.html"}}{{range .Connections}}{{.Name}}{{end}}{{end}}`))
	auth, err := NewAuthenticationHandler(fakeAuth{session: session}, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingAdminStore{}
	h := NewAdminHandler(admin.NewService(store, nil), auth, pages)
	return NewApplicationRouter(auth, h, nil, nil, func(context.Context) error { return nil }), store
}

func sessionFor(user domain.User) authentication.Session {
	return authentication.Session{User: user, CSRFToken: adminCSRF}
}

var (
	rootUser    = domain.User{ID: 1, Username: "root", IsPlatformAdmin: true, IsActive: true}
	regularUser = domain.User{ID: 2, Username: "bob", IsActive: true}
)

func adminMutations() map[string]url.Values {
	return map[string]url.Values{
		"/admin/users":        {"username": {"usagi"}, "password": {"unana yaha yaha"}},
		"/admin/users/status": {"user_id": {"5"}, "active": {"false"}},
		"/admin/users/delete": {"user_id": {"5"}},
		"/admin/groups":       {"name": {"devs"}},
		"/admin/memberships":  {"action": {"add"}, "group_id": {"1"}, "user_id": {"5"}},
		"/admin/grants":       {"action": {"grant"}, "group_id": {"1"}, "database_id": {"1"}, "level": {"VIEW_ONLY"}},
	}
}

func TestAdminPagesRequireLogin(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(rootUser))
	for _, path := range []string{"/admin", "/databases"} {
		rec := do(router, http.MethodGet, path, nil, false)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("GET %s anonymous: %d -> %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	for path, form := range adminMutations() {
		form.Set("csrf_token", adminCSRF)
		rec := do(router, http.MethodPost, path, form, false)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("POST %s anonymous: %d -> %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if len(store.calls) != 0 {
		t.Errorf("anonymous requests reached the store: %v", store.calls)
	}
}

func TestNonAdminCannotUseAdminEndpoints(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(regularUser))
	if rec := do(router, http.MethodGet, "/admin", nil, true); rec.Code != http.StatusForbidden {
		t.Errorf("GET /admin as regular user = %d, want 403", rec.Code)
	}
	for path, form := range adminMutations() {
		form.Set("csrf_token", adminCSRF)
		if rec := do(router, http.MethodPost, path, form, true); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as regular user = %d, want 403", path, rec.Code)
		}
	}
	if len(store.calls) != 0 {
		t.Errorf("forbidden requests reached the store: %v", store.calls)
	}
}

func TestAdminMutationsRejectMissingOrWrongCSRF(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(rootUser))
	for path, form := range adminMutations() {
		none := url.Values{}
		for k, v := range form {
			none[k] = v
		}
		if rec := do(router, http.MethodPost, path, none, true); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF = %d, want 403", path, rec.Code)
		}
		wrong := url.Values{"csrf_token": {"nope"}}
		for k, v := range form {
			wrong[k] = v
		}
		if rec := do(router, http.MethodPost, path, wrong, true); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with wrong CSRF = %d, want 403", path, rec.Code)
		}
	}
	if len(store.calls) != 0 {
		t.Errorf("CSRF-rejected requests reached the store: %v", store.calls)
	}
}

func TestAdminMutationsRejectGracePeriodIdentifier(t *testing.T) {
	sess := sessionFor(rootUser)
	sess.Previous = true
	router, store := newAdminRouter(t, sess)
	for path, form := range adminMutations() {
		form.Set("csrf_token", adminCSRF)
		if rec := do(router, http.MethodPost, path, form, true); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with pre-rotation identifier = %d, want 403", path, rec.Code)
		}
	}
	if len(store.calls) != 0 {
		t.Errorf("stale-session requests reached the store: %v", store.calls)
	}
	if rec := do(router, http.MethodGet, "/admin", nil, true); rec.Code != http.StatusOK {
		t.Errorf("GET /admin with grace identifier = %d, want 200", rec.Code)
	}
}

func TestAdminMutationsSucceedForAdminWithCSRF(t *testing.T) {
	want := map[string]string{
		"/admin/users": "CreateUser", "/admin/users/status": "SetUserActive", "/admin/users/delete": "DeleteUser",
		"/admin/groups": "CreateGroup", "/admin/memberships": "AddMember", "/admin/grants": "GrantAccess",
	}
	for path, form := range adminMutations() {
		router, store := newAdminRouter(t, sessionFor(rootUser))
		form.Set("csrf_token", adminCSRF)
		rec := do(router, http.MethodPost, path, form, true)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin" {
			t.Errorf("POST %s = %d -> %q, want 303 -> /admin", path, rec.Code, rec.Header().Get("Location"))
			continue
		}
		if len(store.calls) != 1 || store.calls[0] != want[path] {
			t.Errorf("POST %s store calls = %v, want [%s]", path, store.calls, want[path])
		}
	}
}

func TestAdminSelfMembershipRenewsSessionWithoutLoginRedirect(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(rootUser))
	form := url.Values{
		"action":     {"add"},
		"group_id":   {"1"},
		"user_id":    {"1"},
		"csrf_token": {adminCSRF},
	}
	rec := do(router, http.MethodPost, "/admin/memberships", form, true)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("self membership = %d -> %q, want 303 -> /admin", rec.Code, rec.Header().Get("Location"))
	}
	cookie := findCookie(rec, sessionCookie)
	if cookie == nil || cookie.Value != "renewed-session" || !cookie.HttpOnly {
		t.Fatalf("renewed cookie = %+v", cookie)
	}
	if len(store.calls) != 1 || store.calls[0] != "AddMember" {
		t.Fatalf("store calls = %v", store.calls)
	}
}

func TestPrivilegeAffectingAdminMutationsRenewSession(t *testing.T) {
	tests := []struct {
		name string
		path string
		form url.Values
		call string
	}{
		{"update own account", "/admin/users/update", url.Values{"user_id": {"1"}, "username": {"root"}, "is_admin": {"on"}}, "UpdateUser"},
		{"reset own password", "/admin/users/password", url.Values{"user_id": {"1"}, "password": {"new-long-password"}}, "ResetPassword"},
		{"delete group", "/admin/groups/delete", url.Values{"group_id": {"1"}}, "DeleteGroup"},
		{"remove self membership", "/admin/memberships", url.Values{"action": {"remove"}, "group_id": {"1"}, "user_id": {"1"}}, "RemoveMember"},
		{"update connection", "/admin/connections/update", url.Values{
			"database_id": {"1"}, "name": {"target"}, "host": {"db"}, "port": {"5432"},
			"db_name": {"target"}, "db_user": {"app"}, "ssl_mode": {"disable"},
		}, "UpdateConnection"},
		{"delete connection", "/admin/connections/delete", url.Values{"database_id": {"1"}}, "DeleteConnection"},
		{"grant access", "/admin/grants", url.Values{"action": {"grant"}, "group_id": {"1"}, "database_id": {"1"}, "level": {"VIEW_EDIT"}}, "GrantAccess"},
		{"revoke access", "/admin/grants", url.Values{"action": {"revoke"}, "group_id": {"1"}, "database_id": {"1"}}, "RevokeAccess"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, store := newAdminRouter(t, sessionFor(rootUser))
			tt.form.Set("csrf_token", adminCSRF)
			rec := do(router, http.MethodPost, tt.path, tt.form, true)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin" {
				t.Fatalf("status=%d location=%q, want 303 -> /admin", rec.Code, rec.Header().Get("Location"))
			}
			cookie := findCookie(rec, sessionCookie)
			if cookie == nil || cookie.Value != "renewed-session" || !cookie.HttpOnly {
				t.Fatalf("renewed cookie = %+v", cookie)
			}
			if len(store.calls) != 1 || store.calls[0] != tt.call {
				t.Fatalf("store calls = %v, want [%s]", store.calls, tt.call)
			}
		})
	}
}

func TestAdminInputValidationAndSelfProtection(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(rootUser))
	post := func(path string, form url.Values) int {
		form.Set("csrf_token", adminCSRF)
		return do(router, http.MethodPost, path, form, true).Code
	}
	if code := post("/admin/users/status", url.Values{"user_id": {"abc"}, "active": {"true"}}); code != http.StatusBadRequest {
		t.Errorf("non-numeric id = %d, want 400", code)
	}
	if code := post("/admin/users/status", url.Values{"user_id": {"5"}, "active": {"maybe"}}); code != http.StatusBadRequest {
		t.Errorf("bad active flag = %d, want 400", code)
	}
	if code := post("/admin/grants", url.Values{"action": {"grant"}, "group_id": {"1"}, "database_id": {"1"}, "level": {"OWNER"}}); code != http.StatusBadRequest {
		t.Errorf("unknown access level = %d, want 400", code)
	}
	if code := post("/admin/memberships", url.Values{"action": {"teleport"}, "group_id": {"1"}, "user_id": {"5"}}); code != http.StatusBadRequest {
		t.Errorf("unknown action = %d, want 400", code)
	}
	if code := post("/admin/groups", url.Values{"name": {"   "}}); code != http.StatusBadRequest {
		t.Errorf("blank group name = %d, want 400", code)
	}
	if code := post("/admin/users", url.Values{"username": {"usagi"}, "password": {"short"}}); code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", code)
	}
	if code := post("/admin/users/status", url.Values{"user_id": {"1"}, "active": {"false"}}); code != http.StatusConflict {
		t.Errorf("admin deactivating self = %d, want 409", code)
	}
	if code := post("/admin/users/delete", url.Values{"user_id": {"1"}}); code != http.StatusConflict {
		t.Errorf("admin deleting self = %d, want 409", code)
	}
	if len(store.calls) != 0 {
		t.Errorf("invalid requests reached the store: %v", store.calls)
	}
}

func TestAdminPageNeverRendersPasswordHashes(t *testing.T) {
	router, _ := newAdminRouter(t, sessionFor(rootUser))
	rec := do(router, http.MethodGet, "/admin", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "root") {
		t.Fatalf("admin page did not render users: %q", body)
	}
	if strings.Contains(body, "SECRET-HASH") {
		t.Error("password hash leaked into the admin page")
	}
}

func TestAnyActiveUserSeesOnlyTheirAccessibleDatabases(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(regularUser))
	rec := do(router, http.MethodGet, "/databases", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "visible-db") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 1 || store.calls[0] != "AccessibleConnections" {
		t.Errorf("store calls = %v", store.calls)
	}
}
