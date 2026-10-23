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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(health))
	mux.HandleFunc("GET /{$}", authenticationHandler.Home)
	mux.HandleFunc("GET /login", authenticationHandler.ShowLogin)
	mux.HandleFunc("POST /login", authenticationHandler.Login)
	mux.HandleFunc("POST /logout", authenticationHandler.Logout)
	mux.HandleFunc("GET /dashboard", authenticationHandler.ShowDashboard)
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
