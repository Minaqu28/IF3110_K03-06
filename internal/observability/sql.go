package observability

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type SQLEvent struct {
	DatabaseID   int64
	DatabaseName string
	Query        string
	Parameters   []any
	ReadOnly     bool
	MigrationID  int64
	JobID        string
	ExecutionID  string
	Attempt      int
	Batch        int
}

func StartSQL(ctx context.Context, logger *slog.Logger, ev SQLEvent) (context.Context, trace.Span, time.Time) {
	if logger == nil {
		logger = slog.Default()
	}
	if ev.DatabaseID > 0 || ev.DatabaseName != "" {
		ctx = WithDatabase(ctx, ev.DatabaseID, ev.DatabaseName)
	}
	spanAttrs := []attribute.KeyValue{
		attribute.String("db.system", "postgresql"),
		attribute.String("db.statement", MaskString(ev.Query)),
		attribute.Bool("db.read_only", ev.ReadOnly),
	}
	if ev.DatabaseID > 0 {
		spanAttrs = append(spanAttrs, attribute.Int64("database.id", ev.DatabaseID))
	}
	if ev.DatabaseName != "" {
		spanAttrs = append(spanAttrs, attribute.String("database.name", ev.DatabaseName))
	}
	if ev.MigrationID > 0 {
		spanAttrs = append(spanAttrs, attribute.Int64("migration.id", ev.MigrationID))
	}
	if ev.JobID != "" {
		spanAttrs = append(spanAttrs, attribute.String("job.id", ev.JobID))
	}
	if ev.ExecutionID != "" {
		spanAttrs = append(spanAttrs, attribute.String("execution.id", ev.ExecutionID))
	}
	if ev.Attempt > 0 {
		spanAttrs = append(spanAttrs, attribute.Int("attempt", ev.Attempt))
	}
	if ev.Batch > 0 {
		spanAttrs = append(spanAttrs, attribute.Int("batch", ev.Batch))
	}

	ctx, span := Tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(spanAttrs...))
	start := time.Now()

	attrs := sqlBaseAttrs(ev)
	attrs = append(attrs, slog.String("event", "sql.start"), slog.String("status", "started"))
	logger.LogAttrs(ctx, slog.LevelInfo, "sql execution started", attrs...)
	return ctx, span, start
}

func EndSQL(ctx context.Context, logger *slog.Logger, span trace.Span, start time.Time, ev SQLEvent, rowCount *int64, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	durationMs := float64(time.Since(start).Microseconds()) / 1000.0
	attrs := sqlBaseAttrs(ev)
	attrs = append(attrs,
		slog.String("event", "sql.result"),
		slog.Float64("duration_ms", durationMs),
	)
	if rowCount != nil {
		attrs = append(attrs, slog.Int64("row_count", *rowCount))
		if span != nil {
			span.SetAttributes(attribute.Int64("db.row_count", *rowCount))
		}
	}

	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
		attrs = append(attrs,
			slog.String("status", "error"),
			slog.String("error", MaskString(err.Error())),
		)
		if code := SQLState(err); code != "" {
			attrs = append(attrs, slog.String("sqlstate", code))
			if span != nil {
				span.SetAttributes(attribute.String("db.sqlstate", code))
			}
		}
		if span != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, MaskString(err.Error()))
		}
	} else {
		attrs = append(attrs, slog.String("status", "ok"))
		if span != nil {
			span.SetStatus(codes.Ok, "")
		}
	}

	logger.LogAttrs(ctx, level, "sql execution finished", attrs...)
	if span != nil {
		span.End()
	}
}

func LogTxOutcome(ctx context.Context, logger *slog.Logger, ev SQLEvent, committed bool, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	event := "sql.rollback"
	status := "rolled_back"
	msg := "sql transaction rolled back"
	if committed {
		event = "sql.commit"
		status = "committed"
		msg = "sql transaction committed"
	}
	attrs := sqlBaseAttrs(ev)
	attrs = append(attrs, slog.String("event", event))
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
		status = "error"
		attrs = append(attrs, slog.String("error", MaskString(err.Error())))
		if code := SQLState(err); code != "" {
			attrs = append(attrs, slog.String("sqlstate", code))
		}
	}
	attrs = append(attrs, slog.String("status", status))
	logger.LogAttrs(ctx, level, msg, attrs...)
}

func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func sqlBaseAttrs(ev SQLEvent) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("query", MaskString(ev.Query)),
		slog.Bool("read_only", ev.ReadOnly),
	}
	if len(ev.Parameters) > 0 {
		attrs = append(attrs, slog.Any("parameters", MaskSQLParams(ev.Parameters)))
	}
	if ev.MigrationID > 0 {
		attrs = append(attrs, slog.Int64("migration_id", ev.MigrationID))
	}
	if ev.JobID != "" {
		attrs = append(attrs, slog.String("job_id", ev.JobID))
	}
	if ev.ExecutionID != "" {
		attrs = append(attrs, slog.String("execution_id", ev.ExecutionID))
	}
	if ev.Attempt > 0 {
		attrs = append(attrs, slog.Int("attempt", ev.Attempt))
	}
	if ev.Batch > 0 {
		attrs = append(attrs, slog.Int("batch", ev.Batch))
	}
	return attrs
}
