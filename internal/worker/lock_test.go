package worker

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestHandleSavesThroughTheFencedStore(t *testing.T) {
	h := newRunHarness()
	h.handle(context.Background(), message())
	if !slices.Equal(h.fenced, []string{"w1:7"}) {
		t.Fatalf("fenced stores = %v, want one for owner w1 and token 7", h.fenced)
	}
}

func TestHandleRetriesLaterWhenTheDatabaseIsLocked(t *testing.T) {
	h := newRunHarness()
	h.acquired = false
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h.worker.now = func() time.Time { return now }
	var saved domain.Job
	var at time.Time
	h.jobs.saveAfter = func(_ context.Context, job domain.Job, notBefore time.Time) error {
		h.events = append(h.events, "saveAfter:"+string(job.Status))
		saved, at = job, notBefore
		return nil
	}
	h.handle(context.Background(), message())
	if want := []string{"lock", "saveAfter:RETRY_WAIT", "ack"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	if saved.ErrorCode != ErrorCodeDatabaseLocked || saved.AttemptCount != 0 || !at.Equal(now.Add(DefaultLockWait)) || len(h.fenced) != 0 {
		t.Fatalf("saved %+v for %v, fenced %v", saved, at, h.fenced)
	}
}

func TestHandleDropsADuplicateOfARunningJob(t *testing.T) {
	h := newRunHarness()
	h.acquired = false
	h.jobs.getJob = func(context.Context, string) (domain.Job, error) {
		j := queuedJob()
		j.Status = domain.JobStatusRunning
		return j, nil
	}
	h.handle(context.Background(), message())
	if want := []string{"lock", "ack"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v; the holder or the sweeper owns this job", h.events, want)
	}
}

func TestHandleRequeuesWhenTheLockCannotBeChecked(t *testing.T) {
	h := newRunHarness()
	h.acquireErr = errors.New("platform down")
	h.handle(context.Background(), message())
	if want := []string{"lock", "requeue"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

func TestLostLockCancelsTheRunAndIsNotOverwritten(t *testing.T) {
	h := newRunHarness()
	h.worker.heartbeat = time.Millisecond
	h.held = func() (bool, error) { return false, nil }
	var cause error
	h.worker.deps.Executor = &mockMigrationExecutor{execute: func(ctx context.Context, _ domain.ExecutionRequest) (domain.ExecutionResult, error) {
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
			return domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: errorCodeInterrupted, AppliedBatches: 1}, nil
		case <-time.After(5 * time.Second):
			return domain.ExecutionResult{}, errors.New("the run was not cancelled")
		}
	}}
	h.jobs.saveAfter = func(context.Context, domain.Job, time.Time) error {
		h.events = append(h.events, "saveAfter:lost")
		return ErrLockLost
	}
	h.handle(context.Background(), message())
	if !errors.Is(cause, ErrLockLost) {
		t.Fatalf("run cancelled with %v, want lock lost", cause)
	}
	if want := []string{"lock", "authorize", "save:RUNNING", "saveAfter:lost", "ack", "release:w1:7"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

func TestHeartbeatErrorsAreRetried(t *testing.T) {
	h := newRunHarness()
	h.worker.heartbeat = time.Millisecond
	var beats atomic.Int32
	h.held = func() (bool, error) {
		if beats.Add(1) < 3 {
			return false, errors.New("blip")
		}
		return true, nil
	}
	h.worker.deps.Executor = &mockMigrationExecutor{execute: func(ctx context.Context, _ domain.ExecutionRequest) (domain.ExecutionResult, error) {
		deadline := time.After(time.Second)
		for beats.Load() < 5 {
			select {
			case <-ctx.Done():
				return domain.ExecutionResult{}, context.Cause(ctx)
			case <-deadline:
				return domain.ExecutionResult{}, errors.New("heartbeats stopped")
			case <-time.After(time.Millisecond):
			}
		}
		return domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}, nil
	}}
	h.handle(context.Background(), message())
	if last := h.saves[len(h.saves)-1]; last.Status != domain.JobStatusSucceeded {
		t.Fatalf("a failing heartbeat call must not stop the run: %+v", last)
	}
}
