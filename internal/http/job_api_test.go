package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const apiCSRF = "csrf-token-value"

type sessionService struct {
	session authentication.Session
	err     error
}

func (s *sessionService) Login(context.Context, string, string) (string, error) { return "", nil }
func (s *sessionService) Validate(context.Context, string) (authentication.Session, error) {
	return s.session, s.err
}
func (s *sessionService) Logout(context.Context, string) error { return nil }
func (s *sessionService) RenewAfterPrivilegeChange(context.Context, authentication.Session) (string, error) {
	return "", nil
}

var apiUser = domain.User{ID: 7, Username: "dev", IsActive: true}

func apiAuth(t *testing.T, err error) *AuthenticationHandler {
	t.Helper()
	h, authErr := NewAuthenticationHandler(&sessionService{session: authentication.Session{ID: "s", User: apiUser, CSRFToken: apiCSRF}, err: err}, template.New("x"), false)
	if authErr != nil {
		t.Fatal(authErr)
	}
	return h
}

func serveAPI(router http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "s"})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func jobRouter(auth *AuthenticationHandler, api *JobAPI) http.Handler {
	return NewApplicationRouter(auth, nil, nil, nil, func(context.Context) error { return nil }, api.Routes)
}

func TestGetJobReturnsTheJob(t *testing.T) {
	total := 10
	job := domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 4, Status: domain.JobStatusRunning, AttemptCount: 1,
		Progress: &domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: 3, Total: &total, LastCursor: "3000", AffectedRows: 3000}}
	var gotActor domain.User
	var gotID string
	jobs := &mockWorkflowJobService{getJob: func(_ context.Context, actor domain.User, id string) (domain.Job, error) {
		gotActor, gotID = actor, id
		return job, nil
	}}
	rec := serveAPI(jobRouter(apiAuth(t, nil), NewJobAPI(apiAuth(t, nil), jobs)), http.MethodGet, "/api/v1/jobs/job-1", "", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	progress, _ := body["progress"].(map[string]any)
	if body["id"] != "job-1" || body["status"] != "RUNNING" || progress["completed"] != float64(3) || progress["affected_rows"] != float64(3000) {
		t.Fatalf("body = %s", rec.Body)
	}
	if gotActor.ID != apiUser.ID || gotID != "job-1" {
		t.Fatalf("service got %+v / %q", gotActor, gotID)
	}
}

func TestGetJobErrors(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{admin.ErrNotFound, http.StatusNotFound},
		{admin.ErrForbidden, http.StatusForbidden},
		{fmt.Errorf("%w: bad id", admin.ErrInvalidInput), http.StatusBadRequest},
		{errors.New("database password leaked here"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		jobs := &mockWorkflowJobService{getJob: func(context.Context, domain.User, string) (domain.Job, error) { return domain.Job{}, c.err }}
		rec := serveAPI(jobRouter(apiAuth(t, nil), NewJobAPI(apiAuth(t, nil), jobs)), http.MethodGet, "/api/v1/jobs/x", "", nil)
		if rec.Code != c.code {
			t.Fatalf("%v: status %d, want %d", c.err, rec.Code, c.code)
		}
		if strings.Contains(rec.Body.String(), "password") {
			t.Fatalf("internal error details leaked: %s", rec.Body)
		}
	}
}

func TestGetJobNeedsALogin(t *testing.T) {
	auth := apiAuth(t, authentication.ErrUnauthenticated)
	jobs := &mockWorkflowJobService{}
	rec := serveAPI(jobRouter(auth, NewJobAPI(auth, jobs)), http.MethodGet, "/api/v1/jobs/job-1", "", nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "login required") {
		t.Fatalf("status %d, body %s; an API call must get 401, not a redirect", rec.Code, rec.Body)
	}
}
