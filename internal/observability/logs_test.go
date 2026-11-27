package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

type captureHandler struct {
	records *[]slog.Record
	attrs   []slog.Attr
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	r.AddAttrs(h.attrs...)
	*h.records = append(*h.records, r)
	return nil
}

func (h captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return captureHandler{records: h.records, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h captureHandler) WithGroup(string) slog.Handler { return h }

func TestLoggerFansOutAndMasksExtraHandler(t *testing.T) {
	var records []slog.Record
	var out bytes.Buffer
	logger := NewLogger(&out, slog.LevelInfo, &maskHandler{inner: captureHandler{records: &records}}, nil)
	logger.With("session_id", "abc123").Info("login", "event", "auth.login", "password", "hunter22")

	if !strings.Contains(out.String(), `"event":"auth.login"`) || strings.Contains(out.String(), "hunter22") || strings.Contains(out.String(), "abc123") {
		t.Fatalf("stdout log = %s", out.String())
	}
	if len(records) != 1 {
		t.Fatalf("extra handler got %d records, want 1", len(records))
	}
	got := map[string]string{}
	records[0].Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.String()
		return true
	})
	if got["event"] != "auth.login" || got["password"] != RedactedValue || got["session_id"] != RedactedValue {
		t.Fatalf("extra handler attrs = %v", got)
	}
}

func TestInitLogExportDisabledWithoutOTLP(t *testing.T) {
	for _, mode := range []string{"", "console", "none"} {
		h, shutdown, err := InitLogExport(context.Background(), "svc", mode)
		if err != nil || h != nil || shutdown(context.Background()) != nil {
			t.Fatalf("mode %q: handler=%v err=%v", mode, h, err)
		}
	}
}

func TestExporterModes(t *testing.T) {
	if got := ExporterModes(" Console , OTLP "); len(got) != 2 || got[0] != "console" || got[1] != "otlp" {
		t.Fatalf("modes = %v", got)
	}
	if got := ExporterModes(""); len(got) != 1 || got[0] != "console" {
		t.Fatalf("default modes = %v", got)
	}
}
