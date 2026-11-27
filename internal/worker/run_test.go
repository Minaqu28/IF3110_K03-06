package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type loopHarness struct {
	mu        sync.Mutex
	inbox     chan JobDelivery
	acks      int
	final     map[string]domain.JobStatus
	running   int
	peak      int
	release   chan struct{}
	execute   func(context.Context) (domain.ExecutionResult, error)
	worker    *Worker
	databases map[string]int64
}

func newLoopHarness() *loopHarness {
	h := &loopHarness{inbox: make(chan JobDelivery, 16), final: map[string]domain.JobStatus{}, release: make(chan struct{}), databases: map[string]int64{}}
	save := func(_ context.Context, job domain.Job) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.final[job.ID] = job.Status
		return nil
	}
	jobs := &delayingJobs{
		mockJobStateStore: &mockJobStateStore{
			getJob: func(_ context.Context, id string) (domain.Job, error) {
				h.mu.Lock()
				defer h.mu.Unlock()
				return domain.Job{ID: id, ExecutionID: "exec-" + id, DatabaseID: h.databases[id], Status: domain.JobStatusQueued}, nil
			},
			saveJob: save,
		},
		saveAfter: func(ctx context.Context, job domain.Job, _ time.Time) error { return save(ctx, job) },
	}
	exec := &mockMigrationExecutor{execute: func(ctx context.Context, _ domain.ExecutionRequest) (domain.ExecutionResult, error) {
		h.mu.Lock()
		h.running++
		h.peak = max(h.peak, h.running)
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.running--
			h.mu.Unlock()
		}()
		if h.execute != nil {
			return h.execute(ctx)
		}
		<-h.release
		return domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}, nil
	}}
	locks := &mockDatabaseLocker{
		acquire:   func(context.Context, int64, string, string) (int64, bool, error) { return 1, true, nil },
		heartbeat: func(context.Context, int64, string, int64) (bool, error) { return true, nil },
		release:   func(context.Context, int64, string, int64) error { return nil },
	}
	consumer := &mockJobConsumer{receive: func(ctx context.Context) (JobDelivery, error) {
		select {
		case d := <-h.inbox:
			return d, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	h.worker = New(Dependencies{
		Consumer: consumer, Jobs: jobs, Executor: exec, Locks: locks,
		Authorizer: &mockExecutionAuthorizer{authorize: func(context.Context, domain.ExecutionRequest) error { return nil }},
		Fence:      func(string, int64) JobStore { return jobs },
	}, "w1", quietLog())
	h.worker.heartbeat = time.Hour
	return h
}

func (h *loopHarness) publish(id string, databaseID int64, mode domain.ExecutionMode) {
	msg := message()
	msg.JobID, msg.ExecutionID, msg.DatabaseID, msg.Mode = id, "exec-"+id, databaseID, mode
	h.mu.Lock()
	h.databases[id] = databaseID
	h.mu.Unlock()
	h.inbox <- &mockJobDelivery{
		message: msg,
		ack: func() error {
			h.mu.Lock()
			h.acks++
			h.mu.Unlock()
			return nil
		},
		requeue: func() error { return errors.New("unexpected requeue") },
		reject:  func() error { return errors.New("unexpected reject") },
	}
}

func (h *loopHarness) waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := done()
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunProcessesJobsInParallelUpToTheSlots(t *testing.T) {
	h := newLoopHarness()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- h.worker.Run(ctx, 2) }()
	for i := range 5 {
		h.publish(fmt.Sprintf("job-%d", i), int64(i+1), domain.ExecutionModeNormal)
	}
	h.waitFor(t, "two jobs running", func() bool { return h.running == 2 })
	time.Sleep(20 * time.Millisecond)
	h.mu.Lock()
	if h.running != 2 {
		h.mu.Unlock()
		t.Fatalf("%d jobs running with 2 slots", h.running)
	}
	h.mu.Unlock()
	close(h.release)
	h.waitFor(t, "all jobs acked", func() bool { return h.acks == 5 })
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if h.peak != 2 || len(h.final) != 5 {
		t.Fatalf("peak concurrency %d, finished %d", h.peak, len(h.final))
	}
	for id, status := range h.final {
		if status != domain.JobStatusSucceeded {
			t.Fatalf("%s = %s", id, status)
		}
	}
}

func TestRunLetsTheCurrentBatchFinishOnShutdown(t *testing.T) {
	h := newLoopHarness()
	started := make(chan struct{})
	var stopCause error
	h.execute = func(ctx context.Context) (domain.ExecutionResult, error) {
		close(started)
		<-ctx.Done()
		stopCause = context.Cause(ctx)
		time.Sleep(20 * time.Millisecond)
		return domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: errorCodeInterrupted, AppliedBatches: 3}, nil
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- h.worker.Run(ctx, 1) }()
	h.publish("job-1", 1, domain.ExecutionModeBatch)
	<-started
	shutdown := errors.New("SIGTERM")
	cancel(shutdown)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stopCause, shutdown) {
		t.Fatalf("the run saw %v, want the shutdown", stopCause)
	}
	if h.final["job-1"] != domain.JobStatusRetryWait || h.acks != 1 {
		t.Fatalf("after shutdown: job %s, acks %d; Run must return only after the job is saved and acked", h.final["job-1"], h.acks)
	}
}

func TestRunStopsOnAReceiveError(t *testing.T) {
	h := newLoopHarness()
	broken := errors.New("channel closed")
	h.worker.deps.Consumer = &mockJobConsumer{receive: func(context.Context) (JobDelivery, error) { return nil, broken }}
	if err := h.worker.Run(context.Background(), 3); !errors.Is(err, broken) {
		t.Fatalf("err = %v, want the receive error", err)
	}
}

type sweepFunc func(context.Context) (int, error)

func (f sweepFunc) SweepExpiredJobs(ctx context.Context) (int, error) { return f(ctx) }

func TestRunSweeperTicksUntilCancelled(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSweeper(ctx, sweepFunc(func(context.Context) (int, error) {
			if calls.Add(1) == 2 {
				return 0, errors.New("platform down")
			}
			return 1, nil
		}), time.Millisecond, quietLog())
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper stopped ticking after an error")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSweeper did not return after cancel")
	}
}
