package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

type runHarness struct {
	events    []string
	saves     []domain.Job
	saveErrs  []error
	authErr   error
	result    domain.ExecutionResult
	execErr   error
	requests  []domain.ExecutionRequest
	saveCtxOK []bool
	jobs      *delayingJobs
	worker    *Worker
}

func newRunHarness() *runHarness {
	h := &runHarness{result: domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}}
	jobs := &delayingJobs{mockJobStateStore: &mockJobStateStore{
		getJob: func(context.Context, string) (domain.Job, error) { return queuedJob(), nil },
		saveJob: func(ctx context.Context, job domain.Job) error {
			h.events = append(h.events, "save:"+string(job.Status))
			h.saves = append(h.saves, job)
			h.saveCtxOK = append(h.saveCtxOK, ctx.Err() == nil)
			if len(h.saveErrs) > 0 {
				err := h.saveErrs[0]
				h.saveErrs = h.saveErrs[1:]
				return err
			}
			return nil
		},
	}}
	h.jobs = jobs
	auth := &mockExecutionAuthorizer{authorize: func(context.Context, domain.ExecutionRequest) error {
		h.events = append(h.events, "authorize")
		return h.authErr
	}}
	exec := &mockMigrationExecutor{execute: func(_ context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
		h.events = append(h.events, "execute")
		h.requests = append(h.requests, req)
		return h.result, h.execErr
	}}
	h.worker = New(Dependencies{Jobs: jobs, Authorizer: auth, Executor: exec}, "w1", quietLog())
	return h
}

func (h *runHarness) handle(ctx context.Context, msg domain.MigrationJobMessage) {
	d := &mockJobDelivery{
		message: msg,
		ack:     func() error { h.events = append(h.events, "ack"); return nil },
		requeue: func() error { h.events = append(h.events, "requeue"); return nil },
		reject:  func() error { h.events = append(h.events, "reject"); return nil },
	}
	h.worker.handle(ctx, d)
}

func TestHandleSavesBeforeItAcks(t *testing.T) {
	h := newRunHarness()
	msg := message()
	target := "exec-0"
	msg.TargetExecutionID = &target
	h.handle(context.Background(), msg)
	want := []string{"authorize", "save:RUNNING", "execute", "save:SUCCEEDED", "ack"}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
	if h.saves[0].AttemptCount != 1 || h.saves[1].AttemptCount != 1 {
		t.Fatalf("attempts = %d, %d", h.saves[0].AttemptCount, h.saves[1].AttemptCount)
	}
	req := h.requests[0]
	if req.JobID != msg.JobID || req.ExecutionID != msg.ExecutionID || req.RevisionID != msg.RevisionID || req.ActorID != msg.ActorID ||
		req.ApprovalRequestID != msg.ApprovalRequestID || req.TargetExecutionID != msg.TargetExecutionID {
		t.Fatalf("request = %+v from %+v", req, msg)
	}
}

func TestHandleNeverAcksAnUnsavedResult(t *testing.T) {
	h := newRunHarness()
	h.saveErrs = []error{nil, errors.New("platform down")}
	h.handle(context.Background(), message())
	if want := []string{"authorize", "save:RUNNING", "execute", "save:SUCCEEDED", "requeue"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}

	h = newRunHarness()
	h.saveErrs = []error{errors.New("platform down")}
	h.handle(context.Background(), message())
	if want := []string{"authorize", "save:RUNNING", "requeue"}; !slices.Equal(h.events, want) {
		t.Fatalf("unsaved start: events = %v, want %v", h.events, want)
	}

	h = newRunHarness()
	h.saveErrs = []error{admin.ErrConflict}
	h.handle(context.Background(), message())
	if want := []string{"authorize", "save:RUNNING", "ack"}; !slices.Equal(h.events, want) {
		t.Fatalf("job finished elsewhere: events = %v, want %v", h.events, want)
	}
}

func TestHandleRecordsRefusalsWithoutExecuting(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("%w: view only", admin.ErrForbidden), ErrorCodeNotAuthorized},
		{fmt.Errorf("%w: not approved", admin.ErrConflict), ErrorCodeNotAllowed},
		{admin.ErrInvalidInput, ErrorCodeInvalidRequest},
	} {
		h := newRunHarness()
		h.authErr = c.err
		h.handle(context.Background(), message())
		if want := []string{"authorize", "save:FAILED", "ack"}; !slices.Equal(h.events, want) {
			t.Fatalf("%v: events = %v, want %v", c.err, h.events, want)
		}
		if h.saves[0].ErrorCode != c.code || h.saves[0].ErrorMessage == "" {
			t.Fatalf("%v: saved %+v", c.err, h.saves[0])
		}
	}
	h := newRunHarness()
	h.authErr = errors.New("platform down")
	h.handle(context.Background(), message())
	if want := []string{"authorize", "requeue"}; !slices.Equal(h.events, want) {
		t.Fatalf("authorizer unavailable: events = %v, want %v", h.events, want)
	}
}

func TestHandleSavesTheResultEvenAfterShutdownBegins(t *testing.T) {
	h := newRunHarness()
	ctx, cancel := context.WithCancel(context.Background())
	h.worker.deps.Executor = &mockMigrationExecutor{execute: func(context.Context, domain.ExecutionRequest) (domain.ExecutionResult, error) {
		cancel()
		return domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: errorCodeInterrupted, AppliedBatches: 2}, nil
	}}
	var retried domain.Job
	var ctxOK bool
	h.jobs.saveAfter = func(c context.Context, job domain.Job, _ time.Time) error {
		retried, ctxOK = job, c.Err() == nil
		return nil
	}
	msg := message()
	msg.Mode = domain.ExecutionModeBatch
	h.handle(ctx, msg)
	if !ctxOK || retried.Status != domain.JobStatusRetryWait || retried.Progress == nil || retried.Progress.Completed != 2 || h.events[len(h.events)-1] != "ack" {
		t.Fatalf("retry save = %+v, ctx ok = %v, events = %v", retried, ctxOK, h.events)
	}
}

func TestOutcomeMapsEveryResult(t *testing.T) {
	batch := message()
	batch.Mode = domain.ExecutionModeBatch
	rollback := batch
	rollback.Action = domain.MigrationActionRollback
	cases := []struct {
		name       string
		msg        domain.MigrationJobMessage
		result     domain.ExecutionResult
		err        error
		wantStatus domain.JobStatus
		wantCode   string
	}{
		{"success", message(), domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}, nil, domain.JobStatusSucceeded, ""},
		{"sql error", message(), domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: "42P01"}, nil, domain.JobStatusFailed, "42P01"},
		{"refused by the target", message(), domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: "LEDGER_CONFLICT"}, nil, domain.JobStatusFailed, "LEDGER_CONFLICT"},
		{"deadlock", message(), domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: "40P01"}, nil, domain.JobStatusRetryWait, "40P01"},
		{"partial on a bad row", batch, domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: "22012", AppliedBatches: 6}, nil, domain.JobStatusFailed, "22012"},
		{"partial on a lock timeout", batch, domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: "55P03", AppliedBatches: 6}, nil, domain.JobStatusRetryWait, "55P03"},
		{"interrupted", batch, domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: errorCodeInterrupted, AppliedBatches: 2}, nil, domain.JobStatusRetryWait, errorCodeInterrupted},
		{"partially reverted", rollback, domain.ExecutionResult{Status: domain.ExecutionStatusPartiallyReverted, ErrorCode: "22012", AppliedBatches: 3}, nil, domain.JobStatusFailed, "22012"},
		{"uncertain result", message(), domain.ExecutionResult{Status: domain.ExecutionStatusUnknown, ErrorCode: "COMMIT_UNCERTAIN"}, nil, domain.JobStatusNeedsReconciliation, "COMMIT_UNCERTAIN"},
		{"transient error", message(), domain.ExecutionResult{}, migrationengine.ErrTargetBusy, domain.JobStatusRetryWait, ErrorCodeTransient},
		{"uncertain error", message(), domain.ExecutionResult{}, fmt.Errorf("%w: eof", migrationengine.ErrCommitUncertain), domain.JobStatusNeedsReconciliation, ErrorCodeCommitUncertain},
		{"permanent error", message(), domain.ExecutionResult{}, admin.ErrNotFound, domain.JobStatusFailed, ErrorCodeTarget},
		{"unexpected status", message(), domain.ExecutionResult{Status: domain.ExecutionStatusRunning}, nil, domain.JobStatusFailed, ErrorCodeUnexpectedResult},
	}
	for _, c := range cases {
		job := outcome(queuedJob(), c.msg, c.result, c.err)
		if job.Status != c.wantStatus || job.ErrorCode != c.wantCode {
			t.Errorf("%s: job = %s / %s, want %s / %s", c.name, job.Status, job.ErrorCode, c.wantStatus, c.wantCode)
		}
		if c.err != nil && job.ErrorMessage == "" {
			t.Errorf("%s: no error message", c.name)
		}
	}
}

func TestOutcomeCarriesBatchProgress(t *testing.T) {
	batch := message()
	batch.Mode = domain.ExecutionModeBatch
	previous := queuedJob()
	total := 10
	previous.Progress = &domain.BatchProgress{Total: &total, LastCursor: "6000"}
	job := outcome(previous, batch, domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: "22012", AppliedBatches: 6, AffectedRows: 6000}, nil)
	if p := job.Progress; p == nil || p.Status != domain.BatchStatusCommitted || p.Completed != 6 || p.AffectedRows != 6000 || p.LastCursor != "6000" || p.Total != &total {
		t.Fatalf("progress = %+v", job.Progress)
	}
	rollback := batch
	rollback.Action = domain.MigrationActionRollback
	if p := outcome(queuedJob(), rollback, domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded, AppliedBatches: 3}, nil).Progress; p == nil || p.Status != domain.BatchStatusCompensated {
		t.Fatalf("revert progress = %+v", p)
	}
	if p := outcome(queuedJob(), message(), domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}, nil).Progress; p != nil {
		t.Fatalf("normal mode progress = %+v, want none", p)
	}
}

func equalEvents(a, b []string) bool { return slices.Equal(a, b) }
