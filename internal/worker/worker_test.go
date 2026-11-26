package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func testMessage() domain.MigrationJobMessage {
	return domain.MigrationJobMessage{
		SchemaVersion:     domain.MigrationJobSchemaVersion,
		JobID:             "job-1",
		ExecutionID:       "exec-1",
		DatabaseID:        10,
		MigrationID:       2,
		RevisionID:        3,
		ApprovalRequestID: 4,
		ActorID:           5,
		Action:            domain.MigrationActionApply,
		Mode:              domain.ExecutionModeNormal,
	}
}

func TestWorkerProcessOneSuccess(t *testing.T) {
	var saved []domain.Job
	acked := false
	released := false

	w := NewWorker(
		&mockJobConsumer{},
		&mockJobStateStore{
			getJob: func(context.Context, string) (domain.Job, error) {
				return domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 10, Status: domain.JobStatusQueued}, nil
			},
			saveJob: func(_ context.Context, j domain.Job) error {
				saved = append(saved, j)
				return nil
			},
		},
		&mockDatabaseLocker{
			acquire: func(context.Context, int64, string, string) (int64, bool, error) { return 7, true, nil },
			heartbeat: func(context.Context, int64, string, int64) (bool, error) {
				return true, nil
			},
			release: func(context.Context, int64, string, int64) error {
				released = true
				return nil
			},
		},
		&mockExecutionAuthorizer{
			authorize: func(context.Context, domain.ExecutionRequest) error { return nil },
		},
		&mockMigrationExecutor{
			execute: func(context.Context, domain.ExecutionRequest) (domain.ExecutionResult, error) {
				return domain.ExecutionResult{ExecutionID: "exec-1", Status: domain.ExecutionStatusSucceeded}, nil
			},
		},
		Config{Owner: "w1", HeartbeatInterval: 50 * time.Millisecond, RetryDelay: 5 * time.Millisecond},
	)

	w.ProcessOne(context.Background(), &mockJobDelivery{
		message: testMessage(),
		ack: func() error {
			acked = true
			return nil
		},
	})

	if !acked || !released {
		t.Fatalf("acked=%v released=%v, want both true", acked, released)
	}
	if len(saved) != 2 || saved[0].Status != domain.JobStatusRunning || saved[1].Status != domain.JobStatusSucceeded {
		t.Fatalf("unexpected saved transitions: %+v", saved)
	}
}

func TestWorkerProcessOneTransientErrorRequeues(t *testing.T) {
	var lastSaved domain.Job
	requeued := false

	w := NewWorker(
		&mockJobConsumer{},
		&mockJobStateStore{
			getJob: func(context.Context, string) (domain.Job, error) {
				return domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 10, Status: domain.JobStatusQueued}, nil
			},
			saveJob: func(_ context.Context, j domain.Job) error {
				lastSaved = j
				return nil
			},
		},
		&mockDatabaseLocker{
			acquire: func(context.Context, int64, string, string) (int64, bool, error) { return 1, true, nil },
			release: func(context.Context, int64, string, int64) error { return nil },
		},
		&mockExecutionAuthorizer{authorize: func(context.Context, domain.ExecutionRequest) error { return nil }},
		&mockMigrationExecutor{
			execute: func(context.Context, domain.ExecutionRequest) (domain.ExecutionResult, error) {
				return domain.ExecutionResult{}, context.DeadlineExceeded
			},
		},
		Config{Owner: "w1", HeartbeatInterval: time.Second, RetryDelay: time.Millisecond, MaxAttempts: 3},
	)

	w.ProcessOne(context.Background(), &mockJobDelivery{
		message: testMessage(),
		requeue: func() error {
			requeued = true
			return nil
		},
	})

	if !requeued || lastSaved.Status != domain.JobStatusRetryWait {
		t.Fatalf("requeued=%v status=%s, want true / RETRY_WAIT", requeued, lastSaved.Status)
	}
}

func TestWorkerProcessOnePermanentAuthFailureRejects(t *testing.T) {
	var lastSaved domain.Job
	rejected := false

	w := NewWorker(
		&mockJobConsumer{},
		&mockJobStateStore{
			getJob: func(context.Context, string) (domain.Job, error) {
				return domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 10, Status: domain.JobStatusQueued}, nil
			},
			saveJob: func(_ context.Context, j domain.Job) error {
				lastSaved = j
				return nil
			},
		},
		&mockDatabaseLocker{
			acquire: func(context.Context, int64, string, string) (int64, bool, error) { return 1, true, nil },
			release: func(context.Context, int64, string, int64) error { return nil },
		},
		&mockExecutionAuthorizer{
			authorize: func(context.Context, domain.ExecutionRequest) error {
				return errors.Join(admin.ErrConflict, errors.New("not approved"))
			},
		},
		&mockMigrationExecutor{},
		Config{Owner: "w1", HeartbeatInterval: time.Second, RetryDelay: time.Millisecond},
	)

	w.ProcessOne(context.Background(), &mockJobDelivery{
		message: testMessage(),
		reject: func() error {
			rejected = true
			return nil
		},
	})

	if !rejected || lastSaved.Status != domain.JobStatusFailed {
		t.Fatalf("rejected=%v status=%s, want true / FAILED", rejected, lastSaved.Status)
	}
}
