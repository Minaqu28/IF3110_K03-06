package httpserver

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const correlationIDHeader = "X-Correlation-ID"

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func observabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		cid := strings.TrimSpace(r.Header.Get(correlationIDHeader))
		if cid == "" {
			cid = observability.NewCorrelationID()
		}
		ctx = observability.WithCorrelationID(ctx, cid)
		w.Header().Set(correlationIDHeader, cid)

		ctx, span := observability.Tracer().Start(ctx, "http.request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
				attribute.String("correlation_id", cid),
			),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		durationMs := float64(time.Since(start).Microseconds()) / 1000.0
		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		level := slog.LevelInfo
		statusText := "ok"
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
			statusText = "error"
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		} else if rec.status >= http.StatusBadRequest {
			level = slog.LevelWarn
			statusText = "client_error"
		} else {
			span.SetStatus(codes.Ok, "")
		}

		slog.LogAttrs(ctx, level, "http request completed",
			slog.String("event", "http.request"),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("http_status", rec.status),
			slog.String("status", statusText),
			slog.Float64("duration_ms", durationMs),
		)
	})
}
