package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

type settled struct {
	acks, requeues, rejects int
}

func (s *settled) total() int { return s.acks + s.requeues + s.rejects }

func delivery(msg domain.MigrationJobMessage, s *settled) *mockJobDelivery {
	return &mockJobDelivery{
		message: msg,
		ack:     func() error { s.acks++; return nil },
		requeue: func() error { s.requeues++; return nil },
		reject:  func() error { s.rejects++; return nil },
	}
}

func message() domain.MigrationJobMessage {
	return domain.MigrationJobMessage{
		SchemaVersion: domain.MigrationJobSchemaVersion, JobID: "job-1", ExecutionID: "exec-1", DatabaseID: 4,
		MigrationID: 9, RevisionID: 90, ApprovalRequestID: 3, ActorID: 2, Action: domain.MigrationActionApply, Mode: domain.ExecutionModeNormal,
	}
}

func queuedJob() domain.Job {
	return domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 4, Status: domain.JobStatusQueued}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func admitWith(t *testing.T, msg domain.MigrationJobMessage, getJob func(context.Context, string) (domain.Job, error)) (domain.Job, bool, *settled) {
	t.Helper()
	s := &settled{}
	w := New(Dependencies{Jobs: &delayingJobs{mockJobStateStore: &mockJobStateStore{getJob: getJob}}}, "w1", quietLog())
	job, ok := w.admit(context.Background(), w.log, delivery(msg, s))
	return job, ok, s
}

func TestAdmitRunsAnOpenJob(t *testing.T) {
	for _, status := range []domain.JobStatus{domain.JobStatusQueued, domain.JobStatusRetryWait, domain.JobStatusNeedsReconciliation, domain.JobStatusRunning} {
		job, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			j.Status = status
			return j, nil
		})
		if !ok || job.ID != "job-1" || s.total() != 0 {
			t.Fatalf("%s: admitted = %v, settled %+v; an open job runs and is not settled yet", status, ok, s)
		}
	}
}

func TestAdmitAcksADuplicateOfAFinishedJob(t *testing.T) {
	for _, status := range []domain.JobStatus{domain.JobStatusSucceeded, domain.JobStatusFailed, domain.JobStatusCancelled} {
		_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			j.Status = status
			return j, nil
		})
		if ok || s.acks != 1 || s.total() != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v; want a single ack", status, ok, s)
		}
	}
}

func TestAdmitRejectsBadMessages(t *testing.T) {
	cases := map[string]func(*domain.MigrationJobMessage){
		"old schema":     func(m *domain.MigrationJobMessage) { m.SchemaVersion = 0 },
		"future schema":  func(m *domain.MigrationJobMessage) { m.SchemaVersion = domain.MigrationJobSchemaVersion + 1 },
		"no job id":      func(m *domain.MigrationJobMessage) { m.JobID = "" },
		"no execution":   func(m *domain.MigrationJobMessage) { m.ExecutionID = "" },
		"no database":    func(m *domain.MigrationJobMessage) { m.DatabaseID = 0 },
		"no revision":    func(m *domain.MigrationJobMessage) { m.RevisionID = 0 },
		"unknown action": func(m *domain.MigrationJobMessage) { m.Action = "DROP" },
		"unknown mode":   func(m *domain.MigrationJobMessage) { m.Mode = "FAST" },
	}
	for name, mutate := range cases {
		msg := message()
		mutate(&msg)
		_, ok, s := admitWith(t, msg, nil)
		if ok || s.rejects != 1 || s.total() != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v; want a single reject", name, ok, s)
		}
	}
}

func TestAdmitRejectsAMessageThatDoesNotMatchItsJob(t *testing.T) {
	for name, mutate := range map[string]func(*domain.Job){
		"other execution": func(j *domain.Job) { j.ExecutionID = "exec-2" },
		"other database":  func(j *domain.Job) { j.DatabaseID = 5 },
	} {
		_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			mutate(&j)
			return j, nil
		})
		if ok || s.rejects != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v", name, ok, s)
		}
	}
	_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) { return domain.Job{}, admin.ErrNotFound })
	if ok || s.rejects != 1 {
		t.Fatalf("unknown job: admitted = %v, settled %+v", ok, s)
	}
}

func TestAdmitRequeuesWhenTheJobCannotBeLoaded(t *testing.T) {
	_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) { return domain.Job{}, errors.New("platform down") })
	if ok || s.requeues != 1 || s.total() != 1 {
		t.Fatalf("admitted = %v, settled %+v; want a single requeue", ok, s)
	}
}

type runHarness struct {
	events     []string
	saves      []domain.Job
	saveErrs   []error
	authErr    error
	result     domain.ExecutionResult
	execErr    error
	requests   []domain.ExecutionRequest
	saveCtxOK  []bool
	jobs       *delayingJobs
	locks      *mockDatabaseLocker
	acquired   bool
	acquireErr error
	held       func() (bool, error)
	fenced     []string
	worker     *Worker
}

func newRunHarness() *runHarness {
	h := &runHarness{result: domain.ExecutionResult{Status: domain.ExecutionStatusSucceeded}, acquired: true}
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
	h.locks = &mockDatabaseLocker{
		acquire: func(_ context.Context, databaseID int64, executionID, owner string) (int64, bool, error) {
			h.events = append(h.events, "lock")
			if !h.acquired || h.acquireErr != nil {
				return 0, false, h.acquireErr
			}
			return 7, true, nil
		},
		heartbeat: func(context.Context, int64, string, int64) (bool, error) {
			if h.held == nil {
				return true, nil
			}
			return h.held()
		},
		release: func(_ context.Context, _ int64, owner string, token int64) error {
			h.events = append(h.events, fmt.Sprintf("release:%s:%d", owner, token))
			return nil
		},
	}
	fence := func(owner string, token int64) JobStore {
		h.fenced = append(h.fenced, fmt.Sprintf("%s:%d", owner, token))
		return jobs
	}
	h.worker = New(Dependencies{Jobs: jobs, Authorizer: auth, Executor: exec, Locks: h.locks, Fence: fence}, "w1", quietLog())
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
	want := []string{"lock", "authorize", "save:RUNNING", "execute", "save:SUCCEEDED", "ack", "release:w1:7"}
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
	if want := []string{"lock", "authorize", "save:RUNNING", "execute", "save:SUCCEEDED", "requeue", "release:w1:7"}; !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}

	h = newRunHarness()
	h.saveErrs = []error{errors.New("platform down")}
	h.handle(context.Background(), message())
	if want := []string{"lock", "authorize", "save:RUNNING", "requeue", "release:w1:7"}; !slices.Equal(h.events, want) {
		t.Fatalf("unsaved start: events = %v, want %v", h.events, want)
	}

	h = newRunHarness()
	h.saveErrs = []error{admin.ErrConflict}
	h.handle(context.Background(), message())
	if want := []string{"lock", "authorize", "save:RUNNING", "ack", "release:w1:7"}; !slices.Equal(h.events, want) {
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
		if want := []string{"lock", "authorize", "save:FAILED", "ack", "release:w1:7"}; !slices.Equal(h.events, want) {
			t.Fatalf("%v: events = %v, want %v", c.err, h.events, want)
		}
		if h.saves[0].ErrorCode != c.code || h.saves[0].ErrorMessage == "" {
			t.Fatalf("%v: saved %+v", c.err, h.saves[0])
		}
	}
	h := newRunHarness()
	h.authErr = errors.New("platform down")
	h.handle(context.Background(), message())
	if want := []string{"lock", "authorize", "requeue", "release:w1:7"}; !slices.Equal(h.events, want) {
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
	if !ctxOK || retried.Status != domain.JobStatusRetryWait || retried.Progress == nil || retried.Progress.Completed != 2 || h.events[len(h.events)-2] != "ack" {
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
	if want := []string{"lock", "authorize", "save:RUNNING", "execute", "saveAfter:RETRY_WAIT", "ack", "release:w1:7"}; !equalEvents(h.events, want) {
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
	if last := h.saves[len(h.saves)-1]; last.Status != domain.JobStatusFailed || last.ErrorCode != ErrorCodeTransient || h.events[len(h.events)-2] != "ack" {
		t.Fatalf("final save = %+v, events = %v", last, h.events)
	}
}

var errUnexpectedWorkerMockCall = errors.New("unexpected worker mock call")

type mockExecutionAuthorizer struct {
	authorize func(context.Context, domain.ExecutionRequest) error
}

var _ ExecutionAuthorizer = (*mockExecutionAuthorizer)(nil)

func (m *mockExecutionAuthorizer) AuthorizeExecution(ctx context.Context, request domain.ExecutionRequest) error {
	if m.authorize == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.authorize(ctx, request)
}

type mockMigrationExecutor struct {
	execute func(context.Context, domain.ExecutionRequest) (domain.ExecutionResult, error)
}

var _ MigrationExecutor = (*mockMigrationExecutor)(nil)

func (m *mockMigrationExecutor) Execute(ctx context.Context, request domain.ExecutionRequest) (domain.ExecutionResult, error) {
	if m.execute == nil {
		return domain.ExecutionResult{}, errUnexpectedWorkerMockCall
	}
	return m.execute(ctx, request)
}

type mockJobStateStore struct {
	getJob  func(context.Context, string) (domain.Job, error)
	saveJob func(context.Context, domain.Job) error
}

var _ JobStateStore = (*mockJobStateStore)(nil)

func (m *mockJobStateStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	if m.getJob == nil {
		return domain.Job{}, errUnexpectedWorkerMockCall
	}
	return m.getJob(ctx, jobID)
}

func (m *mockJobStateStore) SaveJob(ctx context.Context, job domain.Job) error {
	if m.saveJob == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.saveJob(ctx, job)
}

type mockJobDelivery struct {
	message     domain.MigrationJobMessage
	redelivered bool
	ack         func() error
	requeue     func() error
	reject      func() error
}

var _ JobDelivery = (*mockJobDelivery)(nil)

func (m *mockJobDelivery) Message() domain.MigrationJobMessage { return m.message }
func (m *mockJobDelivery) Redelivered() bool                   { return m.redelivered }

func (m *mockJobDelivery) Ack() error {
	if m.ack == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.ack()
}

func (m *mockJobDelivery) Requeue() error {
	if m.requeue == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.requeue()
}

func (m *mockJobDelivery) Reject() error {
	if m.reject == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.reject()
}

type mockJobConsumer struct {
	receive func(context.Context) (JobDelivery, error)
}

var _ JobConsumer = (*mockJobConsumer)(nil)

func (m *mockJobConsumer) Receive(ctx context.Context) (JobDelivery, error) {
	if m.receive == nil {
		return nil, errUnexpectedWorkerMockCall
	}
	return m.receive(ctx)
}

type mockDatabaseLocker struct {
	acquire   func(context.Context, int64, string, string) (int64, bool, error)
	heartbeat func(context.Context, int64, string, int64) (bool, error)
	release   func(context.Context, int64, string, int64) error
}

var _ DatabaseLocker = (*mockDatabaseLocker)(nil)

func (m *mockDatabaseLocker) Acquire(ctx context.Context, databaseID int64, executionID, owner string) (int64, bool, error) {
	if m.acquire == nil {
		return 0, false, errUnexpectedWorkerMockCall
	}
	return m.acquire(ctx, databaseID, executionID, owner)
}

func (m *mockDatabaseLocker) Heartbeat(ctx context.Context, databaseID int64, owner string, token int64) (bool, error) {
	if m.heartbeat == nil {
		return false, errUnexpectedWorkerMockCall
	}
	return m.heartbeat(ctx, databaseID, owner, token)
}

func (m *mockDatabaseLocker) Release(ctx context.Context, databaseID int64, owner string, token int64) error {
	if m.release == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.release(ctx, databaseID, owner, token)
}
