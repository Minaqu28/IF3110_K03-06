package observability

import (
	"context"
	"log/slog"
	"slices"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

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
