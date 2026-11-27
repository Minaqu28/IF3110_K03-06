package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
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
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/metrics"
	httpserver "github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func main() {
	slog.SetDefault(observability.NewLogger(os.Stdout, slog.LevelInfo))
	if err := run(); err != nil {
		slog.Error("server exited with error", slog.String("event", "server.fatal"), slog.String("status", "error"), slog.String("error", err.Error()))
		os.Exit(1)
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
	spa, err := fs.Sub(frontend.Files, ".")
	if err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serviceName := "dbmp-platform"
	if *mode == "worker" {
		serviceName = "dbmp-worker"
	}
	shutdownTracing, err := observability.InitTracing(ctx, serviceName, os.Getenv("OTEL_TRACES_EXPORTER"), os.Stdout)
	if err != nil {
		return err
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = shutdownTracing(shutCtx)
	}()
	logExport, shutdownLogs, err := observability.InitLogExport(ctx, serviceName, os.Getenv("OTEL_LOGS_EXPORTER"))
	if err != nil {
		return err
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = shutdownLogs(shutCtx)
	}()
	if logExport != nil {
		slog.SetDefault(observability.NewLogger(os.Stdout, slog.LevelInfo, logExport))
	}

	db, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		return err
	}
	reg := metrics.NewRegistry()
	if err := reg.RegisterDBPool(db, "platform"); err != nil {
		return err
	}
	recorder, err := metrics.NewMigrationRecorder(reg)
	if err != nil {
		return err
	}
	stopMetrics := startMetricsServer(reg)
	defer stopMetrics()
	if *mode == "worker" {
		adminStore, err := database.NewAdminPostgresStore(db, os.Getenv("DB_CREDENTIAL_KEY"))
		if err != nil {
			return err
		}
		cancel()
		return runWorker(db, adminStore, admin.NewService(adminStore, nil), recorder)
	}
	if err := reg.Register(metrics.NewJobStatusCollector(database.JobStatusCounts(db))); err != nil {
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

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()

	publisher, embeddedConsumer, closeQueue, err := initWebQueue()
	if err != nil {
		return err
	}
	defer closeQueue()

	approvalStore := database.NewApprovalPostgresStore(db)
	revisionStore := database.NewRevisionPostgresStore(db)
	executionStore := database.NewExecutionPostgresStore(db)
	historyStore := database.NewHistoryPostgresStore(db)
	jobStore := database.NewJobPostgresStore(db)
	submitStore := database.NewSubmitPostgresStore(db)

	reviewService := workflow.NewReviewService(adminService, approvalStore, approvalStore, approvalStore)
	submitService := workflow.NewSubmitService(adminService, revisionStore, adminStore, submitStore, publisher, jobStore)
	queryService := workflow.NewQueryService(adminService, revisionStore, historyStore)
	jobService := workflow.NewJobService(adminService, reviewService, approvalStore, submitService, jobStore).
		WithRevisions(revisionStore, adminStore).
		WithDecisions(approvalStore).
		WithExecutions(executionStore)
	actionAdvisor := workflow.NewActionAdvisor(adminService, executionStore, submitStore)
	workflowHandler := httpserver.NewWorkflowHandler(authHandler, queryService, jobService)

	go workflow.NewOutboxRelay(jobStore, publisher, workflow.DefaultRelayInterval, workflow.DefaultRelayBatch).Run(workerCtx)

	if embeddedConsumer != nil {
		var embeddedWorker *worker.Worker
		progress := func(ctx context.Context, executionID string, p domain.BatchProgress) error {
			return embeddedWorker.ReportProgress(ctx, executionID, p)
		}
		targetExecutor := database.NewTargetExecutor(adminStore, database.WorkerTargetOptions(database.DefaultMigrationBatchTimeout), progress)
		engine := migrationengine.NewEngine(revisionStore, historyStore, targetExecutor)
		embeddedWorker = worker.New(worker.Dependencies{
			Consumer:   embeddedConsumer,
			Jobs:       jobStore,
			Authorizer: workflow.NewExecutionAuthorizer(adminService, submitStore),
			Executor:   engine,
			Locks:      database.NewLockPostgresStore(db, 30*time.Second),
			Fence:      func(owner string, token int64) worker.JobStore { return jobStore.Fenced(owner, token) },
		}, workerID(), slog.Default()).WithRecorder(recorder)
		go worker.RunSweeper(workerCtx, jobStore, 0, slog.Default())
		go func() {
			if err := embeddedWorker.Run(workerCtx, 2); err != nil && workerCtx.Err() == nil {
				slog.Error("embedded worker exited with error", slog.String("event", "worker.fatal"), slog.String("status", "error"), slog.String("error", err.Error()))
			}
		}()
	}

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr: addr,
		Handler: httpserver.NewApplicationRouterWithSPA(
			authHandler,
			adminHandler,
			databaseHandler,
			workflowHandler,
			queryService,
			jobService,
			actionAdvisor,
			static,
			spa,
			db.PingContext,
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	go func() {
		<-stop.Done()
		cancelWorkers()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = server.Shutdown(shutCtx)
	}()

	slog.Info("server listening", slog.String("event", "server.start"), slog.String("addr", addr), slog.String("status", "ok"))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
