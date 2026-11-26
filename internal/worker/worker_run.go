package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
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
	log := deliveryLog(w.log, msg)
	job, ok := w.admit(ctx, log, d)
	if !ok {
		return
	}
	held, acquired, err := w.acquire(ctx, log, msg.DatabaseID, msg.ExecutionID)
	if err != nil {
		log.Error("could not take the database lock; requeueing", "error", err)
		w.settle(log, "requeue", d.Requeue)
		return
	}
	if !acquired {
		w.busy(ctx, log, d, job)
		return
	}
	defer held.release(ctx)
	jobs := w.deps.Fence(w.id, held.token)
	runCtx := held.keepAlive(ctx, w.heartbeat)
	req := requestFrom(msg)
	if err := w.deps.Authorizer.AuthorizeExecution(runCtx, req); err != nil {
		code, refused := refusalCode(err)
		if !refused {
			log.Error("could not authorize the job; requeueing", "error", err)
			w.settle(log, "requeue", d.Requeue)
			return
		}
		log.Warn("job refused at run time", "error_code", code, "error", err)
		job.Status, job.ErrorCode, job.ErrorMessage = domain.JobStatusFailed, code, err.Error()
		w.saveThenAck(ctx, log, d, jobs, job)
		return
	}
	job.Status, job.AttemptCount = domain.JobStatusRunning, job.AttemptCount+1
	job.ErrorCode, job.ErrorMessage = "", ""
	if err := jobs.SaveJob(runCtx, job); err != nil {
		w.unsaved(log, d, err)
		return
	}
	log.Info("job started", "attempt", job.AttemptCount, "action", msg.Action, "mode", msg.Mode, "redelivered", d.Redelivered())
	result, err := w.deps.Executor.Execute(runCtx, req)
	if cause := context.Cause(runCtx); errors.Is(cause, ErrLockLost) {
		log.Error("the run lost its database lock", "error", err)
	}
	w.saveThenAck(ctx, log, d, jobs, outcome(job, msg, result, err))
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
