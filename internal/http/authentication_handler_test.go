package httpserver

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
)

type testAuthenticationService struct {
	loginCalls int
}

func (s *testAuthenticationService) Login(_ context.Context, _, _ string) (string, error) {
	s.loginCalls++
	return "session-identifier", nil
}

func (s *testAuthenticationService) Validate(context.Context, string) (authentication.Session, error) {
	return authentication.Session{}, authentication.ErrUnauthenticated
}

func (s *testAuthenticationService) Logout(context.Context, string) error { return nil }
func (s *testAuthenticationService) RenewAfterPrivilegeChange(context.Context, authentication.Session) (string, error) {
	return "renewed-session", nil
}

func TestAuthenticatedRouterLoginCSRF(t *testing.T) {
	pages := template.Must(template.New("login.html").Parse(`<form><input name="csrf_token" value="{{.CSRF}}"></form>`))
	service := &testAuthenticationService{}
	handler, err := NewAuthenticationHandler(service, pages, false)
	if err != nil {
		t.Fatal(err)
	}
	router := NewAuthenticatedRouter(handler, nil, func(context.Context) error { return nil })

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/login", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d", get.Code)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range get.Result().Cookies() {
		if cookie.Name == loginCSRFCookie {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil || !csrfCookie.HttpOnly || csrfCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("login CSRF cookie missing security attributes")
	}

	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=a&password=b")))
	if invalid.Code != http.StatusForbidden || service.loginCalls != 0 {
		t.Fatal("login without CSRF token reached authentication service")
	}

	form := url.Values{"username": {"alice"}, "password": {"secret"}, "csrf_token": {csrfCookie.Value}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(csrfCookie)
	valid := httptest.NewRecorder()
	router.ServeHTTP(valid, request)
	if valid.Code != http.StatusSeeOther || valid.Header().Get("Location") != "/dashboard" || service.loginCalls != 1 {
		t.Fatalf("valid login: status=%d location=%q calls=%d", valid.Code, valid.Header().Get("Location"), service.loginCalls)
	}

	dashboard := httptest.NewRecorder()
	router.ServeHTTP(dashboard, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if dashboard.Code != http.StatusSeeOther || dashboard.Header().Get("Location") != "/login" {
		t.Fatal("dashboard did not redirect unauthenticated request")
	}
}
