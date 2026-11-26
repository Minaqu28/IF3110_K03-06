package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestHistoryStorePostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewHistoryPostgresStore(db)
	revision := f.insertRevision(t, db, 1, true)
	review := f.insertReview(t, db, revision)
	first, err := f.insertExecution(db, revision, review, "APPLY", "RUNNING", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.insertExecution(db, revision, review, "APPLY", "RUNNING", nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Truncate(time.Millisecond)
	finish := start.Add(time.Second)
	appends := []domain.ExecutionAttempt{
		{ExecutionID: first, AttemptNumber: 99, Status: domain.ExecutionStatusUnknown, SQLSnapshot: "SELECT 1", StartedAt: start},
		{ExecutionID: second, Status: domain.ExecutionStatusSucceeded, SQLSnapshot: "SELECT 1", StartedAt: start.Add(time.Millisecond), FinishedAt: &finish},
		{ExecutionID: first, Status: domain.ExecutionStatusFailed, SQLSnapshot: "SELECT 1/0", ErrorCode: "22012", ErrorMessage: "division by zero", StartedAt: start.Add(2 * time.Millisecond), FinishedAt: &finish},
	}
	for _, a := range appends {
		if err := store.AppendAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListAttempts(ctx, f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("attempts = %+v, want 3", got)
	}
	wantNumbers := []struct {
		execution string
		number    int
	}{{first, 1}, {second, 1}, {first, 2}}
	for i, w := range wantNumbers {
		if got[i].ExecutionID != w.execution || got[i].AttemptNumber != w.number || got[i].AttemptID == 0 {
			t.Errorf("attempt %d = %+v, want execution %s number %d", i, got[i], w.execution, w.number)
		}
	}
	last := got[2]
	if last.Status != domain.ExecutionStatusFailed || last.ErrorCode != "22012" || last.ErrorMessage != "division by zero" ||
		last.SQLSnapshot != "SELECT 1/0" || last.FinishedAt == nil || !last.FinishedAt.Equal(finish) {
		t.Fatalf("last attempt = %+v", last)
	}
	if got[0].FinishedAt != nil || got[0].ErrorCode != "" {
		t.Fatalf("unfinished attempt = %+v", got[0])
	}

	if err := store.AppendAttempt(ctx, domain.ExecutionAttempt{ExecutionID: "00000000-0000-0000-0000-000000000000", Status: domain.ExecutionStatusFailed, SQLSnapshot: "x", StartedAt: start}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown execution: err = %v, want not found", err)
	}
	if err := store.AppendAttempt(ctx, domain.ExecutionAttempt{ExecutionID: "not-a-uuid", Status: domain.ExecutionStatusFailed, SQLSnapshot: "x", StartedAt: start}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("malformed execution id: err = %v, want invalid input", err)
	}
	if err := store.AppendAttempt(ctx, domain.ExecutionAttempt{ExecutionID: first, Status: "DONE", SQLSnapshot: "x", StartedAt: start}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("bad status: err = %v, want invalid input", err)
	}
	other, err := store.ListAttempts(ctx, f.databaseID+1000)
	if err != nil || len(other) != 0 {
		t.Fatalf("other database attempts = %+v, err = %v", other, err)
	}
}
