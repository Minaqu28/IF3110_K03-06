package migrationengine

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestProgressSinkReport(t *testing.T) {
	var gotID string
	var gotProgress domain.BatchProgress
	sink := ProgressSink(func(_ context.Context, id string, p domain.BatchProgress) error {
		gotID, gotProgress = id, p
		return nil
	})
	want := domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: 3, LastCursor: "3000", AffectedRows: 3000}

	if err := sink.Report(context.Background(), "exec-1", want); err != nil {
		t.Fatal(err)
	}
	if gotID != "exec-1" || gotProgress != want {
		t.Fatalf("sink got (%q, %+v), want (%q, %+v)", gotID, gotProgress, "exec-1", want)
	}
}

func TestProgressSinkReportReturnsError(t *testing.T) {
	boom := errors.New("boom")
	sink := ProgressSink(func(context.Context, string, domain.BatchProgress) error { return boom })
	if err := sink.Report(context.Background(), "exec-1", domain.BatchProgress{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestNilProgressSinkIsNoop(t *testing.T) {
	var sink ProgressSink
	if err := sink.Report(context.Background(), "exec-1", domain.BatchProgress{}); err != nil {
		t.Fatalf("nil sink returned %v", err)
	}
}
