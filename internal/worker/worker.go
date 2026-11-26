package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultRetryDelay        = 2 * time.Second
	DefaultMaxAttempts       = 5
)

type Config struct {
	Owner             string
	HeartbeatInterval time.Duration
	RetryDelay        time.Duration
	MaxAttempts       int
	FencedStore       func(owner string, token int64) JobStateStore
}

type Worker struct {
	consumer   JobConsumer
	jobs       JobStateStore
	locker     DatabaseLocker
	authorizer ExecutionAuthorizer
	executor   MigrationExecutor
	cfg        Config
}

func NewWorker(consumer JobConsumer, jobs JobStateStore, locker DatabaseLocker, authorizer ExecutionAuthorizer, executor MigrationExecutor, cfg Config) *Worker {
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "worker"
		}
		cfg.Owner = host
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = DefaultRetryDelay
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	return &Worker{
		consumer:   consumer,
		jobs:       jobs,
		locker:     locker,
		authorizer: authorizer,
		executor:   executor,
		cfg:        cfg,
	}
}

// Run consumes deliveries until ctx is canceled or the consumer returns an error.
func (w *Worker) Run(ctx context.Context) error {
	for {
		delivery, err := w.consumer.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		w.ProcessOne(ctx, delivery)
	}
}

// ProcessOne handles a single queue delivery end-to-end: trace restoration, locking, revalidation, execution, and settlement.
func (w *Worker) ProcessOne(ctx context.Context, delivery JobDelivery) {
	msg := delivery.Message()
	ctx = observability.ExtractTraceContext(ctx, msg.TraceContext)
	ctx = observability.WithDatabase(observability.WithActor(ctx, msg.ActorID, ""), msg.DatabaseID, "")

	ctx, span := observability.Tracer().Start(ctx, "worker.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("job.id", msg.JobID),
			attribute.String("execution.id", msg.ExecutionID),
			attribute.Int64("database.id", msg.DatabaseID),
			attribute.Int64("migration.id", msg.MigrationID),
			attribute.Int64("revision.id", msg.RevisionID),
			attribute.Bool("messaging.redelivered", delivery.Redelivered()),
		),
	)
	defer span.End()

	job, err := w.jobs.GetJob(ctx, msg.JobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, admin.ErrNotFound) {
			_ = delivery.Reject()
			return
		}
		_ = delivery.Requeue()
		return
	}
	if job.Status.Terminal() {
		span.SetStatus(codes.Ok, "already terminal")
		_ = delivery.Ack()
		return
	}

	token, acquired, err := w.locker.Acquire(ctx, msg.DatabaseID, msg.ExecutionID, w.cfg.Owner)
	if err != nil || !acquired {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			slog.InfoContext(ctx, "database lock busy; requeuing job",
				slog.String("event", "worker.lock_busy"),
				slog.String("job_id", msg.JobID),
				slog.String("execution_id", msg.ExecutionID),
				slog.String("status", "requeued"),
			)
		}
		w.sleepBackoff(ctx)
		_ = delivery.Requeue()
		return
	}
	defer func() {
		_ = w.locker.Release(context.WithoutCancel(ctx), msg.DatabaseID, w.cfg.Owner, token)
	}()

	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	stopHeartbeat := w.startHeartbeat(execCtx, cancelExec, msg.DatabaseID, token)
	defer stopHeartbeat()

	jobStore := w.jobs
	if w.cfg.FencedStore != nil {
		jobStore = w.cfg.FencedStore(w.cfg.Owner, token)
	}

	req := requestFromMessage(msg)
	if err := w.authorizer.AuthorizeExecution(execCtx, req); err != nil {
		w.handleAuthFailure(execCtx, span, jobStore, delivery, job, err)
		return
	}

	job.AttemptCount++
	job.Status = domain.JobStatusRunning
	if err := jobStore.SaveJob(execCtx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		_ = delivery.Requeue()
		return
	}

	res, execErr := w.executor.Execute(execCtx, req)
	w.settleExecution(ctx, span, jobStore, delivery, job, res, execErr)
}

func (w *Worker) startHeartbeat(ctx context.Context, cancelExec context.CancelFunc, databaseID, token int64) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(w.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				held, err := w.locker.Heartbeat(ctx, databaseID, w.cfg.Owner, token)
				if err == nil && !held {
					slog.WarnContext(ctx, "database lock lease lost during execution",
						slog.String("event", "worker.lock_lost"),
						slog.Int64("database_id", databaseID),
						slog.String("status", "error"),
					)
					cancelExec()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

func (w *Worker) handleAuthFailure(ctx context.Context, span trace.Span, store JobStateStore, delivery JobDelivery, job domain.Job, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	if errors.Is(err, admin.ErrForbidden) || errors.Is(err, admin.ErrInvalidInput) || errors.Is(err, admin.ErrNotFound) || errors.Is(err, admin.ErrConflict) {
		job.Status = domain.JobStatusFailed
		job.ErrorCode = "REVALIDATION_FAILED"
		job.ErrorMessage = err.Error()
		_ = store.SaveJob(context.WithoutCancel(ctx), job)
		_ = delivery.Reject()
		return
	}
	_ = delivery.Requeue()
}

func (w *Worker) settleExecution(ctx context.Context, span trace.Span, store JobStateStore, delivery JobDelivery, job domain.Job, res domain.ExecutionResult, execErr error) {
	saveCtx := context.WithoutCancel(ctx)
	if execErr != nil {
		span.RecordError(execErr)
		span.SetStatus(codes.Error, execErr.Error())
		switch migrationengine.ClassifyError(execErr) {
		case migrationengine.ErrorUncertain:
			job.Status = domain.JobStatusNeedsReconciliation
			job.ErrorCode = "COMMIT_UNCERTAIN"
			job.ErrorMessage = execErr.Error()
			_ = store.SaveJob(saveCtx, job)
			_ = delivery.Ack()
		case migrationengine.ErrorTransient:
			if job.AttemptCount < w.cfg.MaxAttempts {
				job.Status = domain.JobStatusRetryWait
				job.ErrorCode = "TRANSIENT_ERROR"
				job.ErrorMessage = execErr.Error()
				_ = store.SaveJob(saveCtx, job)
				w.sleepBackoff(ctx)
				_ = delivery.Requeue()
				return
			}
			job.Status = domain.JobStatusFailed
			job.ErrorCode = "MAX_ATTEMPTS"
			job.ErrorMessage = execErr.Error()
			_ = store.SaveJob(saveCtx, job)
			_ = delivery.Reject()
		default:
			job.Status = domain.JobStatusFailed
			job.ErrorCode = "PERMANENT_ERROR"
			job.ErrorMessage = execErr.Error()
			_ = store.SaveJob(saveCtx, job)
			_ = delivery.Reject()
		}
		return
	}

	switch res.Status {
	case domain.ExecutionStatusSucceeded:
		job.Status = domain.JobStatusSucceeded
		job.ErrorCode = ""
		job.ErrorMessage = ""
		span.SetStatus(codes.Ok, "")
	case domain.ExecutionStatusUnknown:
		job.Status = domain.JobStatusNeedsReconciliation
		job.ErrorCode = res.ErrorCode
		job.ErrorMessage = res.ErrorMessage
		span.SetStatus(codes.Error, res.ErrorMessage)
	default:
		job.Status = domain.JobStatusFailed
		job.ErrorCode = res.ErrorCode
		job.ErrorMessage = res.ErrorMessage
		span.SetStatus(codes.Error, res.ErrorMessage)
	}
	if err := store.SaveJob(saveCtx, job); err != nil {
		span.RecordError(err)
		if errors.Is(err, ErrLockLost) {
			_ = delivery.Requeue()
			return
		}
	}
	_ = delivery.Ack()
}

func (w *Worker) sleepBackoff(ctx context.Context) {
	timer := time.NewTimer(w.cfg.RetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func requestFromMessage(msg domain.MigrationJobMessage) domain.ExecutionRequest {
	return domain.ExecutionRequest{
		JobID:             msg.JobID,
		ExecutionID:       msg.ExecutionID,
		DatabaseID:        msg.DatabaseID,
		MigrationID:       msg.MigrationID,
		RevisionID:        msg.RevisionID,
		ApprovalRequestID: msg.ApprovalRequestID,
		ActorID:           msg.ActorID,
		Action:            msg.Action,
		Mode:              msg.Mode,
		TargetExecutionID: msg.TargetExecutionID,
		RequestedAt:       msg.RequestedAt,
	}
}
