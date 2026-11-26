package migrationengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type mockFinisher struct {
	finish func(context.Context, domain.ExecutionResult) error
}

func (m *mockFinisher) FinishExecution(ctx context.Context, res domain.ExecutionResult) error {
	if m.finish == nil {
		return nil
	}
	return m.finish(ctx, res)
}

func TestEngineExecuteRecordsAttemptAndFinishes(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	rev := domain.MigrationRevision{
		ID:          11,
		MigrationID: 2,
		DatabaseID:  3,
		Revision:    1,
		UpSQL:       "CREATE TABLE t(id INT);",
		DownSQL:     "DROP TABLE t;",
		Reversible:  true,
	}
	var recordedAttempt domain.ExecutionAttempt
	var finishedResult domain.ExecutionResult

	eng := NewEngine(
		&mockRevisionStore{
			getRevision: func(_ context.Context, dbID, revID int64) (domain.MigrationRevision, error) {
				if dbID != 3 || revID != 11 {
					t.Fatalf("unexpected ids: %d, %d", dbID, revID)
				}
				return rev, nil
			},
		},
		&mockHistoryStore{
			appendAttempt: func(_ context.Context, a domain.ExecutionAttempt) error {
				recordedAttempt = a
				return nil
			},
		},
		&mockTargetExecutor{
			execute: func(_ context.Context, req domain.ExecutionRequest, gotRev domain.MigrationRevision) (domain.ExecutionResult, error) {
				return domain.ExecutionResult{
					ExecutionID:  req.ExecutionID,
					Status:       domain.ExecutionStatusSucceeded,
					AffectedRows: 5,
					StartedAt:    now,
					FinishedAt:   &now,
				}, nil
			},
		},
		&mockFinisher{
			finish: func(_ context.Context, res domain.ExecutionResult) error {
				finishedResult = res
				return nil
			},
		},
	)
	eng.now = func() time.Time { return now }

	res, err := eng.Execute(context.Background(), domain.ExecutionRequest{
		ExecutionID: "exec-1",
		DatabaseID:  3,
		MigrationID: 2,
		RevisionID:  11,
		Action:      domain.MigrationActionApply,
		Mode:        domain.ExecutionModeNormal,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Status != domain.ExecutionStatusSucceeded || finishedResult.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("unexpected result: %+v / %+v", res, finishedResult)
	}
	if recordedAttempt.SQLSnapshot != rev.UpSQL || recordedAttempt.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("unexpected recorded attempt: %+v", recordedAttempt)
	}
}

func TestEngineExecuteRecordsRollbackDownSQLAndTransientFailure(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	rev := domain.MigrationRevision{
		ID:          11,
		MigrationID: 2,
		DatabaseID:  3,
		Revision:    1,
		UpSQL:       "CREATE TABLE t(id INT);",
		DownSQL:     "DROP TABLE t;",
		Reversible:  true,
	}
	var recordedAttempt domain.ExecutionAttempt
	finishCalled := false

	eng := NewEngine(
		&mockRevisionStore{
			getRevision: func(context.Context, int64, int64) (domain.MigrationRevision, error) { return rev, nil },
		},
		&mockHistoryStore{
			appendAttempt: func(_ context.Context, a domain.ExecutionAttempt) error {
				recordedAttempt = a
				return nil
			},
		},
		&mockTargetExecutor{
			execute: func(context.Context, domain.ExecutionRequest, domain.MigrationRevision) (domain.ExecutionResult, error) {
				return domain.ExecutionResult{}, errors.New("connection reset")
			},
		},
		&mockFinisher{
			finish: func(context.Context, domain.ExecutionResult) error {
				finishCalled = true
				return nil
			},
		},
	)
	eng.now = func() time.Time { return now }

	_, err := eng.Execute(context.Background(), domain.ExecutionRequest{
		ExecutionID: "exec-2",
		DatabaseID:  3,
		MigrationID: 2,
		RevisionID:  11,
		Action:      domain.MigrationActionRollback,
		Mode:        domain.ExecutionModeNormal,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if finishCalled {
		t.Fatal("finisher should not be called on transient target error")
	}
	if recordedAttempt.SQLSnapshot != rev.DownSQL || recordedAttempt.Status != domain.ExecutionStatusFailed {
		t.Fatalf("unexpected attempt: %+v", recordedAttempt)
	}
}

func TestEngineExecuteRejectsInvalidInput(t *testing.T) {
	eng := NewEngine(&mockRevisionStore{}, &mockHistoryStore{}, &mockTargetExecutor{}, nil)
	if _, err := eng.Execute(context.Background(), domain.ExecutionRequest{}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}
