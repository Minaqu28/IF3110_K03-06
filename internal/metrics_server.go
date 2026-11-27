package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/metrics"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

var _ worker.Recorder = (*metrics.MigrationRecorder)(nil)

const defaultMetricsAddr = ":9090"

func startMetricsServer(reg *metrics.Registry) func() {
	addr := envOr("METRICS_ADDR", defaultMetricsAddr)
	if addr == "off" {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("metrics listening", slog.String("event", "metrics.start"), slog.String("addr", addr), slog.String("status", "ok"))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server failed", slog.String("event", "metrics.fatal"), slog.String("status", "error"), slog.String("error", err.Error()))
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
}
