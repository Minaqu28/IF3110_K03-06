package worker

import (
	"context"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type delayingJobs struct {
	*mockJobStateStore
	saveAfter func(context.Context, domain.Job, time.Time) error
}

func (d *delayingJobs) SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error {
	if d.saveAfter == nil {
		return errUnexpectedWorkerMockCall
	}
	return d.saveAfter(ctx, job, notBefore)
}

func TestBackoffDoublesAndIsCapped(t *testing.T) {
	p := RetryPolicy{Base: 2 * time.Second, Max: 2 * time.Minute, MaxAttempts: 5}
	want := []time.Duration{2, 4, 8, 16, 32, 64, 120, 120}
	for i, w := range want {
		if got := p.Backoff(i + 1); got != w*time.Second {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if p.Backoff(0) != 2*time.Second || p.Backoff(1000) != 2*time.Minute {
		t.Fatalf("edge attempts: %v, %v", p.Backoff(0), p.Backoff(1000))
	}
	p.Jitter = func() float64 { return 1 }
	if got := p.Backoff(1); got != 2400*time.Millisecond {
		t.Fatalf("full jitter on attempt 1 = %v, want +20%%", got)
	}
	if got := p.Backoff(7); got != 2*time.Minute {
		t.Fatalf("jitter must not push past the cap: %v", got)
	}
	d := DefaultRetryPolicy()
	for range 100 {
		if b := d.Backoff(3); b < 8*time.Second || b > 9600*time.Millisecond {
			t.Fatalf("default jitter out of range: %v", b)
		}
	}
}

func TestScheduleGivesUpOnlyOnRetries(t *testing.T) {
	p := RetryPolicy{Base: time.Second, Max: time.Minute, MaxAttempts: 3}
	retry := domain.Job{Status: domain.JobStatusRetryWait, AttemptCount: 2, ErrorMessage: "deadlock"}
	if job, delay := p.schedule(retry); job.Status != domain.JobStatusRetryWait || delay != 2*time.Second {
		t.Fatalf("attempt 2 = %s after %v", job.Status, delay)
	}
	retry.AttemptCount = 3
	job, delay := p.schedule(retry)
	if job.Status != domain.JobStatusFailed || delay != 0 || job.ErrorMessage != "gave up after 3 attempts: deadlock" {
		t.Fatalf("last attempt = %+v after %v", job, delay)
	}
	uncertain := domain.Job{Status: domain.JobStatusNeedsReconciliation, AttemptCount: 50}
	if job, delay := p.schedule(uncertain); job.Status != domain.JobStatusNeedsReconciliation || delay != time.Minute {
		t.Fatalf("reconciliation must never give up: %s after %v", job.Status, delay)
	}
	for _, s := range []domain.JobStatus{domain.JobStatusSucceeded, domain.JobStatusFailed} {
		if _, delay := p.schedule(domain.Job{Status: s, AttemptCount: 9}); delay != 0 {
			t.Fatalf("%s delayed by %v", s, delay)
		}
	}
}

func TestHandleDelaysARetry(t *testing.T) {
	h := newRunHarness()
	h.execErr = context.DeadlineExceeded
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h.worker.now = func() time.Time { return now }
	h.worker.retry = RetryPolicy{Base: 2 * time.Second, Max: time.Minute, MaxAttempts: 5}
	var notBefore time.Time
	h.jobs.saveAfter = func(_ context.Context, job domain.Job, at time.Time) error {
		h.events = append(h.events, "saveAfter:"+string(job.Status))
		notBefore = at
		return nil
	}
	h.handle(context.Background(), message())
	if want := []string{"authorize", "save:RUNNING", "execute", "saveAfter:RETRY_WAIT", "ack"}; !equalEvents(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	if !notBefore.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("retry at %v, want 2s after attempt 1", notBefore)
	}
}

func TestHandleGivesUpAfterTheLastAttempt(t *testing.T) {
	h := newRunHarness()
	h.execErr = context.DeadlineExceeded
	h.worker.retry.MaxAttempts = 1
	h.handle(context.Background(), message())
	if last := h.saves[len(h.saves)-1]; last.Status != domain.JobStatusFailed || last.ErrorCode != ErrorCodeTransient || h.events[len(h.events)-1] != "ack" {
		t.Fatalf("final save = %+v, events = %v", last, h.events)
	}
}
