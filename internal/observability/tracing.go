package observability

import (
	"context"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/Minaqu28/IF3110_K03-06"

func InitTracing(ctx context.Context, serviceName, exporterMode string, w io.Writer) (func(context.Context) error, error) {
	if serviceName == "" {
		serviceName = "dbmp-platform"
	}
	if w == nil {
		w = os.Stdout
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}

	for _, mode := range ExporterModes(exporterMode) {
		switch mode {
		case "otlp":
			exp, err := otlptracehttp.New(ctx)
			if err != nil {
				return nil, err
			}
			opts = append(opts, sdktrace.WithBatcher(exp))
		case "none", "disabled":
		default:
			exp, err := stdouttrace.New(stdouttrace.WithWriter(w))
			if err != nil {
				return nil, err
			}
			opts = append(opts, sdktrace.WithSyncer(exp))
		}
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

func ExporterModes(raw string) []string {
	var modes []string
	for _, part := range strings.Split(raw, ",") {
		if m := strings.ToLower(strings.TrimSpace(part)); m != "" {
			modes = append(modes, m)
		}
	}
	if len(modes) == 0 {
		return []string{"console"}
	}
	return modes
}

func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

func InjectTraceContext(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	out := make(map[string]string, len(carrier))
	for k, v := range carrier {
		out[k] = v
	}
	return out
}

func ExtractTraceContext(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
