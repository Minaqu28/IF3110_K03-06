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
	"github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/rabbitmq/amqp091-go"
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

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()

	publisher, consumer, closeQueue, err := initQueue(workerCtx)
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
	lockStore := database.NewLockPostgresStore(db, 30*time.Second)

	targetExecutor := database.NewTargetExecutor(
		adminStore,
		database.WorkerTargetOptions(database.DefaultMigrationBatchTimeout),
		migrationengine.ProgressSink(func(ctx context.Context, executionID string, progress domain.BatchProgress) error {
			return jobStore.UpdateProgressByExecution(ctx, executionID, progress)
		}),
	)
	engine := migrationengine.NewEngine(revisionStore, historyStore, targetExecutor, executionStore)
	authorizer := workflow.NewExecutionAuthorizer(adminService, submitStore)
	reviewService := workflow.NewReviewService(adminService, approvalStore, approvalStore, approvalStore)
	submitService := workflow.NewSubmitService(adminService, revisionStore, adminStore, submitStore, publisher, jobStore)
	queryService := workflow.NewQueryService(adminService, revisionStore, historyStore)
	jobService := workflow.NewJobService(adminService, reviewService, approvalStore, submitService, jobStore)
	workflowHandler := httpserver.NewWorkflowHandler(authHandler, queryService, jobService)

	outboxRelay := workflow.NewOutboxRelay(jobStore, publisher, workflow.DefaultRelayInterval, workflow.DefaultRelayBatch)
	workerRunner := worker.NewWorker(consumer, jobStore, lockStore, authorizer, engine, worker.Config{
		FencedStore: func(owner string, token int64) worker.JobStateStore {
			return jobStore.Fenced(owner, token)
		},
	})

	go outboxRelay.Run(workerCtx)
	go runJobSweeper(workerCtx, jobStore, 30*time.Second)
	go func() {
		if err := workerRunner.Run(workerCtx); err != nil && workerCtx.Err() == nil {
			slog.Error("worker exited with error", slog.String("event", "worker.fatal"), slog.String("status", "error"), slog.String("error", err.Error()))
		}
	}()

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpserver.NewApplicationRouterWithWorkflow(authHandler, adminHandler, databaseHandler, workflowHandler, static, db.PingContext, jobRoutes),
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

func initQueue(ctx context.Context) (workflow.JobPublisher, worker.JobConsumer, func(), error) {
	amqpURL := strings.TrimSpace(os.Getenv("AMQP_URL"))
	if amqpURL == "" {
		mem := queue.NewMemory()
		return mem, mem, func() {}, nil
	}
	queueName := strings.TrimSpace(os.Getenv("AMQP_QUEUE"))
	if queueName == "" {
		queueName = "migration_jobs"
	}
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return nil, nil, nil, err
	}
	pub, err := queue.NewRabbitPublisher(conn, queueName)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	con, err := queue.NewRabbitConsumer(ctx, conn, queueName, 1)
	if err != nil {
		_ = pub.Close()
		_ = conn.Close()
		return nil, nil, nil, err
	}
	cleanup := func() {
		_ = con.Close()
		_ = pub.Close()
		_ = conn.Close()
	}
	return pub, con, cleanup, nil
}

func runJobSweeper(ctx context.Context, jobs *database.JobPostgresStore, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if swept, err := jobs.SweepExpiredJobs(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sweep expired jobs failed", slog.String("event", "worker.sweep"), slog.String("status", "error"), slog.String("error", err.Error()))
		} else if swept > 0 {
			slog.Warn("swept expired worker jobs", slog.String("event", "worker.sweep"), slog.String("status", "ok"), slog.Int("swept", swept))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
