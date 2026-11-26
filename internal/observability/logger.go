package observability

import (
	"context"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

type ContextHandler struct {
	inner slog.Handler
}

func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			return MaskAttr(a)
		},
	})
	return slog.New(&ContextHandler{inner: jsonHandler})
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
