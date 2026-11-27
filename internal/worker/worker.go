package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
	rec       Recorder
}

func New(deps Dependencies, id string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{deps: deps, id: id, log: log.With("worker", id), now: time.Now, retry: DefaultRetryPolicy(),
		heartbeat: DefaultHeartbeatInterval, lockWait: DefaultLockWait, rec: nopRecorder{}}
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

// ExecutionAuthorizer memvalidasi ulang izin, approval, dan kondisi target tepat sebelum worker memanggil migration engine.
type ExecutionAuthorizer interface {
	// AuthorizeExecution mengembalikan nil hanya jika request masih boleh dijalankan.
	// Implementasi harus membaca state terbaru, bukan mempercayai flag approval atau permission dari payload queue.
	AuthorizeExecution(ctx context.Context, request domain.ExecutionRequest) error
}

// MigrationExecutor menghubungkan worker dengan migration engine.
type MigrationExecutor interface {
	// Execute meneruskan satu request yang sudah direvalidasi ke engine. Retry job
	// yang sama harus tetap memakai execution ID yang sama agar idempotent.
	Execute(ctx context.Context, request domain.ExecutionRequest) (domain.ExecutionResult, error)
}

// JobStateStore menyimpan state job secara persisten dan terpisah dari broker.
type JobStateStore interface {
	// GetJob mengambil state job terbaru sebelum worker memproses atau melanjutkan
	// pesan yang dikirim ulang.
	GetJob(ctx context.Context, jobID string) (domain.Job, error)

	// SaveJob menyimpan perubahan status, attempt, error, atau progress sebelum
	// worker melakukan acknowledgement terhadap pesan.
	SaveJob(ctx context.Context, job domain.Job) error
}

// JobDelivery membungkus satu pesan queue tanpa mengikat worker pada library
// broker tertentu.
type JobDelivery interface {
	// Message mengembalikan payload job yang sudah berhasil didecode.
	Message() domain.MigrationJobMessage

	// Redelivered bernilai true jika broker pernah mengirim pesan ini sebelumnya.
	Redelivered() bool

	// Ack menandai pesan selesai dan hanya dipanggil setelah hasil job tersimpan.
	Ack() error

	// Requeue mengembalikan pesan untuk retry ketika error masih dapat dipulihkan.
	Requeue() error

	// Reject menolak pesan tanpa retry, misalnya untuk payload yang tidak valid.
	Reject() error
}

// JobConsumer menyediakan pesan job kepada worker.
type JobConsumer interface {
	// Receive menunggu satu delivery atau berhenti ketika context dibatalkan.
	Receive(ctx context.Context) (JobDelivery, error)
}

type DatabaseLocker interface {
	Acquire(ctx context.Context, databaseID int64, executionID, owner string) (token int64, acquired bool, err error)
	Heartbeat(ctx context.Context, databaseID int64, owner string, token int64) (held bool, err error)
	Release(ctx context.Context, databaseID int64, owner string, token int64) error
}

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

const (
	ErrorCodeTransient        = "TRANSIENT_ERROR"
	ErrorCodeTarget           = "TARGET_ERROR"
	ErrorCodeCommitUncertain  = "COMMIT_UNCERTAIN"
	ErrorCodeUnexpectedResult = "UNEXPECTED_RESULT"
	errorCodeInterrupted      = "INTERRUPTED"
)

func outcome(job domain.Job, msg domain.MigrationJobMessage, result domain.ExecutionResult, err error) domain.Job {
	job.ErrorCode, job.ErrorMessage = "", ""
	if err != nil {
		job.ErrorMessage = err.Error()
		switch migrationengine.ClassifyError(err) {
		case migrationengine.ErrorTransient:
			job.Status, job.ErrorCode = domain.JobStatusRetryWait, ErrorCodeTransient
		case migrationengine.ErrorUncertain:
			job.Status, job.ErrorCode = domain.JobStatusNeedsReconciliation, ErrorCodeCommitUncertain
		default:
			job.Status, job.ErrorCode = domain.JobStatusFailed, ErrorCodeTarget
		}
		return job
	}
	job.ErrorCode, job.ErrorMessage = result.ErrorCode, result.ErrorMessage
	if msg.Mode == domain.ExecutionModeBatch {
		job.Progress = progressOf(job.Progress, msg.Action, result)
	}
	switch result.Status {
	case domain.ExecutionStatusSucceeded:
		job.Status = domain.JobStatusSucceeded
	case domain.ExecutionStatusUnknown:
		job.Status = domain.JobStatusNeedsReconciliation
	case domain.ExecutionStatusFailed, domain.ExecutionStatusPartial, domain.ExecutionStatusPartiallyReverted:
		job.Status = domain.JobStatusFailed
		if retryable(result.ErrorCode) {
			job.Status = domain.JobStatusRetryWait
		}
	default:
		job.Status, job.ErrorCode = domain.JobStatusFailed, ErrorCodeUnexpectedResult
		job.ErrorMessage = "the engine returned status " + string(result.Status)
	}
	return job
}

func retryable(code string) bool {
	return code == errorCodeInterrupted || migrationengine.ClassifySQLState(code) == migrationengine.ErrorTransient
}

func progressOf(previous *domain.BatchProgress, action domain.MigrationAction, result domain.ExecutionResult) *domain.BatchProgress {
	p := &domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: result.AppliedBatches, AffectedRows: result.AffectedRows}
	if action == domain.MigrationActionRollback {
		p.Status = domain.BatchStatusCompensated
	}
	if previous != nil {
		p.Total, p.LastCursor = previous.Total, previous.LastCursor
	}
	return p
}

type activeRun struct {
	mu   sync.Mutex
	job  domain.Job
	jobs JobStore
}

type activeRuns struct {
	mu   sync.Mutex
	runs map[string]*activeRun
}

func (a *activeRuns) track(executionID string, job domain.Job, jobs JobStore) *activeRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs == nil {
		a.runs = map[string]*activeRun{}
	}
	run := &activeRun{job: job, jobs: jobs}
	a.runs[executionID] = run
	return run
}

func (a *activeRuns) untrack(executionID string, run *activeRun) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs[executionID] == run {
		delete(a.runs, executionID)
	}
}

func (a *activeRuns) get(executionID string) *activeRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[executionID]
}

func (r *activeRun) latest() domain.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}

func (w *Worker) ReportProgress(ctx context.Context, executionID string, progress domain.BatchProgress) error {
	run := w.active.get(executionID)
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if progress.Total == nil && run.job.Progress != nil {
		progress.Total = run.job.Progress.Total
	}
	job := run.job
	job.Progress = &progress
	if err := run.jobs.SaveJob(ctx, job); err != nil {
		return err
	}
	batches, rows := progress.Completed, progress.AffectedRows
	if prev := run.job.Progress; prev != nil {
		batches, rows = batches-prev.Completed, rows-prev.AffectedRows
	}
	if batches > 0 || rows > 0 {
		w.rec.BatchesCommitted(max(batches, 0), max(rows, 0))
	}
	run.job = job
	return nil
}

type Recorder interface {
	QueueWait(d time.Duration)
	LockWait(d time.Duration, acquired bool)
	JobFinished(action domain.MigrationAction, mode domain.ExecutionMode, status domain.JobStatus, d time.Duration)
	Retry(reason string)
	BatchesCommitted(batches int, rows int64)
}

type nopRecorder struct{}

func (nopRecorder) QueueWait(time.Duration)      {}
func (nopRecorder) LockWait(time.Duration, bool) {}
func (nopRecorder) JobFinished(domain.MigrationAction, domain.ExecutionMode, domain.JobStatus, time.Duration) {
}
func (nopRecorder) Retry(string)                {}
func (nopRecorder) BatchesCommitted(int, int64) {}

func (w *Worker) WithRecorder(r Recorder) *Worker {
	if r == nil {
		r = nopRecorder{}
	}
	w.rec = r
	return w
}

type JobStore interface {
	JobStateStore
	SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error
}

type RetryPolicy struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int
	Jitter      func() float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: 2 * time.Second, Max: 2 * time.Minute, MaxAttempts: 5, Jitter: rand.Float64}
}

func (p RetryPolicy) Backoff(attempt int) time.Duration {
	delay := p.Max
	if attempt < 1 {
		attempt = 1
	}
	if shift := attempt - 1; shift < 30 && p.Base<<shift < p.Max {
		delay = p.Base << shift
	}
	if p.Jitter != nil {
		delay += time.Duration(p.Jitter() * float64(delay) / 5)
	}
	return min(delay, p.Max)
}

func (p RetryPolicy) schedule(job domain.Job) (domain.Job, time.Duration) {
	switch job.Status {
	case domain.JobStatusRetryWait:
		if job.AttemptCount >= p.MaxAttempts {
			job.Status = domain.JobStatusFailed
			job.ErrorMessage = fmt.Sprintf("gave up after %d attempts: %s", job.AttemptCount, job.ErrorMessage)
			return job, 0
		}
		return job, p.Backoff(job.AttemptCount)
	case domain.JobStatusNeedsReconciliation:
		return job, p.Backoff(job.AttemptCount)
	}
	return job, 0
}

var ErrLockLost = errors.New("database lock lost")
