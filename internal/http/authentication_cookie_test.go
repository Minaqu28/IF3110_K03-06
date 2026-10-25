package httpserver

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type scriptedAuth struct {
	loginID     string
	loginErr    error
	session     authentication.Session
	validateErr error
	loggedOut   []string
}

func (s *scriptedAuth) Login(context.Context, string, string) (string, error) {
	return s.loginID, s.loginErr
}
func (s *scriptedAuth) Validate(_ context.Context, id string) (authentication.Session, error) {
	if id != "valid" {
		return authentication.Session{}, authentication.ErrUnauthenticated
	}
	return s.session, s.validateErr
}
func (s *scriptedAuth) Logout(_ context.Context, id string) error {
	s.loggedOut = append(s.loggedOut, id)
	return nil
}

var cookieTestPages = template.Must(template.New("login.html").Parse(
	`{{define "login.html"}}<input name="csrf_token" value="{{.CSRF}}">{{.Message}}{{end}}` +
		`{{define "dashboard.html"}}dashboard{{end}}`))

func newCookieRouter(t *testing.T, svc *scriptedAuth, secure bool) http.Handler {
	t.Helper()
	h, err := NewAuthenticationHandler(svc, cookieTestPages, secure)
	if err != nil {
		t.Fatal(err)
	}
	return NewApplicationRouter(h, nil, nil, nil, func(context.Context) error { return nil })
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// loginForm fetches a valid login CSRF pair and submits credentials.
func loginForm(t *testing.T, router http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/login", nil))
	csrfCookie := findCookie(get, loginCSRFCookie)
	if csrfCookie == nil {
		t.Fatal("login page did not set a CSRF cookie")
	}
	form := url.Values{"username": {username}, "password": {password}, "csrf_token": {csrfCookie.Value}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		router := newCookieRouter(t, &scriptedAuth{loginID: "opaque-identifier"}, secure)
		rec := loginForm(t, router, "chiikawa", "wibbly wobbly timey wimey")
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
			t.Fatalf("secure=%v: login = %d -> %q", secure, rec.Code, rec.Header().Get("Location"))
		}
		c := findCookie(rec, sessionCookie)
		if c == nil {
			t.Fatalf("secure=%v: no session cookie", secure)
		}
		if c.Value != "opaque-identifier" {
			t.Errorf("cookie value = %q", c.Value)
		}
		if !c.HttpOnly {
			t.Error("session cookie must be HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode && c.SameSite != http.SameSiteStrictMode {
			t.Errorf("SameSite = %v, want Lax or Strict", c.SameSite)
		}
		if c.Secure != secure {
			t.Errorf("Secure = %v, want %v", c.Secure, secure)
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.MaxAge != 0 {
			t.Errorf("session cookie should be a browser-session cookie, MaxAge=%d", c.MaxAge)
		}
		if findCookie(rec, loginCSRFCookie) == nil || findCookie(rec, loginCSRFCookie).MaxAge >= 0 {
			t.Error("login CSRF cookie should be cleared after login")
		}
	}
}

func TestFailedLoginSetsNoSessionAndLeaksNoSecrets(t *testing.T) {
	router := newCookieRouter(t, &scriptedAuth{loginErr: authentication.ErrInvalidCredentials}, false)
	rec := loginForm(t, router, "chiikawa", "horsing-around")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if findCookie(rec, sessionCookie) != nil {
		t.Error("failed login must not set a session cookie")
	}
	if body := rec.Body.String(); strings.Contains(body, "horsing-around") {
		t.Error("response echoed the submitted password")
	}
}

func TestLoginWithoutCSRFIsForbidden(t *testing.T) {
	svc := &scriptedAuth{loginID: "x"}
	router := newCookieRouter(t, svc, false)
	form := url.Values{"username": {"chiikawa"}, "password": {"wibbly wobbly timey wimey"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || findCookie(rec, sessionCookie) != nil {
		t.Fatalf("status = %d, cookie = %v", rec.Code, findCookie(rec, sessionCookie))
	}
}

func authedSession() authentication.Session {
	return authentication.Session{User: domain.User{ID: 1, Username: "chiikawa", IsActive: true}, CSRFToken: "csrf-ok"}
}

func TestRotatedIdentifierIsIssuedUnderSameCookieName(t *testing.T) {
	sess := authedSession()
	sess.RotatedIdentifier = "brand-new-identifier"
	router := newCookieRouter(t, &scriptedAuth{session: sess}, true)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	c := findCookie(rec, sessionCookie)
	if c == nil || c.Value != "brand-new-identifier" {
		t.Fatalf("rotated cookie = %+v", c)
	}
	if !c.HttpOnly || !c.Secure {
		t.Errorf("rotated cookie lost its flags: HttpOnly=%v Secure=%v", c.HttpOnly, c.Secure)
	}
}

func TestInvalidSessionClearsCookieAndRedirectsToLogin(t *testing.T) {
	router := newCookieRouter(t, &scriptedAuth{session: authedSession()}, false)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired-or-forged"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := findCookie(rec, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("stale cookie not cleared: %+v", c)
	}
}

func TestProtectedPagesRequireSession(t *testing.T) {
	router := newCookieRouter(t, &scriptedAuth{session: authedSession()}, false)
	for _, path := range []string{"/dashboard"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s without cookie: %d -> %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestStoreFailureDuringValidationIsServerErrorNotLogout(t *testing.T) {
	router := newCookieRouter(t, &scriptedAuth{session: authedSession(), validateErr: errors.New("db down")}, false)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if c := findCookie(rec, sessionCookie); c != nil && c.MaxAge < 0 {
		t.Error("a transient store error must not clear the user's cookie")
	}
}

func TestLogout(t *testing.T) {
	post := func(router http.Handler, csrf string) *httptest.ResponseRecorder {
		form := url.Values{"csrf_token": {csrf}}
		req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("wrong CSRF keeps session", func(t *testing.T) {
		svc := &scriptedAuth{session: authedSession()}
		rec := post(newCookieRouter(t, svc, false), "wrong")
		if rec.Code != http.StatusForbidden || len(svc.loggedOut) != 0 {
			t.Fatalf("status=%d loggedOut=%v", rec.Code, svc.loggedOut)
		}
	})

	t.Run("revokes server side and clears cookie", func(t *testing.T) {
		svc := &scriptedAuth{session: authedSession()}
		rec := post(newCookieRouter(t, svc, false), "csrf-ok")
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Fatalf("got %d -> %q", rec.Code, rec.Header().Get("Location"))
		}
		if len(svc.loggedOut) != 1 || svc.loggedOut[0] != "valid" {
			t.Errorf("backend revoke calls = %v", svc.loggedOut)
		}
		if c := findCookie(rec, sessionCookie); c == nil || c.MaxAge >= 0 {
			t.Errorf("cookie not cleared: %+v", c)
		}
	})

	t.Run("GET is not allowed", func(t *testing.T) {
		router := newCookieRouter(t, &scriptedAuth{session: authedSession()}, false)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logout", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET /logout = %d, want 405", rec.Code)
		}
	})
}
