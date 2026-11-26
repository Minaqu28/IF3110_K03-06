package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
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
	req := requestFrom(msg)
	if err := w.deps.Authorizer.AuthorizeExecution(ctx, req); err != nil {
		code, refused := refusalCode(err)
		if !refused {
			log.Error("could not authorize the job; requeueing", "error", err)
			w.settle(log, "requeue", d.Requeue)
			return
		}
		log.Warn("job refused at run time", "error_code", code, "error", err)
		job.Status, job.ErrorCode, job.ErrorMessage = domain.JobStatusFailed, code, err.Error()
		w.saveThenAck(ctx, log, d, job)
		return
	}
	job.Status, job.AttemptCount = domain.JobStatusRunning, job.AttemptCount+1
	job.ErrorCode, job.ErrorMessage = "", ""
	if err := w.deps.Jobs.SaveJob(ctx, job); err != nil {
		w.unsaved(log, d, err)
		return
	}
	log.Info("job started", "attempt", job.AttemptCount, "action", msg.Action, "mode", msg.Mode, "redelivered", d.Redelivered())
	result, err := w.deps.Executor.Execute(ctx, req)
	w.saveThenAck(ctx, log, d, outcome(job, msg, result, err))
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

func (w *Worker) saveThenAck(ctx context.Context, log *slog.Logger, d JobDelivery, job domain.Job) {
	job, delay := w.retry.schedule(job)
	ctx = context.WithoutCancel(ctx)
	var err error
	if delay > 0 {
		err = w.deps.Jobs.SaveJobAfter(ctx, job, w.now().Add(delay))
	} else {
		err = w.deps.Jobs.SaveJob(ctx, job)
	}
	if err != nil {
		w.unsaved(log, d, err)
		return
	}
	log.Info("job saved", "status", job.Status, "attempt", job.AttemptCount, "error_code", job.ErrorCode, "retry_in", delay)
	w.settle(log, "ack", d.Ack)
}

func (w *Worker) unsaved(log *slog.Logger, d JobDelivery, err error) {
	if errors.Is(err, admin.ErrConflict) {
		log.Info("job was finished elsewhere; acknowledging", "error", err)
		w.settle(log, "ack", d.Ack)
		return
	}
	log.Error("could not save the job; requeueing without ack", "error", err)
	w.settle(log, "requeue", d.Requeue)
}
