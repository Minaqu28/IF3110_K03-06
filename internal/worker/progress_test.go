package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestReportProgressSavesTheRunningJob(t *testing.T) {
	h := newRunHarness()
	msg := message()
	msg.Mode = domain.ExecutionModeBatch
	var reportErr error
	h.worker.deps.Executor = &mockMigrationExecutor{execute: func(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
		for i := 1; i <= 2; i++ {
			if err := h.worker.ReportProgress(ctx, req.ExecutionID, domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: i, LastCursor: "c", AffectedRows: int64(i)}); err != nil {
				reportErr = err
			}
		}
		return domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded, AppliedBatches: 2, AffectedRows: 2}, nil
	}}
	h.handle(context.Background(), msg)
	if reportErr != nil {
		t.Fatal(reportErr)
	}
	if len(h.saves) != 4 {
		t.Fatalf("saves = %d, want RUNNING, two progress saves, then the result", len(h.saves))
	}
	for i, s := range h.saves[1:3] {
		if s.Status != domain.JobStatusRunning || s.Progress == nil || s.Progress.Completed != i+1 || s.AttemptCount != 1 {
			t.Fatalf("progress save %d = %+v", i+1, s)
		}
	}
	if final := h.saves[3]; final.Status != domain.JobStatusSucceeded || final.Progress.Completed != 2 || final.Progress.LastCursor != "c" {
		t.Fatalf("final save = %+v; it must keep the reported cursor", final)
	}
	if err := h.worker.ReportProgress(context.Background(), msg.ExecutionID, domain.BatchProgress{Completed: 9}); err != nil || len(h.saves) != 4 {
		t.Fatalf("a report after the run must be ignored: err = %v, saves = %d", err, len(h.saves))
	}
}

func TestReportProgressStopsTheRunWhenTheSaveFails(t *testing.T) {
	h := newRunHarness()
	h.saveErrs = []error{nil, ErrLockLost}
	var reportErr error
	h.worker.deps.Executor = &mockMigrationExecutor{execute: func(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
		reportErr = h.worker.ReportProgress(ctx, req.ExecutionID, domain.BatchProgress{Completed: 1})
		return domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: errorCodeInterrupted, AppliedBatches: 1}, nil
	}}
	h.jobs.saveAfter = func(context.Context, domain.Job, time.Time) error { return ErrLockLost }
	h.handle(context.Background(), message())
	if !errors.Is(reportErr, ErrLockLost) {
		t.Fatalf("report err = %v, want the save error passed back to the executor", reportErr)
	}
}
