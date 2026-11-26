package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestExecutionStorePostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewExecutionPostgresStore(db)
	revision := f.insertRevision(t, db, 1, true)
	review := f.insertReview(t, db, revision)
	base := domain.ExecutionRequest{
		JobID: "ignored", ExecutionID: "ignored",
		DatabaseID: f.databaseID, MigrationID: f.migrationID, RevisionID: revision, ApprovalRequestID: review,
		ActorID: f.userID, Action: domain.MigrationActionApply, Mode: domain.ExecutionModeNormal,
	}

	applied, err := store.CreateExecution(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.ExecutionID) != 36 || applied.JobID != "" || applied.RequestedAt.IsZero() {
		t.Fatalf("created = %+v", applied)
	}
	req, res, err := store.GetExecution(ctx, applied.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if req != applied {
		t.Fatalf("request = %+v, want %+v", req, applied)
	}
	if res.ExecutionID != applied.ExecutionID || res.Status != domain.ExecutionStatusPending || !res.StartedAt.IsZero() ||
		res.FinishedAt != nil || res.RevertsExecutionID != nil || res.AppliedBatches != 0 {
		t.Fatalf("result = %+v", res)
	}

	rollback := base
	rollback.Action = domain.MigrationActionRollback
	rollback.TargetExecutionID = &applied.ExecutionID
	reverted, err := store.CreateExecution(ctx, rollback)
	if err != nil {
		t.Fatal(err)
	}
	req, res, err = store.GetExecution(ctx, reverted.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if req.TargetExecutionID == nil || *req.TargetExecutionID != applied.ExecutionID ||
		res.RevertsExecutionID == nil || *res.RevertsExecutionID != applied.ExecutionID {
		t.Fatalf("rollback = %+v / %+v", req, res)
	}

	invalid := map[string]func(*domain.ExecutionRequest){
		"unknown action":          func(r *domain.ExecutionRequest) { r.Action = "RESUME" },
		"rollback without target": func(r *domain.ExecutionRequest) { r.Action = domain.MigrationActionRollback },
		"apply with target":       func(r *domain.ExecutionRequest) { r.TargetExecutionID = &applied.ExecutionID },
		"unknown mode":            func(r *domain.ExecutionRequest) { r.Mode = "FAST" },
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if _, err := store.CreateExecution(ctx, r); !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatalf("err = %v, want invalid input", err)
			}
		})
	}
	mismatched := map[string]func(*domain.ExecutionRequest){
		"other database":  func(r *domain.ExecutionRequest) { r.DatabaseID += 1000 },
		"other migration": func(r *domain.ExecutionRequest) { r.MigrationID += 1000 },
		"other review":    func(r *domain.ExecutionRequest) { r.ApprovalRequestID += 1000 },
	}
	for name, mutate := range mismatched {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if _, err := store.CreateExecution(ctx, r); !errors.Is(err, admin.ErrNotFound) {
				t.Fatalf("err = %v, want not found", err)
			}
		})
	}
	if _, _, err := store.GetExecution(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown execution: err = %v, want not found", err)
	}
	if _, _, err := store.GetExecution(ctx, "nope"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("malformed id: err = %v, want invalid input", err)
	}
}
