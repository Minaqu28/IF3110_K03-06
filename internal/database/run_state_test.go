package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func TestExecutionAuthorizerPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	adminStore, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	submits := database.NewSubmitPostgresStore(db)
	authorizer := workflow.NewExecutionAuthorizer(admin.NewService(adminStore, nil), submits)

	var group int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ('editors') RETURNING id`).Scan(&group); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, group, f.userID)
	execSQL(t, db, `INSERT INTO group_database_access (group_id, database_id, access_level) VALUES ($1, $2, 'VIEW_EDIT')`, group, f.databaseID)

	req := approvedRequest(t, db, f)
	result, err := submitWith(submits, req, jobA, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.ExecutionID = result.Job.ExecutionID

	actor, state, err := submits.LoadRunState(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if actor.ID != f.userID || !actor.IsActive || actor.Username != "author" || len(state.Executions) != 1 ||
		state.Executions[0].Status != domain.ExecutionStatusPending {
		t.Fatalf("run state = %+v / %+v", actor, state)
	}
	if err := authorizer.AuthorizeExecution(ctx, req); err != nil {
		t.Fatalf("queued execution: %v", err)
	}

	execSQL(t, db, `UPDATE group_database_access SET access_level = 'VIEW_ONLY' WHERE group_id = $1`, group)
	if err := authorizer.AuthorizeExecution(ctx, req); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("after the grant became view-only: err = %v, want forbidden", err)
	}
	execSQL(t, db, `UPDATE group_database_access SET access_level = 'VIEW_EDIT' WHERE group_id = $1`, group)
	execSQL(t, db, `UPDATE users SET is_active = FALSE WHERE id = $1`, f.userID)
	if err := authorizer.AuthorizeExecution(ctx, req); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("after the user was deactivated: err = %v, want forbidden", err)
	}
	execSQL(t, db, `UPDATE users SET is_active = TRUE WHERE id = $1`, f.userID)

	if _, err := database.NewRevisionPostgresStore(db).CreateRevision(ctx, domain.MigrationRevision{
		MigrationID: f.migrationID, DatabaseID: f.databaseID, UpSQL: "SELECT 2", IrreversibleReason: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeExecution(ctx, req); !errors.Is(err, workflow.ErrNotLatestRevision) {
		t.Fatalf("queued run after a newer revision: err = %v, want not latest revision", err)
	}
	execSQL(t, db, `UPDATE migration_executions SET status = 'PARTIAL' WHERE id = $1`, req.ExecutionID)
	if err := authorizer.AuthorizeExecution(ctx, req); err != nil {
		t.Fatalf("resuming a started run keeps its revision: %v", err)
	}
	execSQL(t, db, `UPDATE migration_executions SET status = 'SUCCEEDED' WHERE id = $1`, req.ExecutionID)
	if err := authorizer.AuthorizeExecution(ctx, req); !errors.Is(err, workflow.ErrActionNotAllowed) {
		t.Fatalf("finished execution: err = %v, want action not allowed", err)
	}

	gone := req
	gone.ActorID += 1000
	if err := authorizer.AuthorizeExecution(ctx, gone); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("deleted user: err = %v, want forbidden", err)
	}
}
