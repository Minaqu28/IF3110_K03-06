package main

import (
	"context"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
)

func main() {
	slog.SetDefault(observability.NewLogger(os.Stdout, slog.LevelInfo))
	if err := run(); err != nil {
		slog.Error("server exited with error", slog.String("event", "server.fatal"), slog.String("status", "error"), slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	pages, err := template.ParseFS(frontend.Files, "pages/*.html")
	if err != nil {
		return err
	}
	static, err := fs.Sub(frontend.Files, "assets")
	if err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shutdownTracing, err := observability.InitTracing(ctx, "dbmp-platform", os.Getenv("OTEL_TRACES_EXPORTER"), os.Stdout)
	if err != nil {
		return err
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = shutdownTracing(shutCtx)
	}()
	db, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		return err
	}
	if err := database.BootstrapAdmin(ctx, db, os.Getenv("BOOTSTRAP_ADMIN_USERNAME"), os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")); err != nil {
		return err
	}
	authService := authentication.NewService(database.NewPostgresStore(db), nil)
	adminStore, err := database.NewAdminPostgresStore(db, os.Getenv("DB_CREDENTIAL_KEY"))
	if err != nil {
		return err
	}
	adminService := admin.NewService(adminStore, nil)
	secureCookies := strings.EqualFold(os.Getenv("APP_ENV"), "production") || strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true")
	authHandler, err := httpserver.NewAuthenticationHandler(authService, pages, secureCookies)
	if err != nil {
		return err
	}
	adminHandler := httpserver.NewAdminHandler(adminService, authHandler, pages)
	databaseHandler := httpserver.NewDatabaseHandler(explorer.NewService(adminService, adminStore), authHandler, pages)

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpserver.NewApplicationRouter(authHandler, adminHandler, databaseHandler, static, db.PingContext),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	slog.Info("server listening", slog.String("event", "server.start"), slog.String("addr", addr), slog.String("status", "ok"))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
