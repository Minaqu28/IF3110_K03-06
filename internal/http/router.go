package httpserver

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"
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
	return NewApplicationRouter(authenticationHandler, nil, static, health)
}

func NewApplicationRouter(authenticationHandler *AuthenticationHandler, adminHandler *AdminHandler, static fs.FS, health func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(health))
	mux.HandleFunc("GET /{$}", authenticationHandler.Home)
	mux.HandleFunc("GET /login", authenticationHandler.ShowLogin)
	mux.HandleFunc("POST /login", authenticationHandler.Login)
	mux.HandleFunc("POST /logout", authenticationHandler.Logout)
	mux.HandleFunc("GET /dashboard", authenticationHandler.ShowDashboard)
	if adminHandler != nil {
		mux.HandleFunc("GET /admin", adminHandler.Show)
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
		mux.HandleFunc("GET /databases", adminHandler.ListDatabases)
		mux.HandleFunc("GET /databases/{id}", adminHandler.Database)
	}
	if static != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
	return securityHeaders(mux)
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
