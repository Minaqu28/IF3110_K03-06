package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	mode := flag.String("mode", "web", "web serves the application; worker runs migration jobs from the queue")
	flag.Parse()
	if *mode != "web" && *mode != "worker" {
		return fmt.Errorf("unknown -mode %q: use web or worker", *mode)
	}
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
	db, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		return err
	}
	if *mode == "worker" {
		adminStore, err := database.NewAdminPostgresStore(db, os.Getenv("DB_CREDENTIAL_KEY"))
		if err != nil {
			return err
		}
		cancel()
		return runWorker(db, adminStore, admin.NewService(adminStore, nil))
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
	jobRoutes, closeJobs, err := webJobRoutes(db, authHandler, adminStore, adminService)
	if err != nil {
		return err
	}
	defer closeJobs()

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpserver.NewApplicationRouter(authHandler, adminHandler, databaseHandler, static, db.PingContext, jobRoutes),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
