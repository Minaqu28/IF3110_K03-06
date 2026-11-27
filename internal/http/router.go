package httpserver

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func NewApplicationRouter(authenticationHandler *AuthenticationHandler, adminHandler *AdminHandler, databaseHandler *DatabaseHandler, static fs.FS, health func(context.Context) error) http.Handler {
	return NewApplicationRouterWithSPA(authenticationHandler, adminHandler, databaseHandler, nil, nil, nil, nil, static, nil, health)
}

// NewApplicationRouterWithSPA wires the JSON API, workflow endpoints, and embedded SPA assets.
func NewApplicationRouterWithSPA(
	authenticationHandler *AuthenticationHandler,
	adminHandler *AdminHandler,
	databaseHandler *DatabaseHandler,
	workflowHandler *WorkflowHandler,
	migrations MigrationQueryService,
	workflow WorkflowJobService,
	actions JobActionAdvisor,
	static, spa fs.FS,
	health func(context.Context) error,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(health))

	// SPA JSON API (/api/v1/...)
	mux.HandleFunc("GET /api/v1/auth/login-csrf", authenticationHandler.apiLoginCSRF)
	mux.HandleFunc("POST /api/v1/auth/login", authenticationHandler.apiLogin)
	mux.HandleFunc("GET /api/v1/auth/session", authenticationHandler.apiCurrentSession)
	mux.HandleFunc("POST /api/v1/auth/logout", authenticationHandler.apiLogout)
	if adminHandler != nil {
		mux.HandleFunc("GET /api/v1/admin", adminHandler.apiAdmin)
		mux.HandleFunc("POST /api/v1/admin/actions", adminHandler.apiAdminAction)
		mux.HandleFunc("GET /api/v1/databases", adminHandler.apiDatabases)
	}
	if databaseHandler != nil {
		mux.HandleFunc("GET /api/v1/databases/{id}", databaseHandler.apiDatabase)
		mux.HandleFunc("POST /api/v1/databases/{id}/query", databaseHandler.apiQuery)
		mux.HandleFunc("POST /api/v1/databases/{id}/explain", databaseHandler.apiExplain)
	}
	if spa != nil || migrations != nil || workflow != nil {
		services := &apiServices{auth: authenticationHandler, migrations: migrations, workflow: workflow, actions: actions}
		mux.HandleFunc("GET /api/v1/databases/{id}/revisions", services.revisions)
		mux.HandleFunc("POST /api/v1/databases/{id}/revisions", services.createRevision)
		mux.HandleFunc("POST /api/v1/databases/{id}/revisions/{revisionId}/review", services.submitReview)
		mux.HandleFunc("GET /api/v1/databases/{id}/history", services.history)
		mux.HandleFunc("GET /api/v1/databases/{id}/reviews", services.reviews)
		mux.HandleFunc("GET /api/v1/databases/{id}/reviews/{reviewId}/decisions", services.reviewDecisions)
		mux.HandleFunc("POST /api/v1/reviews/{id}/decision", services.decide)
		mux.HandleFunc("POST /api/v1/jobs", services.submit)
		mux.HandleFunc("POST /api/v1/databases/{id}/executions", services.submitForDatabase)
		mux.HandleFunc("GET /api/v1/jobs/{id}", services.job)
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
	})

	// Workflow JSON API (/api/...)
	if workflowHandler != nil {
		mux.HandleFunc("GET /api/databases/{id}/revisions", workflowHandler.ListRevisions)
		mux.HandleFunc("GET /api/databases/{id}/history", workflowHandler.ListHistory)
		mux.HandleFunc("GET /api/databases/{id}/reviews", workflowHandler.ListReviews)
		mux.HandleFunc("POST /api/reviews/{reviewId}/decide", workflowHandler.DecideApproval)
		mux.HandleFunc("POST /api/databases/{id}/executions", workflowHandler.SubmitExecution)
		mux.HandleFunc("GET /api/jobs/{jobId}", workflowHandler.GetJob)
	}

	// Embedded SPA or legacy SSR pages
	var spaPage http.Handler
	if spa != nil {
		spaPage = spaHandler(spa)
		mux.Handle("/app/", http.StripPrefix("/app/", http.FileServer(http.FS(spa))))
		mux.Handle("/", spaPage)
	} else {
		spaPage = http.HandlerFunc(authenticationHandler.Home)
	}
	mux.Handle("GET /{$}", spaPage)
	if spa == nil {
		mux.HandleFunc("GET /login", authenticationHandler.ShowLogin)
	} else {
		mux.Handle("GET /login", spaPage)
	}
	mux.HandleFunc("POST /login", authenticationHandler.Login)
	mux.HandleFunc("POST /logout", authenticationHandler.Logout)
	if spa == nil {
		mux.HandleFunc("GET /dashboard", authenticationHandler.ShowDashboard)
	} else {
		mux.Handle("GET /dashboard", spaPage)
	}
	if adminHandler != nil {
		if spa == nil {
			mux.HandleFunc("GET /admin", adminHandler.Show)
		} else {
			mux.Handle("GET /admin", spaPage)
		}
		mux.HandleFunc("POST /admin/users", adminHandler.CreateUser)
		mux.HandleFunc("POST /admin/users/status", adminHandler.SetUserActive)
		mux.HandleFunc("POST /admin/users/update", adminHandler.UpdateUser)
		mux.HandleFunc("POST /admin/users/password", adminHandler.ResetPassword)
		mux.HandleFunc("POST /admin/users/delete", adminHandler.DeleteUser)
		mux.HandleFunc("POST /admin/groups", adminHandler.CreateGroup)
		mux.HandleFunc("POST /admin/groups/rename", adminHandler.RenameGroup)
		mux.HandleFunc("POST /admin/groups/leader", adminHandler.SetGroupLeader)
		mux.HandleFunc("POST /admin/groups/delete", adminHandler.DeleteGroup)
		mux.HandleFunc("POST /admin/memberships", adminHandler.Membership)
		mux.HandleFunc("POST /admin/connections", adminHandler.CreateConnection)
		mux.HandleFunc("POST /admin/connections/update", adminHandler.UpdateConnection)
		mux.HandleFunc("POST /admin/connections/delete", adminHandler.DeleteConnection)
		mux.HandleFunc("POST /admin/grants", adminHandler.Grant)
		if spa == nil {
			mux.HandleFunc("GET /databases", adminHandler.ListDatabases)
		} else {
			mux.Handle("GET /databases", spaPage)
		}
	}
	if databaseHandler != nil {
		if spa == nil {
			mux.HandleFunc("GET /databases/{id}", databaseHandler.Show)
		} else {
			mux.Handle("GET /databases/{id}", spaPage)
		}
		mux.HandleFunc("POST /databases/{id}/query", databaseHandler.Execute)
		mux.HandleFunc("POST /databases/{id}/explain", databaseHandler.Explain)
	}
	if static != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
	return observabilityMiddleware(securityHeaders(mux))
}

func spaHandler(files fs.FS) http.Handler {
	assets := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if info, err := fs.Stat(files, name); err == nil && !info.IsDir() {
				assets.ServeHTTP(w, r)
				return
			}
		}
		if strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "SPA unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

func healthHandler(health func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := health(ctx); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

const correlationIDHeader = "X-Correlation-ID"

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func observabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		cid := strings.TrimSpace(r.Header.Get(correlationIDHeader))
		if cid == "" {
			cid = observability.NewCorrelationID()
		}
		ctx = observability.WithCorrelationID(ctx, cid)
		w.Header().Set(correlationIDHeader, cid)

		ctx, span := observability.Tracer().Start(ctx, "http.request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
				attribute.String("correlation_id", cid),
			),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		durationMs := float64(time.Since(start).Microseconds()) / 1000.0
		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		level := slog.LevelInfo
		statusText := "ok"
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
			statusText = "error"
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		} else if rec.status >= http.StatusBadRequest {
			level = slog.LevelWarn
			statusText = "client_error"
		} else {
			span.SetStatus(codes.Ok, "")
		}

		slog.LogAttrs(ctx, level, "http request completed",
			slog.String("event", "http.request"),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("http_status", rec.status),
			slog.String("status", statusText),
			slog.Float64("duration_ms", durationMs),
		)
	})
}
