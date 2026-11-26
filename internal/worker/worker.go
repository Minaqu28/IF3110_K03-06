package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type Dependencies struct {
	Consumer   JobConsumer
	Jobs       JobStore
	Authorizer ExecutionAuthorizer
	Executor   MigrationExecutor
	Locks      DatabaseLocker
	Fence      func(owner string, token int64) JobStore
}

type Worker struct {
	deps  Dependencies
	id    string
	log   *slog.Logger
	now   func() time.Time
	retry RetryPolicy

	heartbeat time.Duration
	lockWait  time.Duration
	active    activeRuns
}

func New(deps Dependencies, id string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{deps: deps, id: id, log: log.With("worker", id), now: time.Now, retry: DefaultRetryPolicy(),
		heartbeat: DefaultHeartbeatInterval, lockWait: DefaultLockWait}
}

func (w *Worker) admit(ctx context.Context, log *slog.Logger, d JobDelivery) (domain.Job, bool) {
	msg := d.Message()
	if !wellFormed(msg) {
		log.Warn("rejecting malformed job message", "schema_version", msg.SchemaVersion)
		w.settle(log, "reject", d.Reject)
		return domain.Job{}, false
	}
	job, err := w.deps.Jobs.GetJob(ctx, msg.JobID)
	if errors.Is(err, admin.ErrNotFound) {
		log.Warn("rejecting a message for an unknown job")
		w.settle(log, "reject", d.Reject)
		return domain.Job{}, false
	}
	if err != nil {
		log.Error("could not load the job; requeueing", "error", err)
		w.settle(log, "requeue", d.Requeue)
		return domain.Job{}, false
	}
	if job.ExecutionID != msg.ExecutionID || job.DatabaseID != msg.DatabaseID {
		log.Warn("rejecting a message that does not match its job", "job_execution_id", job.ExecutionID)
		w.settle(log, "reject", d.Reject)
		return domain.Job{}, false
	}
	if job.Status.Terminal() {
		log.Info("job already finished; acknowledging duplicate delivery", "status", job.Status)
		w.settle(log, "ack", d.Ack)
		return domain.Job{}, false
	}
	return job, true
}

func deliveryLog(log *slog.Logger, msg domain.MigrationJobMessage) *slog.Logger {
	return log.With("job_id", msg.JobID, "execution_id", msg.ExecutionID, "database_id", msg.DatabaseID)
}

func wellFormed(msg domain.MigrationJobMessage) bool {
	return msg.SchemaVersion == domain.MigrationJobSchemaVersion && msg.JobID != "" && msg.ExecutionID != "" &&
		msg.DatabaseID > 0 && msg.RevisionID > 0 && msg.Action.Valid() &&
		(msg.Mode == domain.ExecutionModeNormal || msg.Mode == domain.ExecutionModeBatch)
}

func (w *Worker) settle(log *slog.Logger, action string, settle func() error) {
	if err := settle(); err != nil {
		log.Error("could not settle the delivery", "action", action, "error", err)
	}
}
