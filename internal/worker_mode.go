package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/metrics"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	defaultJobQueue   = "migration.jobs"
	brokerDialTimeout = 10 * time.Second
)

var _ worker.MigrationExecutor = (*migrationengine.Engine)(nil)

type workerConfig struct {
	amqpURL      string
	queue        string
	slots        int
	batchTimeout time.Duration
	lockLease    time.Duration
}

func workerConfigFromEnv() (workerConfig, error) {
	c := workerConfig{
		amqpURL: os.Getenv("AMQP_URL"), queue: envOr("AMQP_QUEUE", defaultJobQueue),
		slots: 4, batchTimeout: database.DefaultMigrationBatchTimeout, lockLease: 30 * time.Second,
	}
	if c.amqpURL == "" {
		return c, errors.New("AMQP_URL is required in worker mode")
	}
	if v := os.Getenv("WORKER_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("WORKER_SLOTS must be a positive integer, got %q", v)
		}
		c.slots = n
	}
	for name, target := range map[string]*time.Duration{"MIGRATION_BATCH_TIMEOUT": &c.batchTimeout, "LOCK_LEASE": &c.lockLease} {
		if v := os.Getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return c, fmt.Errorf("%s must be a positive duration such as 5m, got %q", name, v)
			}
			*target = d
		}
	}
	if c.lockLease <= worker.DefaultHeartbeatInterval {
		return c, fmt.Errorf("LOCK_LEASE must be longer than the %s heartbeat", worker.DefaultHeartbeatInterval)
	}
	return c, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func workerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "worker"
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(suffix))
}

func runWorker(db *sql.DB, adminStore *database.AdminPostgresStore, adminService *admin.AdminService, recorder *metrics.MigrationRecorder) error {
	cfg, err := workerConfigFromEnv()
	if err != nil {
		return err
	}
	log := slog.Default()
	conn, err := amqp.DialConfig(cfg.amqpURL, amqp.Config{Dial: amqp.DefaultDial(brokerDialTimeout)})
	if err != nil {
		return fmt.Errorf("connect to the message broker: %w", err)
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	publisher, err := queue.NewRabbitPublisher(conn, cfg.queue)
	if err != nil {
		return err
	}
	defer publisher.Close()
	consumer, err := queue.NewRabbitConsumer(ctx, conn, cfg.queue, cfg.slots)
	if err != nil {
		return err
	}

	jobs := database.NewJobPostgresStore(db)
	var w *worker.Worker
	progress := func(ctx context.Context, executionID string, p domain.BatchProgress) error {
		return w.ReportProgress(ctx, executionID, p)
	}
	target := database.NewTargetExecutor(adminStore, database.WorkerTargetOptions(cfg.batchTimeout), progress)
	engine := migrationengine.NewEngine(database.NewRevisionPostgresStore(db), database.NewHistoryPostgresStore(db), target)
	w = worker.New(worker.Dependencies{
		Consumer:   consumer,
		Jobs:       jobs,
		Authorizer: workflow.NewExecutionAuthorizer(adminService, database.NewSubmitPostgresStore(db)),
		Executor:   engine,
		Locks:      database.NewLockPostgresStore(db, cfg.lockLease),
		Fence:      func(owner string, token int64) worker.JobStore { return jobs.Fenced(owner, token) },
	}, workerID(), log).WithRecorder(recorder)

	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		workflow.NewOutboxRelay(jobs, publisher, 0, 0).Run(ctx)
	}()
	go func() {
		defer background.Done()
		worker.RunSweeper(ctx, jobs, 0, log)
	}()
	err = w.Run(ctx, cfg.slots)
	stop()
	background.Wait()
	return err
}
