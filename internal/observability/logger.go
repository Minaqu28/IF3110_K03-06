package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

type ContextHandler struct {
	inner slog.Handler
}

func NewLogger(w io.Writer, level slog.Level, extra ...slog.Handler) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			return MaskAttr(a)
		},
	})
	var inner slog.Handler = jsonHandler
	if extra = slices.DeleteFunc(extra, func(h slog.Handler) bool { return h == nil }); len(extra) > 0 {
		inner = &fanoutHandler{handlers: append([]slog.Handler{jsonHandler}, extra...)}
	}
	return slog.New(&ContextHandler{inner: inner})
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	extra := make([]slog.Attr, 0, 6)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		extra = append(extra,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if cid := CorrelationIDFromContext(ctx); cid != "" {
		extra = append(extra, slog.String("correlation_id", cid))
	}
	if actor, ok := ActorFromContext(ctx); ok {
		if actor.ID > 0 {
			extra = append(extra, slog.Int64("actor_id", actor.ID))
		}
		if actor.Username != "" {
			extra = append(extra, slog.String("actor", actor.Username))
		}
	}
	if db, ok := DatabaseFromContext(ctx); ok {
		if db.ID > 0 {
			extra = append(extra, slog.Int64("database_id", db.ID))
		}
		if db.Name != "" {
			extra = append(extra, slog.String("database", db.Name))
		}
	}
	if len(extra) > 0 {
		cloned := record.Clone()
		cloned.AddAttrs(extra...)
		return h.inner.Handle(ctx, cloned)
	}
	return h.inner.Handle(ctx, record)
}

func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = MaskAttr(a)
	}
	return &ContextHandler{inner: h.inner.WithAttrs(masked)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}

func InitLogExport(ctx context.Context, serviceName, exporterMode string) (slog.Handler, func(context.Context) error, error) {
	if !slices.Contains(ExporterModes(exporterMode), "otlp") {
		return nil, func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, nil, err
	}
	exp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	provider := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	handler := otelslog.NewHandler(tracerName, otelslog.WithLoggerProvider(provider))
	return &maskHandler{inner: handler}, provider.Shutdown, nil
}

type maskHandler struct {
	inner slog.Handler
}

func (h *maskHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *maskHandler) Handle(ctx context.Context, record slog.Record) error {
	masked := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(a slog.Attr) bool {
		masked.AddAttrs(MaskAttr(a))
		return true
	})
	return h.inner.Handle(ctx, masked)
}

func (h *maskHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = MaskAttr(a)
	}
	return &maskHandler{inner: h.inner.WithAttrs(masked)}
}

func (h *maskHandler) WithGroup(name string) slog.Handler {
	return &maskHandler{inner: h.inner.WithGroup(name)}
}

type fanoutHandler struct {
	handlers []slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, inner := range h.handlers {
		if inner.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var first error
	for _, inner := range h.handlers {
		if !inner.Enabled(ctx, record.Level) {
			continue
		}
		if err := inner.Handle(ctx, record.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}

const RedactedValue = "[REDACTED]"

var sensitiveKeySubstrings = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"cookie",
	"session",
	"authorization",
	"credential",
	"dsn",
	"database_url",
	"api_key",
	"apikey",
}

var (
	dsnPattern        = regexp.MustCompile(`(?i)\bpostgres(?:ql)?://[^\s"'` + "`" + `]+`)
	kvPasswordPattern = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|session_id|cookie_session)\s*=\s*([^\s;,&'"]+)`)
)

func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, sub := range sensitiveKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func MaskString(s string) string {
	if s == "" {
		return s
	}
	s = dsnPattern.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			return raw
		}
		if _, hasPass := u.User.Password(); !hasPass {
			return raw
		}
		u.User = url.UserPassword(u.User.Username(), RedactedValue)
		return u.String()
	})
	s = kvPasswordPattern.ReplaceAllString(s, "$1="+RedactedValue)
	return s
}

func MaskAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, RedactedValue)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, MaskString(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		masked := make([]any, len(attrs))
		for i, ga := range attrs {
			masked[i] = MaskAttr(ga)
		}
		return slog.Group(a.Key, masked...)
	default:
		return a
	}
}

func MaskSQLParams(params []any) []any {
	if len(params) == 0 {
		return nil
	}
	out := make([]any, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case string:
			out[i] = MaskString(v)
		case []byte:
			out[i] = RedactedValue
		default:
			out[i] = v
		}
	}
	return out
}

type contextKey int

const (
	correlationIDKey contextKey = iota + 1
	actorKey
	databaseKey
)

type ActorInfo struct {
	ID       int64
	Username string
}

type DatabaseInfo struct {
	ID   int64
	Name string
}

func NewCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationIDKey, id)
}

func CorrelationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

func WithActor(ctx context.Context, id int64, username string) context.Context {
	if id <= 0 && username == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey, ActorInfo{ID: id, Username: username})
}

func ActorFromContext(ctx context.Context) (ActorInfo, bool) {
	if ctx == nil {
		return ActorInfo{}, false
	}
	info, ok := ctx.Value(actorKey).(ActorInfo)
	return info, ok
}

func WithDatabase(ctx context.Context, id int64, name string) context.Context {
	if id <= 0 && name == "" {
		return ctx
	}
	return context.WithValue(ctx, databaseKey, DatabaseInfo{ID: id, Name: name})
}

func DatabaseFromContext(ctx context.Context) (DatabaseInfo, bool) {
	if ctx == nil {
		return DatabaseInfo{}, false
	}
	info, ok := ctx.Value(databaseKey).(DatabaseInfo)
	return info, ok
}
