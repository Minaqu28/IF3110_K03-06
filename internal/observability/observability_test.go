package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLoggerMasksSensitiveDataAndCorrelatesTrace(t *testing.T) {
	var spanBuf bytes.Buffer
	shutdown, err := InitTracing(context.Background(), "test-svc", "console", &spanBuf)
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	var logBuf bytes.Buffer
	logger := NewLogger(&logBuf, slog.LevelInfo)

	ctx := WithCorrelationID(context.Background(), "corr-123")
	ctx = WithActor(ctx, 42, "alice")
	ctx = WithDatabase(ctx, 7, "shop")
	ctx, span := Tracer().Start(ctx, "api.test")
	wantTraceID := span.SpanContext().TraceID().String()
	wantSpanID := span.SpanContext().SpanID().String()

	logger.InfoContext(ctx, "auth attempt",
		slog.String("event", "auth.login"),
		slog.String("password", "super-secret-password"),
		slog.String("cookie_session", "sess-xyz-999"),
		slog.String("dsn", "postgres://admin:topsecret@db:5432/app"),
		slog.String("note", "connecting to postgres://user:hiddenpass@localhost:5432/db"),
	)
	span.End()

	var entry map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid json log: %v\nraw: %s", err, logBuf.String())
	}
	if entry["trace_id"] != wantTraceID {
		t.Errorf("trace_id = %v, want %s", entry["trace_id"], wantTraceID)
	}
	if entry["span_id"] != wantSpanID {
		t.Errorf("span_id = %v, want %s", entry["span_id"], wantSpanID)
	}
	if entry["correlation_id"] != "corr-123" {
		t.Errorf("correlation_id = %v, want corr-123", entry["correlation_id"])
	}
	if entry["actor"] != "alice" {
		t.Errorf("actor = %v, want alice", entry["actor"])
	}
	if entry["database"] != "shop" {
		t.Errorf("database = %v, want shop", entry["database"])
	}
	for _, key := range []string{"password", "cookie_session", "dsn"} {
		if entry[key] != RedactedValue {
			t.Errorf("%s = %v, want %s", key, entry[key], RedactedValue)
		}
	}
	raw := logBuf.String()
	for _, leaked := range []string{"super-secret-password", "sess-xyz-999", "topsecret", "hiddenpass"} {
		if strings.Contains(raw, leaked) {
			t.Errorf("log leaked sensitive value %q: %s", leaked, raw)
		}
	}
	if !strings.Contains(spanBuf.String(), wantTraceID) {
		t.Errorf("console span output missing trace_id %s: %s", wantTraceID, spanBuf.String())
	}
}

func TestTraceContextQueuePropagation(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "test-queue", "none", nil)
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	pubCtx, pubSpan := Tracer().Start(context.Background(), "queue.publish")
	carrier := InjectTraceContext(pubCtx)
	pubSpan.End()

	if carrier["traceparent"] == "" {
		t.Fatalf("expected traceparent in carrier, got %+v", carrier)
	}

	workerCtx := ExtractTraceContext(context.Background(), carrier)
	workerCtx, workerSpan := Tracer().Start(workerCtx, "worker.consume")
	defer workerSpan.End()

	if got, want := workerSpan.SpanContext().TraceID(), pubSpan.SpanContext().TraceID(); got != want {
		t.Fatalf("worker trace_id = %s, want %s", got, want)
	}
	if workerSpan.SpanContext().SpanID() == pubSpan.SpanContext().SpanID() {
		t.Fatal("worker span_id should be a child span, not identical to publisher span_id")
	}
}

func TestSQLLoggingStartResultAndTxOutcome(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "test-sql", "none", nil)
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	var logBuf bytes.Buffer
	logger := NewLogger(&logBuf, slog.LevelInfo)

	ev := SQLEvent{
		DatabaseID:   3,
		DatabaseName: "analytics",
		Query:        "UPDATE orders SET status = $1 WHERE id > $2",
		Parameters:   []any{"paid", 100},
		MigrationID:  11,
		JobID:        "job-1",
		ExecutionID:  "exec-1",
		Attempt:      2,
		Batch:        4,
	}
	ctx, span, start := StartSQL(context.Background(), logger, ev)
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	rows := int64(0)
	EndSQL(ctx, logger, span, start, ev, &rows, pgErr)
	LogTxOutcome(ctx, logger, ev, false, nil)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3:\n%s", len(lines), logBuf.String())
	}

	var startLog, resultLog, rbLog map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &startLog); err != nil {
		t.Fatalf("startLog json: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &resultLog); err != nil {
		t.Fatalf("resultLog json: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &rbLog); err != nil {
		t.Fatalf("rbLog json: %v", err)
	}

	if startLog["event"] != "sql.start" || startLog["status"] != "started" {
		t.Errorf("unexpected startLog: %+v", startLog)
	}
	if resultLog["event"] != "sql.result" || resultLog["status"] != "error" || resultLog["sqlstate"] != "23505" {
		t.Errorf("unexpected resultLog: %+v", resultLog)
	}
	if resultLog["job_id"] != "job-1" || resultLog["execution_id"] != "exec-1" {
		t.Errorf("missing correlation ids in resultLog: %+v", resultLog)
	}
	if rbLog["event"] != "sql.rollback" || rbLog["status"] != "rolled_back" {
		t.Errorf("unexpected rbLog: %+v", rbLog)
	}
}
