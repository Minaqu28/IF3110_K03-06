package database_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	httpserver "github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Every test uses its own PostgreSQL schema, so TEST_DATABASE_URL can point to
// a shared disposable test instance without truncating another schema.
func integrationDB(t *testing.T) *sql.DB {
	return isolatedDB(t, true)
}

func isolatedDB(t *testing.T, migrate bool) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	base, err := database.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "it_" + hex.EncodeToString(suffix[:])
	if _, err := base.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.RuntimeParams["search_path"] = schema
	config.RuntimeParams["client_encoding"] = "UTF8"
	db := stdlib.OpenDB(*config)
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = base.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = base.Close()
	})
	if migrate {
		if err := database.Migrate(ctx, db, migration.Files); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestSinglePlatformInitMigrationPostgres(t *testing.T) {
	db := isolatedDB(t, false)
	ctx := context.Background()
	files, err := fs.Glob(migration.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "001_init.sql" {
		t.Fatalf("platform migration files = %v, want only 001_init.sql", files)
	}
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND (table_name, column_name) IN (
		      ('sessions', 'previous_valid_until'),
		      ('migrations', 'duration_ms'),
		      ('migrations', 'updated_at')
		  )`).Scan(&columns); err != nil || columns != 3 {
		t.Fatalf("consolidated columns=%d err=%v", columns, err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('database_change_log', 'migration_attempts')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("consolidated tables=%d err=%v", tables, err)
	}
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
}

func insertUser(t *testing.T, db *sql.DB, username, password string, isAdmin bool) domain.User {
	t.Helper()
	hash, err := authentication.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{Username: username, PasswordHash: hash, IsPlatformAdmin: isAdmin, IsActive: true}
	err = db.QueryRow(`INSERT INTO users (username, password_hash, is_platform_admin) VALUES ($1, $2, $3) RETURNING id`, username, hash, isAdmin).Scan(&u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newCredentialKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestSessionLifecyclePostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	insertUser(t, db, "alice", "alice-long-password", false)
	var clock atomic.Int64
	start := time.Now().UTC().Truncate(time.Second)
	clock.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	setTime := func(at time.Time) { clock.Store(at.UnixNano()) }
	svc := authentication.NewService(database.NewPostgresStore(db), now)
	if _, err := svc.Login(ctx, "alice", "wrong-password"); !errors.Is(err, authentication.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("wrong login created session: count=%d err=%v", count, err)
	}
	id, err := svc.Login(ctx, "alice", "alice-long-password")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := svc.Validate(ctx, id)
	if err != nil || initial.Previous || initial.RotatedIdentifier != "" {
		t.Fatalf("initial validation: %#v, %v", initial, err)
	}
	setTime(start.Add(authentication.RotationInterval))
	rotated, err := svc.Validate(ctx, id)
	if err != nil || rotated.RotatedIdentifier == "" || rotated.Previous {
		t.Fatalf("rotation: %#v, %v", rotated, err)
	}
	newID := rotated.RotatedIdentifier
	var absoluteExpiry time.Time
	if err := db.QueryRow(`SELECT absolute_expires_at FROM sessions WHERE id = $1`, initial.ID).Scan(&absoluteExpiry); err != nil || !absoluteExpiry.Equal(start.Add(authentication.AbsoluteLifetime)) {
		t.Fatalf("rotation changed absolute expiry: %v, %v", absoluteExpiry, err)
	}
	setTime(start.Add(authentication.RotationInterval + 10*time.Second))
	previous, err := svc.Validate(ctx, id)
	if err != nil || !previous.Previous || previous.RotatedIdentifier != "" {
		t.Fatalf("previous during grace: %#v, %v", previous, err)
	}
	var lastSeen time.Time
	if err := db.QueryRow(`SELECT last_seen_at FROM sessions WHERE id = $1`, initial.ID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	if !lastSeen.Equal(start.Add(authentication.RotationInterval)) {
		t.Fatal("previous identifier extended idle activity")
	}
	setTime(start.Add(authentication.RotationInterval + authentication.PreviousGrace + time.Second))
	if _, err := svc.Validate(ctx, id); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("old identifier accepted after grace: %v", err)
	}
	if _, err := svc.Validate(ctx, newID); err != nil {
		t.Fatalf("current identifier rejected: %v", err)
	}
	if err := svc.Logout(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Validate(ctx, newID); err != nil {
		t.Fatalf("expired previous identifier revoked current session: %v", err)
	}
	setTime(start.Add(authentication.RotationInterval + authentication.PreviousGrace + time.Second + authentication.IdleTimeout + time.Second))
	if _, err := svc.Validate(ctx, newID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("idle-expired session accepted: %v", err)
	}

	setTime(start)
	absID, err := svc.Login(ctx, "alice", "alice-long-password")
	if err != nil {
		t.Fatal(err)
	}
	absSession, err := svc.Validate(ctx, absID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, absSession.ID, start.Add(authentication.AbsoluteLifetime-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	setTime(start.Add(authentication.AbsoluteLifetime))
	if _, err := svc.Validate(ctx, absID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("absolute-expired session accepted: %v", err)
	}

	setTime(start)
	logoutID, err := svc.Login(ctx, "alice", "alice-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, logoutID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Validate(ctx, logoutID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("revoked session accepted: %v", err)
	}
	setTime(start)
	graceID, err := svc.Login(ctx, "alice", "alice-long-password")
	if err != nil {
		t.Fatal(err)
	}
	setTime(start.Add(authentication.RotationInterval))
	graceRotation, err := svc.Validate(ctx, graceID)
	if err != nil {
		t.Fatal(err)
	}
	setTime(start.Add(authentication.RotationInterval + 10*time.Second))
	if err := svc.Logout(ctx, graceID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Validate(ctx, graceRotation.RotatedIdentifier); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("logout with previous ID did not revoke current ID: %v", err)
	}
}

func TestConcurrentRotationPostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	insertUser(t, db, "bob", "bob-long-password", false)
	start := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	svc := authentication.NewService(database.NewPostgresStore(db), func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	id, err := svc.Login(ctx, "bob", "bob-long-password")
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(authentication.RotationInterval).UnixNano())
	const callers = 8
	results := make(chan authentication.Session, callers)
	errorsCh := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := svc.Validate(ctx, id)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- session
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	var rotated, previous int
	for session := range results {
		if session.RotatedIdentifier != "" {
			rotated++
		} else if session.Previous {
			previous++
		}
	}
	if rotated != 1 || previous != callers-1 {
		t.Fatalf("concurrent rotation: rotated=%d previous=%d", rotated, previous)
	}
}

func TestPreviousIdentifierCannotMutateAdminPostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	insertUser(t, db, "admin", "admin-long-password", true)
	start := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	auth := authentication.NewService(database.NewPostgresStore(db), func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	store, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pages := template.Must(template.ParseFS(frontend.Files, "pages/*.html"))
	authHandler, err := httpserver.NewAuthenticationHandler(auth, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	router := httpserver.NewApplicationRouter(authHandler, httpserver.NewAdminHandler(admin.NewService(store, nil), authHandler, pages), nil, nil, db.PingContext)
	id, err := auth.Login(ctx, "admin", "admin-long-password")
	if err != nil {
		t.Fatal(err)
	}
	session, err := auth.Validate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(authentication.RotationInterval).UnixNano())
	get := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	get.AddCookie(&http.Cookie{Name: "cookie_session", Value: id})
	getResult := httptest.NewRecorder()
	router.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("dashboard rotation status: %d", getResult.Code)
	}
	var rotatedID string
	for _, cookie := range getResult.Result().Cookies() {
		if cookie.Name == "cookie_session" {
			rotatedID = cookie.Value
		}
	}
	if rotatedID == "" || rotatedID == id {
		t.Fatal("rotation did not issue a new cookie")
	}
	post := func(identifier string) int {
		form := url.Values{"csrf_token": {session.CSRFToken}, "name": {"fresh"}}
		r := httptest.NewRequest(http.MethodPost, "/admin/groups", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "cookie_session", Value: identifier})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Code
	}
	if got := post(id); got != http.StatusForbidden {
		t.Fatalf("old identifier mutated admin data: %d", got)
	}
	if got := post(rotatedID); got != http.StatusSeeOther {
		t.Fatalf("new identifier rejected: %d", got)
	}
}

func TestAdminAuthorizationAndHTTPPostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	adminUser := insertUser(t, db, "root", "root-long-password", true)
	viewer := insertUser(t, db, "viewer", "viewer-long-password", false)
	store, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	adminService := admin.NewService(store, nil)
	if _, err := adminService.CreateGroup(ctx, viewer, "forbidden"); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("nonadmin created group: %v", err)
	}
	if err := adminService.UpdateUser(ctx, viewer, viewer.ID, admin.UpdateUserInput{Username: "elevated", IsAdmin: true}); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("nonadmin elevated account: %v", err)
	}
	group, err := adminService.CreateGroup(ctx, adminUser, "viewers")
	if err != nil {
		t.Fatal(err)
	}
	connection := func(name string) domain.Connection {
		c, err := adminService.CreateConnection(ctx, adminUser, admin.CreateConnectionInput{
			Connection: domain.Connection{Name: name, Host: "127.0.0.1", Port: 5432, DBname: name, DBuser: "test", SSLMode: "disable"},
			Password:   "target-secret",
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	allowed := connection("allowed")
	denied := connection("denied")
	var encrypted []byte
	if err := db.QueryRow(`SELECT password_encrypted FROM database_connections WHERE id = $1`, allowed.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if string(encrypted) == "target-secret" || strings.Contains(string(encrypted), "target-secret") {
		t.Fatal("target password was stored as plaintext")
	}
	if got, err := store.DecryptPassword(encrypted); err != nil || got != "target-secret" {
		t.Fatalf("credential decryption failed: %v", err)
	}
	if err := adminService.AddMember(ctx, adminUser, group.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if err := adminService.GrantAccess(ctx, adminUser, group.ID, allowed.ID, authorization.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if level, err := adminService.AccessFor(ctx, viewer, allowed.ID); err != nil || level != authorization.AccessViewOnly {
		t.Fatalf("effective access: %q, %v", level, err)
	}
	if _, err := adminService.ConnectionFor(ctx, viewer, denied.ID); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("cross-target access: %v", err)
	}
	if _, err := adminService.ConnectionFor(ctx, adminUser, allowed.ID); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("admin bypassed target grant: %v", err)
	}
	visible, err := adminService.ListAccessibleConnections(ctx, viewer)
	if err != nil || len(visible) != 1 || visible[0].ID != allowed.ID {
		t.Fatalf("visible connections: %#v, %v", visible, err)
	}

	auth := authentication.NewService(database.NewPostgresStore(db), nil)
	pages := template.Must(template.ParseFS(frontend.Files, "pages/*.html"))
	authHandler, err := httpserver.NewAuthenticationHandler(auth, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	databaseHandler := httpserver.NewDatabaseHandler(explorer.NewService(adminService, store), authHandler, pages)
	router := httpserver.NewApplicationRouter(authHandler, httpserver.NewAdminHandler(adminService, authHandler, pages), databaseHandler, nil, db.PingContext)
	adminID, err := auth.Login(ctx, "root", "root-long-password")
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := auth.Login(ctx, "viewer", "viewer-long-password")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, id string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if form == nil {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if id != "" {
			r.AddCookie(&http.Cookie{Name: "cookie_session", Value: id})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if got := request(http.MethodGet, "/admin", "", nil).Code; got != http.StatusSeeOther {
		t.Fatalf("guest admin status: %d", got)
	}
	if got := request(http.MethodGet, "/admin", viewerID, nil).Code; got != http.StatusForbidden {
		t.Fatalf("nonadmin admin status: %d", got)
	}
	adminView := request(http.MethodGet, "/admin", adminID, nil)
	if adminView.Code != http.StatusOK || !strings.Contains(adminView.Body.String(), "Administrasi Platform") || strings.Contains(adminView.Body.String(), "target-secret") {
		t.Fatalf("admin page render or credential exposure: status=%d", adminView.Code)
	}
	if got := request(http.MethodPost, "/admin/groups", viewerID, url.Values{"name": {"bad"}}).Code; got != http.StatusForbidden {
		t.Fatalf("nonadmin mutation status: %d", got)
	}
	if got := request(http.MethodPost, "/admin/groups", adminID, url.Values{"name": {"bad"}}).Code; got != http.StatusForbidden {
		t.Fatalf("missing CSRF status: %d", got)
	}
	adminSession, err := auth.Validate(ctx, adminID)
	if err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodPost, "/admin/groups", adminID, url.Values{"name": {"editors"}, "csrf_token": {adminSession.CSRFToken}}).Code; got != http.StatusSeeOther {
		t.Fatalf("admin create group status: %d", got)
	}
	var selfGroupID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM groups WHERE name = 'editors'`).Scan(&selfGroupID); err != nil {
		t.Fatal(err)
	}
	selfMembership := request(http.MethodPost, "/admin/memberships", adminID, url.Values{
		"action": {"add"}, "group_id": {fmt.Sprint(selfGroupID)}, "user_id": {fmt.Sprint(adminUser.ID)}, "csrf_token": {adminSession.CSRFToken},
	})
	if selfMembership.Code != http.StatusSeeOther || selfMembership.Header().Get("Location") != "/admin" {
		t.Fatalf("self membership status=%d location=%q", selfMembership.Code, selfMembership.Header().Get("Location"))
	}
	var renewedAdminID string
	for _, cookie := range selfMembership.Result().Cookies() {
		if cookie.Name == "cookie_session" && cookie.MaxAge >= 0 {
			renewedAdminID = cookie.Value
		}
	}
	if renewedAdminID == "" || renewedAdminID == adminID {
		t.Fatalf("self membership did not issue a replacement session")
	}
	if _, err := auth.Validate(ctx, adminID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("old admin identifier survived self membership: %v", err)
	}
	adminID = renewedAdminID
	adminSession, err = auth.Validate(ctx, adminID)
	if err != nil {
		t.Fatalf("replacement admin session invalid: %v", err)
	}
	if got := request(http.MethodGet, "/admin", adminID, nil).Code; got != http.StatusOK {
		t.Fatalf("admin page after self membership status=%d", got)
	}
	selfGrant := request(http.MethodPost, "/admin/grants", adminID, url.Values{
		"action": {"grant"}, "group_id": {fmt.Sprint(selfGroupID)}, "database_id": {fmt.Sprint(allowed.ID)},
		"level": {string(authorization.AccessViewEdit)}, "csrf_token": {adminSession.CSRFToken},
	})
	if selfGrant.Code != http.StatusSeeOther || selfGrant.Header().Get("Location") != "/admin" {
		t.Fatalf("self group grant status=%d location=%q", selfGrant.Code, selfGrant.Header().Get("Location"))
	}
	renewedAdminID = ""
	for _, cookie := range selfGrant.Result().Cookies() {
		if cookie.Name == "cookie_session" && cookie.MaxAge >= 0 {
			renewedAdminID = cookie.Value
		}
	}
	if renewedAdminID == "" || renewedAdminID == adminID {
		t.Fatalf("self group grant did not issue a replacement session")
	}
	if _, err := auth.Validate(ctx, adminID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("old admin identifier survived self group grant: %v", err)
	}
	adminID = renewedAdminID
	adminSession, err = auth.Validate(ctx, adminID)
	if err != nil {
		t.Fatalf("replacement session after self group grant invalid: %v", err)
	}
	if got := request(http.MethodGet, "/admin", adminID, nil).Code; got != http.StatusOK {
		t.Fatalf("admin page after self group grant status=%d", got)
	}
	if got := request(http.MethodGet, fmt.Sprintf("/databases/%d", denied.ID), viewerID, nil).Code; got != http.StatusForbidden {
		t.Fatalf("cross-target HTTP status: %d", got)
	}
	allowedView := request(http.MethodGet, fmt.Sprintf("/databases/%d", allowed.ID), viewerID, nil)
	if allowedView.Code != http.StatusBadGateway || !strings.Contains(allowedView.Body.String(), "Tidak dapat terhubung") || strings.Contains(allowedView.Body.String(), "target-secret") {
		t.Fatalf("unreachable allowed target: status=%d body=%q", allowedView.Code, allowedView.Body.String())
	}
	list := request(http.MethodGet, "/databases", viewerID, nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "/databases/"+fmt.Sprint(denied.ID)+`"`) {
		t.Fatalf("list exposed unauthorized target: code=%d", list.Code)
	}
	if got := request(http.MethodPost, "/logout", adminID, url.Values{"csrf_token": {"bad"}}).Code; got != http.StatusForbidden {
		t.Fatalf("logout with bad CSRF status: %d", got)
	}
	if _, err := auth.Validate(ctx, adminID); err != nil {
		t.Fatalf("bad CSRF revoked session: %v", err)
	}
	if _, err := adminService.CreateUser(ctx, viewer, admin.CreateUserInput{Username: "intruder", Password: "strong-password"}); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("nonadmin created user: %v", err)
	}
	if err := adminService.RevokeAccess(ctx, adminUser, group.ID, allowed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Validate(ctx, viewerID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("session survived grant revocation: %v", err)
	}
	if got := request(http.MethodPost, "/logout", adminID, url.Values{"csrf_token": {adminSession.CSRFToken}}).Code; got != http.StatusSeeOther {
		t.Fatalf("valid logout status: %d", got)
	}
	if _, err := auth.Validate(ctx, adminID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("logout did not revoke admin: %v", err)
	}

	editorGroup, err := adminService.CreateGroup(ctx, adminUser, "editors")
	if !errors.Is(err, admin.ErrConflict) || editorGroup.ID != 0 {
		t.Fatalf("duplicate group did not conflict: %v", err)
	}
	editorGroup, err = adminService.CreateGroup(ctx, adminUser, "editor-grant")
	if err != nil {
		t.Fatal(err)
	}
	if err := adminService.GrantAccess(ctx, adminUser, group.ID, allowed.ID, authorization.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if err := adminService.GrantAccess(ctx, adminUser, editorGroup.ID, allowed.ID, authorization.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	viewerID, err = auth.Login(ctx, "viewer", "viewer-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := adminService.AddMember(ctx, adminUser, editorGroup.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Validate(ctx, viewerID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("membership change did not revoke session: %v", err)
	}
	if level, err := adminService.AccessFor(ctx, viewer, allowed.ID); err != nil || level != authorization.AccessViewEdit {
		t.Fatalf("multi-group access: %q, %v", level, err)
	}
	if err := adminService.RemoveMember(ctx, adminUser, editorGroup.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if level, err := adminService.AccessFor(ctx, viewer, allowed.ID); err != nil || level != authorization.AccessViewOnly {
		t.Fatalf("access after member removal: %q, %v", level, err)
	}
	if err := adminService.RenameGroup(ctx, adminUser, editorGroup.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := adminService.DeleteGroup(ctx, adminUser, editorGroup.ID); err != nil {
		t.Fatal(err)
	}
	denied.Name = "renamed-target"
	if err := adminService.UpdateConnection(ctx, adminUser, admin.CreateConnectionInput{Connection: denied, Password: "new-target-secret"}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetConnection(ctx, denied.ID); err != nil || got.Name != "renamed-target" {
		t.Fatalf("connection update: %#v, %v", got, err)
	}
	if err := db.QueryRow(`SELECT password_encrypted FROM database_connections WHERE id = $1`, denied.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if got, err := store.DecryptPassword(encrypted); err != nil || got != "new-target-secret" {
		t.Fatalf("connection password update: %q, %v", got, err)
	}
	if err := adminService.DeleteConnection(ctx, adminUser, denied.ID); err != nil {
		t.Fatal(err)
	}
	if err := adminService.DeleteUser(ctx, adminUser, adminUser.ID); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("admin deleted self: %v", err)
	}
	if err := adminService.ResetPassword(ctx, adminUser, viewer.ID, "viewer-new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Login(ctx, "viewer", "viewer-long-password"); !errors.Is(err, authentication.ErrInvalidCredentials) {
		t.Fatalf("old password still worked: %v", err)
	}
	if _, err := auth.Login(ctx, "viewer", "viewer-new-password"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	adminID, err = auth.Login(ctx, "root", "root-long-password")
	if err != nil {
		t.Fatal(err)
	}
	adminSession, err = auth.Validate(ctx, adminID)
	if err != nil {
		t.Fatal(err)
	}
	reset := request(http.MethodPost, "/admin/users/password", adminID, url.Values{
		"user_id": {fmt.Sprint(adminUser.ID)}, "password": {"root-new-password"}, "csrf_token": {adminSession.CSRFToken},
	})
	if reset.Code != http.StatusSeeOther || reset.Header().Get("Location") != "/admin" {
		t.Fatalf("self password reset status=%d location=%q", reset.Code, reset.Header().Get("Location"))
	}
	if _, err := auth.Validate(ctx, adminID); !errors.Is(err, authentication.ErrUnauthenticated) {
		t.Fatalf("old session survived self password reset: %v", err)
	}
	renewedAdminID = ""
	for _, cookie := range reset.Result().Cookies() {
		if cookie.Name == "cookie_session" && cookie.MaxAge >= 0 {
			renewedAdminID = cookie.Value
		}
	}
	if renewedAdminID == "" || renewedAdminID == adminID {
		t.Fatal("self password reset did not issue a replacement session")
	}
	if _, err := auth.Validate(ctx, renewedAdminID); err != nil {
		t.Fatalf("replacement session after self password reset invalid: %v", err)
	}
	if _, err := auth.Login(ctx, "root", "root-new-password"); err != nil {
		t.Fatalf("new admin password rejected: %v", err)
	}
}

func TestExplorerEndToEndPostgres(t *testing.T) {
	rootDSN := os.Getenv("TEST_POSTGRES_DSN")
	if rootDSN == "" {
		t.Skip("set TEST_POSTGRES_DSN to run target integration")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	targetName := fmt.Sprintf("dbmp_target_%d", time.Now().UnixNano())
	targetID := pgx.Identifier{targetName}.Sanitize()
	if _, err := root.Exec(ctx, `CREATE DATABASE `+targetID); err != nil {
		root.Close(ctx)
		t.Fatalf("create isolated target database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `DROP DATABASE `+targetID+` WITH (FORCE)`)
		_ = root.Close(context.Background())
	})
	config, err := pgx.ParseConfig(rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = targetName
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close(context.Background()) })
	schema := fmt.Sprintf("app_%d", time.Now().UnixNano())
	qualified := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := target.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(ctx, `CREATE TABLE `+qualified+` (id integer PRIMARY KEY, label text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(ctx, `INSERT INTO `+qualified+` SELECT i, 'row-' || i::text FROM generate_series(1, 51) AS i`); err != nil {
		t.Fatal(err)
	}

	platform := integrationDB(t)
	adminUser := insertUser(t, platform, "root", "root-long-password", true)
	viewer := insertUser(t, platform, "viewer", "viewer-long-password", false)
	store, err := database.NewAdminPostgresStore(platform, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	adminService := admin.NewService(store, nil)
	connection, err := adminService.CreateConnection(ctx, adminUser, admin.CreateConnectionInput{
		Connection: domain.Connection{Name: "isolated target", Host: config.Host, Port: int64(config.Port), DBname: targetName, DBuser: config.User, SSLMode: "disable"},
		Password:   config.Password,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := adminService.CreateConnection(ctx, adminUser, admin.CreateConnectionInput{
		Connection: domain.Connection{Name: "no access", Host: config.Host, Port: int64(config.Port), DBname: targetName, DBuser: config.User, SSLMode: "disable"},
		Password:   config.Password,
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := adminService.CreateGroup(ctx, adminUser, "editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := adminService.AddMember(ctx, adminUser, group.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if err := adminService.GrantAccess(ctx, adminUser, group.ID, connection.ID, authorization.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	auth := authentication.NewService(database.NewPostgresStore(platform), nil)
	identifier, err := auth.Login(ctx, "viewer", "viewer-long-password")
	if err != nil {
		t.Fatal(err)
	}
	session, err := auth.Validate(ctx, identifier)
	if err != nil {
		t.Fatal(err)
	}
	pages := template.Must(template.ParseFS(frontend.Files, "pages/*.html"))
	authHandler, err := httpserver.NewAuthenticationHandler(auth, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	databaseHandler := httpserver.NewDatabaseHandler(explorer.NewService(adminService, store), authHandler, pages)
	router := httpserver.NewApplicationRouter(authHandler, httpserver.NewAdminHandler(adminService, authHandler, pages), databaseHandler, nil, platform.PingContext)
	request := func(method, path, token string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if form == nil {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.AddCookie(&http.Cookie{Name: "cookie_session", Value: token})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	path := fmt.Sprintf("/databases/%d", connection.ID)
	if got := request(http.MethodGet, fmt.Sprintf("/databases/%d", other.ID), identifier, nil).Code; got != http.StatusForbidden {
		t.Fatalf("cross-target viewer status=%d", got)
	}
	firstPage := request(http.MethodGet, path+"?schema="+schema+"&table=items", identifier, nil)
	if firstPage.Code != http.StatusOK || !strings.Contains(firstPage.Body.String(), "row-1") || !strings.Contains(firstPage.Body.String(), "items_pkey") || !strings.Contains(firstPage.Body.String(), "Berikutnya") {
		t.Fatalf("first viewer page status=%d body=%q", firstPage.Code, firstPage.Body.String())
	}
	secondPage := request(http.MethodGet, path+"?schema="+schema+"&table=items&page=2", identifier, nil)
	if secondPage.Code != http.StatusOK || !strings.Contains(secondPage.Body.String(), "row-51") || !strings.Contains(secondPage.Body.String(), "Sebelumnya") {
		t.Fatalf("second viewer page status=%d body=%q", secondPage.Code, secondPage.Body.String())
	}
	createForm := url.Values{"title": {"add note"}, "migration_sql": {"ALTER TABLE " + qualified + " ADD COLUMN note text; UPDATE " + qualified + " SET note = 'ok' WHERE id = 1"}, "csrf_token": {session.CSRFToken}}
	created := request(http.MethodPost, path+"/migrations", identifier, createForm)
	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != path+"?migration=1" {
		t.Fatalf("create migration status=%d location=%q", created.Code, created.Header().Get("Location"))
	}
	executed := request(http.MethodPost, path+"/migrations/1/execute", identifier, url.Values{"csrf_token": {session.CSRFToken}})
	if executed.Code != http.StatusSeeOther || executed.Header().Get("Location") != path+"?migration=1" {
		t.Fatalf("execute migration status=%d body=%q", executed.Code, executed.Body.String())
	}
	var note string
	if err := target.QueryRow(ctx, `SELECT note FROM `+qualified+` WHERE id = 1`).Scan(&note); err != nil || note != "ok" {
		t.Fatalf("target migration effect=%q err=%v", note, err)
	}
	var ledgerVersion int64
	if err := target.QueryRow(ctx, `SELECT version FROM public._dbmp_migrations`).Scan(&ledgerVersion); err != nil || ledgerVersion != 1 {
		t.Fatalf("target ledger version=%d err=%v", ledgerVersion, err)
	}
	failedTable := pgx.Identifier{schema, "rolled_back"}.Sanitize()
	failedForm := url.Values{"title": {"fail after DDL"}, "migration_sql": {"CREATE TABLE " + failedTable + " (id integer); SELECT 1/0"}, "csrf_token": {session.CSRFToken}}
	if got := request(http.MethodPost, path+"/migrations", identifier, failedForm).Code; got != http.StatusSeeOther {
		t.Fatalf("create failing migration status=%d", got)
	}
	failed := request(http.MethodPost, path+"/migrations/2/execute", identifier, url.Values{"csrf_token": {session.CSRFToken}})
	if failed.Code != http.StatusOK || !strings.Contains(failed.Body.String(), "division by zero") {
		t.Fatalf("failed migration status=%d body=%q", failed.Code, failed.Body.String())
	}
	var rolledBack bool
	if err := target.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, schema+".rolled_back").Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("failed SQL was not rolled back: missing=%v err=%v", rolledBack, err)
	}
	var ledgerCount int
	if err := target.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_migrations`).Scan(&ledgerCount); err != nil || ledgerCount != 1 {
		t.Fatalf("failed migration changed ledger: count=%d err=%v", ledgerCount, err)
	}
	var successfulAttempts, failedAttempts int
	if err := platform.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status = 'SUCCESS'), count(*) FILTER (WHERE status = 'FAILED') FROM migration_attempts WHERE database_id = $1`, connection.ID).
		Scan(&successfulAttempts, &failedAttempts); err != nil || successfulAttempts != 1 || failedAttempts != 1 {
		t.Fatalf("attempt audit success=%d failed=%d err=%v", successfulAttempts, failedAttempts, err)
	}
	retry, err := store.UpdateMigration(ctx, explorer.MigrationInput{
		DatabaseID: connection.ID, Version: 2, Title: "retry after rollback", SQL: "CREATE TABLE " + failedTable + " (id integer)",
	})
	if err != nil {
		t.Fatal(err)
	}
	abandonedAttempt, err := store.BeginMigrationAttempt(ctx, retry, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	manualTarget, err := store.OpenTarget(ctx, connection.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	versioned := manualTarget.(explorer.VersionedTarget)
	manualResults, already, err := versioned.ApplyVersionedMigration(ctx, retry, viewer.ID, 10)
	_ = manualTarget.Close(ctx)
	if err != nil || already || len(manualResults) == 0 || manualResults[0].Error != "" {
		t.Fatalf("simulate target commit before platform audit: results=%+v already=%v err=%v", manualResults, already, err)
	}
	if _, err := platform.ExecContext(ctx, `UPDATE migration_attempts SET started_at = now() - interval '31 seconds' WHERE id = $1`, abandonedAttempt); err != nil {
		t.Fatal(err)
	}
	recovered := request(http.MethodPost, path+"/migrations/2/execute", identifier, url.Values{"csrf_token": {session.CSRFToken}})
	if recovered.Code != http.StatusSeeOther {
		t.Fatalf("reconcile committed target status=%d body=%q", recovered.Code, recovered.Body.String())
	}
	if err := target.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_migrations`).Scan(&ledgerCount); err != nil || ledgerCount != 2 {
		t.Fatalf("reconciliation duplicated target SQL: ledger count=%d err=%v", ledgerCount, err)
	}
	if err := adminService.GrantAccess(ctx, adminUser, group.ID, connection.ID, authorization.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	identifier, err = auth.Login(ctx, "viewer", "viewer-long-password")
	if err != nil {
		t.Fatal(err)
	}
	session, err = auth.Validate(ctx, identifier)
	if err != nil {
		t.Fatal(err)
	}
	failedForm.Set("csrf_token", session.CSRFToken)
	if got := request(http.MethodPost, path+"/migrations", identifier, failedForm).Code; got != http.StatusForbidden {
		t.Fatalf("VIEW_ONLY created migration: status=%d", got)
	}
	planForm := url.Values{
		"plan_sql":   {"SELECT * FROM " + qualified + " WHERE id = 1"},
		"mode":       {explorer.PlanModeAnalyze},
		"csrf_token": {session.CSRFToken},
	}
	planResponse := request(http.MethodPost, path+"/explain", identifier, planForm)
	if planResponse.Code != http.StatusOK || !strings.Contains(planResponse.Body.String(), "Actual Execution Plan") || !strings.Contains(planResponse.Body.String(), "actual rows") {
		t.Fatalf("VIEW_ONLY actual plan status=%d body=%q", planResponse.Code, planResponse.Body.String())
	}
	var beforeRejectedPlan int
	if err := target.QueryRow(ctx, `SELECT count(*) FROM `+qualified).Scan(&beforeRejectedPlan); err != nil {
		t.Fatal(err)
	}
	planForm.Set("plan_sql", "DELETE FROM "+qualified)
	if got := request(http.MethodPost, path+"/explain", identifier, planForm).Code; got != http.StatusBadRequest {
		t.Fatalf("mutation plan status=%d, want 400", got)
	}
	var afterRejectedPlan int
	if err := target.QueryRow(ctx, `SELECT count(*) FROM `+qualified).Scan(&afterRejectedPlan); err != nil || afterRejectedPlan != beforeRejectedPlan {
		t.Fatalf("rejected plan changed target: before=%d after=%d err=%v", beforeRejectedPlan, afterRejectedPlan, err)
	}
}

func TestMigrationAttemptLifecyclePostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	actor := insertUser(t, db, "admin", "admin-long-password", true)
	store, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	service := admin.NewService(store, nil)
	connection, err := service.CreateConnection(ctx, actor, admin.CreateConnectionInput{
		Connection: domain.Connection{Name: "target", Host: "localhost", Port: 5432, DBname: "target", DBuser: "user", SSLMode: "disable"},
		Password:   "target-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	migration, err := store.CreateMigration(ctx, explorer.MigrationInput{DatabaseID: connection.ID, UserID: actor.ID, SQL: "CREATE TABLE sample (id integer)"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.BeginMigrationAttempt(ctx, migration, actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginMigrationAttempt(ctx, migration, actor.ID); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("concurrent attempt accepted: %v", err)
	}
	if _, err := store.UpdateMigration(ctx, explorer.MigrationInput{DatabaseID: connection.ID, Version: migration.Version, SQL: "DROP TABLE sample"}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("running draft was edited: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_attempts SET started_at = now() - interval '31 seconds' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginMigrationAttempt(ctx, migration, actor.ID)
	if err != nil || second == first {
		t.Fatalf("stale attempt recovery: first=%d second=%d err=%v", first, second, err)
	}
	if err := store.FinishMigrationAttempt(ctx, second, explorer.MigrationExecution{
		DatabaseID: connection.ID, Migration: migration, UserID: actor.ID, Status: "FAILED", Error: "query failed",
	}); err != nil {
		t.Fatal(err)
	}
	var unknown, failed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status = 'UNKNOWN'), count(*) FILTER (WHERE status = 'FAILED') FROM migration_attempts WHERE migration_id = $1`, migration.ID).
		Scan(&unknown, &failed); err != nil || unknown != 1 || failed != 1 {
		t.Fatalf("recovered attempts unknown=%d failed=%d err=%v", unknown, failed, err)
	}
	if _, err := store.UpdateMigration(ctx, explorer.MigrationInput{DatabaseID: connection.ID, Version: migration.Version, SQL: "CREATE TABLE sample (name text)"}); err != nil {
		t.Fatalf("failed draft could not be edited: %v", err)
	}
}
