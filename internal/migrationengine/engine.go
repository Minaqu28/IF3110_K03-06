package migrationengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	ErrorCodeDefinitionChanged = "DEFINITION_CHANGED"
	ErrorCodeInvalidBatch      = "INVALID_BATCH"
	ErrorCodeTargetError       = "TARGET_ERROR"
	ErrorCodeTransientError    = "TRANSIENT_ERROR"
	ErrorCodeCommitUncertain   = "COMMIT_UNCERTAIN"
)

var ErrAttemptNotRecorded = errors.New("the execution attempt could not be recorded")

type ExecutionFinisher interface {
	FinishExecution(ctx context.Context, result domain.ExecutionResult) error
}

type Engine struct {
	revisions RevisionStore
	history   HistoryStore
	target    TargetExecutor
	finisher  ExecutionFinisher
	now       func() time.Time
}

func NewEngine(revisions RevisionStore, history HistoryStore, target TargetExecutor) *Engine {
	return &Engine{
		revisions: revisions,
		history:   history,
		target:    target,
		now:       time.Now,
	}
}

func (e *Engine) WithFinisher(finisher ExecutionFinisher) *Engine {
	e.finisher = finisher
	return e
}

func (e *Engine) Execute(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
	started := e.now()
	ctx = observability.WithDatabase(observability.WithActor(ctx, req.ActorID, ""), req.DatabaseID, "")
	ctx, span := observability.Tracer().Start(ctx, "migration.execute",
		trace.WithAttributes(
			attribute.String("execution.id", req.ExecutionID),
			attribute.String("job.id", req.JobID),
			attribute.Int64("database.id", req.DatabaseID),
			attribute.Int64("migration.id", req.MigrationID),
			attribute.Int64("revision.id", req.RevisionID),
			attribute.String("migration.action", string(req.Action)),
			attribute.String("migration.mode", string(req.Mode)),
		),
	)
	defer span.End()

	rev, err := e.revisions.GetRevision(ctx, req.DatabaseID, req.RevisionID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return domain.ExecutionResult{}, err
	}

	var (
		result domain.ExecutionResult
		runErr error
	)
	if refusal := checkRevision(rev); refusal.ErrorCode != "" {
		result = e.finish(refusal, req, started)
	} else {
		result, runErr = e.target.Execute(ctx, req, rev)
	}

	attempt := e.attemptFor(req, rev, result, runErr, started)
	logExecutionAttempt(ctx, span, req, rev, attempt, result, runErr)

	if e.history != nil {
		if hErr := e.history.AppendAttempt(context.WithoutCancel(ctx), attempt); hErr != nil {
			slog.ErrorContext(ctx, "append execution attempt failed",
				slog.String("event", "migration.history"),
				slog.String("status", "error"),
				slog.String("execution_id", req.ExecutionID),
				slog.Int64("migration_id", req.MigrationID),
				slog.String("error", hErr.Error()),
			)
			return result, errors.Join(runErr, fmt.Errorf("%w: %w", ErrAttemptNotRecorded, hErr))
		}
	}

	if e.finisher != nil {
		if runErr == nil {
			if fErr := e.finisher.FinishExecution(context.WithoutCancel(ctx), result); fErr != nil {
				slog.ErrorContext(ctx, "finish execution record failed",
					slog.String("event", "migration.finish"),
					slog.String("status", "error"),
					slog.String("execution_id", req.ExecutionID),
					slog.String("error", fErr.Error()),
				)
			}
		} else if errors.Is(runErr, ErrCommitUncertain) {
			finished := e.now()
			uncertain := domain.ExecutionResult{
				ExecutionID:  req.ExecutionID,
				Status:       domain.ExecutionStatusUnknown,
				ErrorCode:    ErrorCodeCommitUncertain,
				ErrorMessage: runErr.Error(),
				StartedAt:    started,
				FinishedAt:   &finished,
			}
			if fErr := e.finisher.FinishExecution(context.WithoutCancel(ctx), uncertain); fErr != nil {
				slog.ErrorContext(ctx, "mark execution unknown failed",
					slog.String("event", "migration.finish"),
					slog.String("status", "error"),
					slog.String("execution_id", req.ExecutionID),
					slog.String("error", fErr.Error()),
				)
			}
		}
	}

	return result, runErr
}

func logExecutionAttempt(ctx context.Context, span trace.Span, req domain.ExecutionRequest, rev domain.MigrationRevision, attempt domain.ExecutionAttempt, res domain.ExecutionResult, execErr error) {
	var durationMs float64
	if attempt.FinishedAt != nil && !attempt.StartedAt.IsZero() {
		durationMs = float64(attempt.FinishedAt.Sub(attempt.StartedAt).Microseconds()) / 1000.0
	}
	level := slog.LevelInfo
	if execErr != nil || attempt.Status != domain.ExecutionStatusSucceeded {
		level = slog.LevelError
		if execErr != nil {
			span.RecordError(execErr)
			span.SetStatus(codes.Error, execErr.Error())
		} else {
			span.SetStatus(codes.Error, attempt.ErrorMessage)
		}
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.SetAttributes(
		attribute.String("migration.status", string(attempt.Status)),
		attribute.Int("migration.applied_batches", res.AppliedBatches),
		attribute.Int64("migration.affected_rows", res.AffectedRows),
	)
	attrs := []slog.Attr{
		slog.String("event", "migration.execute"),
		slog.String("job_id", req.JobID),
		slog.String("execution_id", req.ExecutionID),
		slog.Int64("migration_id", req.MigrationID),
		slog.Int64("revision_id", req.RevisionID),
		slog.Int("revision", rev.Revision),
		slog.String("action", string(req.Action)),
		slog.String("mode", string(req.Mode)),
		slog.String("status", string(attempt.Status)),
		slog.String("query", observability.MaskString(attempt.SQLSnapshot)),
		slog.Int("applied_batches", res.AppliedBatches),
		slog.Int64("affected_rows", res.AffectedRows),
		slog.Float64("duration_ms", durationMs),
	}
	if attempt.ErrorCode != "" {
		attrs = append(attrs, slog.String("error_code", attempt.ErrorCode))
	}
	if attempt.ErrorMessage != "" {
		attrs = append(attrs, slog.String("error", attempt.ErrorMessage))
	}
	slog.LogAttrs(ctx, level, "migration execution attempt finished", attrs...)
}

func (e *Engine) attemptFor(req domain.ExecutionRequest, rev domain.MigrationRevision, result domain.ExecutionResult, runErr error, started time.Time) domain.ExecutionAttempt {
	attempt := domain.ExecutionAttempt{
		ExecutionID: req.ExecutionID, Status: result.Status, SQLSnapshot: rev.UpSQL,
		ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage, StartedAt: started, FinishedAt: result.FinishedAt,
	}
	if req.Action == domain.MigrationActionRollback {
		attempt.SQLSnapshot = rev.DownSQL
	}
	if !result.StartedAt.IsZero() {
		attempt.StartedAt = result.StartedAt
	}
	if runErr != nil {
		attempt.Status, attempt.ErrorCode, attempt.ErrorMessage = domain.ExecutionStatusFailed, ErrorCodeTargetError, runErr.Error()
		switch ClassifyError(runErr) {
		case ErrorTransient:
			attempt.ErrorCode = ErrorCodeTransientError
		case ErrorUncertain:
			attempt.Status, attempt.ErrorCode = domain.ExecutionStatusUnknown, ErrorCodeCommitUncertain
		}
	}
	if attempt.FinishedAt == nil {
		finished := e.now()
		attempt.FinishedAt = &finished
	}
	return attempt
}

func checkRevision(rev domain.MigrationRevision) domain.ExecutionResult {
	if rev.DefinitionHash == "" || domain.ComputeDefinitionHash(rev) != rev.DefinitionHash {
		return domain.ExecutionResult{
			Status: domain.ExecutionStatusFailed, ErrorCode: ErrorCodeDefinitionChanged,
			ErrorMessage: "the stored revision no longer matches its approved definition hash",
		}
	}
	if rev.Batch != nil {
		if err := ValidateBatch(rev); err != nil {
			return domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: ErrorCodeInvalidBatch, ErrorMessage: err.Error()}
		}
	}
	return domain.ExecutionResult{}
}

func (e *Engine) finish(result domain.ExecutionResult, req domain.ExecutionRequest, started time.Time) domain.ExecutionResult {
	finished := e.now()
	result.ExecutionID, result.RevertsExecutionID = req.ExecutionID, req.TargetExecutionID
	result.StartedAt, result.FinishedAt = started, &finished
	return result
}

// RevisionStore menyediakan definisi migration immutable yang dibutuhkan engine.
type RevisionStore interface {
	// GetRevision mengambil satu revision pada database target. Implementasi harus
	// mengembalikan revision persis seperti yang tersimpan tanpa mengubah SQL-nya.
	GetRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error)

	// ListRevisions mengambil seluruh revision milik satu database untuk daftar
	// migration dan pemeriksaan urutan versi.
	ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error)
}

// HistoryStore menyimpan riwayat setiap percobaan eksekusi, termasuk kegagalan.
type HistoryStore interface {
	// AppendAttempt menambahkan satu catatan attempt baru. Implementasi tidak boleh
	// menimpa atau menghapus attempt lama karena history bersifat append-only.
	AppendAttempt(ctx context.Context, attempt domain.ExecutionAttempt) error

	// ListAttempts mengambil riwayat attempt pada satu database untuk ditampilkan
	// dan diproses oleh aturan Hide Rollback.
	ListAttempts(ctx context.Context, databaseID int64) ([]domain.ExecutionAttempt, error)
}

// TargetExecutor adalah batas antara engine dan PostgreSQL target.
type TargetExecutor interface {
	// Execute menjalankan revision yang telah dipilih sesuai action pada request.
	// Implementasi menangani transaction, ledger, checkpoint, dan idempotency,
	// lalu mengembalikan hasil tanpa mengurus HTTP, approval, atau queue.
	Execute(ctx context.Context, request domain.ExecutionRequest, revision domain.MigrationRevision) (domain.ExecutionResult, error)
}

type ProgressSink func(ctx context.Context, executionID string, progress domain.BatchProgress) error

func (s ProgressSink) Report(ctx context.Context, executionID string, progress domain.BatchProgress) error {
	if s == nil {
		return nil
	}
	return s(ctx, executionID, progress)
}

type ErrorClass int

const (
	ErrorPermanent ErrorClass = iota
	ErrorTransient
	ErrorUncertain
)

var (
	ErrCommitUncertain = errors.New("the commit outcome is unknown")
	ErrTargetBusy      = errors.New("another migration is running on the target database")
)

var transientSQLStates = map[string]bool{
	"40001": true,
	"40P01": true,
	"55P03": true,
	"57P01": true,
	"57P02": true,
	"57P03": true,
	"53300": true,
}

func ClassifySQLState(code string) ErrorClass {
	if transientSQLStates[code] || (len(code) == 5 && strings.HasPrefix(code, "08")) {
		return ErrorTransient
	}
	return ErrorPermanent
}

func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorPermanent
	}
	if errors.Is(err, ErrCommitUncertain) {
		return ErrorUncertain
	}
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return ClassifySQLState(coded.SQLState())
	}
	if errors.Is(err, ErrTargetBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return ErrorTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorTransient
	}
	return ErrorPermanent
}
