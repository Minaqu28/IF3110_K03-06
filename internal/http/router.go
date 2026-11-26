package httpserver

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// NewRouter registers the routes available in the frontend foundation.
func NewRouter(pages *template.Template, static fs.FS) http.Handler {
	pageHandler := &PageHandler{pages: pages}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", pageHandler.Health)
	mux.HandleFunc("GET /{$}", pageHandler.Home)
	mux.HandleFunc("GET /login", pageHandler.Login)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return securityHeaders(mux)
}

// NewAuthenticatedRouter wires the authentication handler when a session store is ready.
func NewAuthenticatedRouter(authenticationHandler *AuthenticationHandler, static fs.FS, health func(context.Context) error) http.Handler {
	return NewApplicationRouter(authenticationHandler, nil, nil, static, health)
}

func NewApplicationRouter(authenticationHandler *AuthenticationHandler, adminHandler *AdminHandler, databaseHandler *DatabaseHandler, static fs.FS, health func(context.Context) error) http.Handler {
	return NewApplicationRouterWithSPA(authenticationHandler, adminHandler, databaseHandler, nil, nil, static, nil, health)
}

// NewApplicationRouterWithSPA wires the JSON API and serves embedded SPA assets.
func NewApplicationRouterWithSPA(authenticationHandler *AuthenticationHandler, adminHandler *AdminHandler, databaseHandler *DatabaseHandler, migrations MigrationQueryService, workflow WorkflowJobService, static, spa fs.FS, health func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(health))
	services := &apiServices{auth: authenticationHandler, migrations: migrations, workflow: workflow}
	spaPage := http.Handler(http.HandlerFunc(authenticationHandler.Home))
	if spa != nil {
		spaPage = spaHandler(spa)
	}
	mux.HandleFunc("GET /api/v1/auth/login-csrf", authenticationHandler.apiLoginCSRF)
	mux.HandleFunc("POST /api/v1/auth/login", authenticationHandler.apiLogin)
	mux.HandleFunc("GET /api/v1/auth/session", authenticationHandler.apiCurrentSession)
	mux.HandleFunc("POST /api/v1/auth/logout", authenticationHandler.apiLogout)
	mux.HandleFunc("GET /api/v1/databases/{id}/revisions", services.revisions)
	mux.HandleFunc("GET /api/v1/databases/{id}/history", services.history)
	mux.HandleFunc("GET /api/v1/databases/{id}/reviews", services.reviews)
	mux.HandleFunc("POST /api/v1/reviews/{id}/decision", services.decide)
	mux.HandleFunc("POST /api/v1/jobs", services.submit)
	mux.HandleFunc("GET /api/v1/jobs/{id}", services.job)
	if adminHandler != nil {
		mux.HandleFunc("GET /api/v1/admin", adminHandler.apiAdmin)
		mux.HandleFunc("POST /api/v1/admin/actions", adminHandler.apiAdminAction)
		mux.HandleFunc("GET /api/v1/databases", adminHandler.apiDatabases)
	}
	if databaseHandler != nil {
		mux.HandleFunc("GET /api/v1/databases/{id}", databaseHandler.apiDatabase)
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
	})
	if spa != nil {
		mux.Handle("/app/", http.StripPrefix("/app/", http.FileServer(http.FS(spa))))
		mux.Handle("/", spaHandler(spa))
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
		if spa == nil {
			mux.HandleFunc("POST /databases/{id}/query", databaseHandler.Execute)
			mux.HandleFunc("POST /databases/{id}/explain", databaseHandler.Explain)
			mux.HandleFunc("POST /databases/{id}/migrations/preview", databaseHandler.PreviewMigration)
			mux.HandleFunc("POST /databases/{id}/migrations", databaseHandler.CreateMigration)
			mux.HandleFunc("POST /databases/{id}/migrations/{version}/update", databaseHandler.UpdateMigration)
			mux.HandleFunc("POST /databases/{id}/migrations/{version}/execute", databaseHandler.ExecuteMigration)
		}
	}
	if static != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
	return securityHeaders(mux)
}

type apiServices struct {
	auth       *AuthenticationHandler
	migrations MigrationQueryService
	workflow   WorkflowJobService
}

func spaHandler(files fs.FS) http.Handler {
	assets := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", 405)
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
			http.Error(w, "SPA unavailable", 500)
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
