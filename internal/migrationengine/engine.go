package migrationengine

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

// ExecutionFinisher persists the outcome of a migration execution in platform storage.
type ExecutionFinisher interface {
	FinishExecution(ctx context.Context, result domain.ExecutionResult) error
}

// Engine coordinates revision lookup, target execution, and append-only attempt history.
type Engine struct {
	revisions RevisionStore
	history   HistoryStore
	target    TargetExecutor
	finisher  ExecutionFinisher
	now       func() time.Time
}

// NewEngine constructs a migration Engine. finisher may be nil in isolated tests.
func NewEngine(revisions RevisionStore, history HistoryStore, target TargetExecutor, finisher ExecutionFinisher) *Engine {
	return &Engine{
		revisions: revisions,
		history:   history,
		target:    target,
		finisher:  finisher,
		now:       time.Now,
	}
}

// Execute runs a single validated execution request against the target database and records the attempt.
func (e *Engine) Execute(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
	if req.ExecutionID == "" || !req.Action.Valid() {
		return domain.ExecutionResult{}, admin.ErrInvalidInput
	}
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

	sqlSnapshot := rev.UpSQL
	if req.Action == domain.MigrationActionRollback {
		sqlSnapshot = rev.DownSQL
	}

	started := e.now()
	res, execErr := e.target.Execute(ctx, req, rev)
	finished := e.now()

	attempt := buildAttempt(req.ExecutionID, sqlSnapshot, started, finished, res, execErr)
	if e.history != nil {
		if hErr := e.history.AppendAttempt(context.WithoutCancel(ctx), attempt); hErr != nil {
			slog.ErrorContext(ctx, "append execution attempt failed",
				slog.String("event", "migration.history"),
				slog.String("status", "error"),
				slog.String("execution_id", req.ExecutionID),
				slog.Int64("migration_id", req.MigrationID),
				slog.String("error", hErr.Error()),
			)
		}
	}

	if e.finisher != nil {
		if execErr == nil {
			if fErr := e.finisher.FinishExecution(context.WithoutCancel(ctx), res); fErr != nil {
				slog.ErrorContext(ctx, "finish execution record failed",
					slog.String("event", "migration.finish"),
					slog.String("status", "error"),
					slog.String("execution_id", req.ExecutionID),
					slog.String("error", fErr.Error()),
				)
			}
		} else if errors.Is(execErr, ErrCommitUncertain) {
			uncertain := domain.ExecutionResult{
				ExecutionID:  req.ExecutionID,
				Status:       domain.ExecutionStatusUnknown,
				ErrorCode:    "COMMIT_UNCERTAIN",
				ErrorMessage: execErr.Error(),
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

	logExecutionAttempt(ctx, span, req, rev, attempt, res, execErr)
	return res, execErr
}

func buildAttempt(executionID, sqlSnapshot string, started, finished time.Time, res domain.ExecutionResult, execErr error) domain.ExecutionAttempt {
	attempt := domain.ExecutionAttempt{
		ExecutionID: executionID,
		Status:      res.Status,
		SQLSnapshot: sqlSnapshot,
		ErrorCode:   res.ErrorCode,
		ErrorMessage: res.ErrorMessage,
		StartedAt:   res.StartedAt,
		FinishedAt:  res.FinishedAt,
	}
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = started
	}
	if attempt.FinishedAt == nil {
		attempt.FinishedAt = &finished
	}
	if execErr != nil {
		if errors.Is(execErr, ErrCommitUncertain) {
			attempt.Status = domain.ExecutionStatusUnknown
			attempt.ErrorCode = "COMMIT_UNCERTAIN"
		} else if attempt.Status == "" {
			attempt.Status = domain.ExecutionStatusFailed
		}
		if attempt.ErrorMessage == "" {
			attempt.ErrorMessage = execErr.Error()
		}
	}
	if attempt.Status == "" {
		attempt.Status = domain.ExecutionStatusFailed
	}
	return attempt
}

func logExecutionAttempt(ctx context.Context, span trace.Span, req domain.ExecutionRequest, rev domain.MigrationRevision, attempt domain.ExecutionAttempt, res domain.ExecutionResult, execErr error) {
	durationMs := float64(attempt.FinishedAt.Sub(attempt.StartedAt).Microseconds()) / 1000.0
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
