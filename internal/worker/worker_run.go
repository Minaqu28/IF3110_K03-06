package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const DefaultLockWait = 5 * time.Second

const (
	ErrorCodeDatabaseLocked = "DATABASE_LOCKED"
	ErrorCodeNotAuthorized  = "NOT_AUTHORIZED"
	ErrorCodeNotAllowed     = "NOT_ALLOWED"
	ErrorCodeInvalidRequest = "INVALID_REQUEST"
)

func (w *Worker) handle(ctx context.Context, d JobDelivery) {
	msg := d.Message()
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
			attribute.String("migration.action", string(msg.Action)),
			attribute.String("migration.mode", string(msg.Mode)),
			attribute.Bool("messaging.redelivered", d.Redelivered()),
		),
	)
	defer span.End()

	log := deliveryLog(w.log, msg)
	job, ok := w.admit(ctx, log, d)
	if !ok {
		return
	}
	if job.AttemptCount == 0 && !job.CreatedAt.IsZero() {
		w.rec.QueueWait(w.now().Sub(job.CreatedAt))
	}
	lockCtx, lockSpan := observability.Tracer().Start(ctx, "worker.lock",
		trace.WithAttributes(
			attribute.Int64("database.id", msg.DatabaseID),
			attribute.String("execution.id", msg.ExecutionID),
			attribute.String("worker.id", w.id),
		),
	)
	lockStart := w.now()
	held, acquired, err := w.acquire(lockCtx, log, msg.DatabaseID, msg.ExecutionID)
	w.rec.LockWait(w.now().Sub(lockStart), err == nil && acquired)
	if err != nil {
		lockSpan.RecordError(err)
		lockSpan.SetStatus(codes.Error, err.Error())
		lockSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.ErrorContext(ctx, "could not take the database lock; requeueing", "event", "worker.lock", "status", "error", "error", err)
		w.settle(log, "requeue", d.Requeue)
		return
	}
	lockSpan.SetAttributes(attribute.Bool("lock.acquired", acquired))
	if !acquired {
		lockSpan.End()
		w.busy(ctx, log, d, job)
		return
	}
	lockSpan.SetAttributes(attribute.Int64("lock.fencing_token", held.token))
	lockSpan.SetStatus(codes.Ok, "")
	lockSpan.End()

	defer held.release(ctx)
	jobs := w.deps.Fence(w.id, held.token)
	runCtx := held.keepAlive(ctx, w.heartbeat)
	req := requestFrom(msg)
	if err := w.deps.Authorizer.AuthorizeExecution(runCtx, req); err != nil {
		code, refused := refusalCode(err)
		if !refused {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			log.ErrorContext(runCtx, "could not authorize the job; requeueing", "event", "worker.authorize", "status", "error", "error", err)
			w.settle(log, "requeue", d.Requeue)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, code)
		log.WarnContext(runCtx, "job refused at run time", "event", "worker.authorize", "status", "refused", "error_code", code, "error", err)
		job.Status, job.ErrorCode, job.ErrorMessage = domain.JobStatusFailed, code, err.Error()
		w.saveThenAck(ctx, log, d, jobs, job)
		return
	}
	job.Status, job.AttemptCount = domain.JobStatusRunning, job.AttemptCount+1
	job.ErrorCode, job.ErrorMessage = "", ""
	if err := jobs.SaveJob(runCtx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.unsaved(log, d, err)
		return
	}
	log.InfoContext(runCtx, "job started", "event", "worker.start", "status", "running", "attempt", job.AttemptCount, "action", msg.Action, "mode", msg.Mode, "redelivered", d.Redelivered())
	run := w.active.track(msg.ExecutionID, job, jobs)
	execStart := w.now()
	result, err := w.deps.Executor.Execute(runCtx, req)
	execDuration := w.now().Sub(execStart)
	w.active.untrack(msg.ExecutionID, run)
	job = run.latest()
	if cause := context.Cause(runCtx); errors.Is(cause, ErrLockLost) {
		span.RecordError(cause)
		log.ErrorContext(ctx, "the run lost its database lock", "event", "worker.lock_lost", "status", "error", "error", err)
	}
	finalJob := outcome(job, msg, result, err)
	w.rec.JobFinished(msg.Action, msg.Mode, finalJob.Status, execDuration)
	if finalJob.Status == domain.JobStatusSucceeded {
		span.SetStatus(codes.Ok, "")
	} else {
		if err != nil {
			span.RecordError(err)
		}
		span.SetStatus(codes.Error, string(finalJob.Status))
	}
	w.saveThenAck(ctx, log, d, jobs, finalJob)
}

func (w *Worker) busy(ctx context.Context, log *slog.Logger, d JobDelivery, job domain.Job) {
	if job.Status == domain.JobStatusRunning {
		log.Info("job is running under another worker's lock; dropping the duplicate delivery")
		w.settle(log, "ack", d.Ack)
		return
	}
	job.Status, job.ErrorCode, job.ErrorMessage = domain.JobStatusRetryWait, ErrorCodeDatabaseLocked, "database sedang dikunci"
	if err := w.deps.Jobs.SaveJobAfter(context.WithoutCancel(ctx), job, w.now().Add(w.lockWait)); err != nil {
		w.unsaved(log, d, err)
		return
	}
	w.rec.Retry("locked")
	log.Info("database is locked by another execution; retrying later", "retry_in", w.lockWait)
	w.settle(log, "ack", d.Ack)
}

func requestFrom(msg domain.MigrationJobMessage) domain.ExecutionRequest {
	return domain.ExecutionRequest{
		JobID: msg.JobID, ExecutionID: msg.ExecutionID, DatabaseID: msg.DatabaseID, MigrationID: msg.MigrationID,
		RevisionID: msg.RevisionID, ApprovalRequestID: msg.ApprovalRequestID, ActorID: msg.ActorID,
		Action: msg.Action, Mode: msg.Mode, TargetExecutionID: msg.TargetExecutionID, RequestedAt: msg.RequestedAt,
	}
}

func refusalCode(err error) (string, bool) {
	switch {
	case errors.Is(err, admin.ErrForbidden):
		return ErrorCodeNotAuthorized, true
	case errors.Is(err, admin.ErrConflict):
		return ErrorCodeNotAllowed, true
	case errors.Is(err, admin.ErrInvalidInput), errors.Is(err, admin.ErrNotFound):
		return ErrorCodeInvalidRequest, true
	}
	return "", false
}

func (w *Worker) saveThenAck(ctx context.Context, log *slog.Logger, d JobDelivery, jobs JobStore, job domain.Job) {
	job, delay := w.retry.schedule(job)
	ctx = context.WithoutCancel(ctx)
	var err error
	if delay > 0 {
		err = jobs.SaveJobAfter(ctx, job, w.now().Add(delay))
	} else {
		err = jobs.SaveJob(ctx, job)
	}
	if err != nil {
		w.unsaved(log, d, err)
		return
	}
	if delay > 0 {
		w.rec.Retry("transient")
	}
	log.Info("job saved", "status", job.Status, "attempt", job.AttemptCount, "error_code", job.ErrorCode, "retry_in", delay)
	w.settle(log, "ack", d.Ack)
}

func (w *Worker) unsaved(log *slog.Logger, d JobDelivery, err error) {
	if errors.Is(err, admin.ErrConflict) || errors.Is(err, ErrLockLost) {
		log.Info("job is no longer ours to save; acknowledging", "error", err)
		w.settle(log, "ack", d.Ack)
		return
	}
	log.Error("could not save the job; requeueing without ack", "error", err)
	w.settle(log, "requeue", d.Requeue)
}
