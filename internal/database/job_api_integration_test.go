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

func TestJobQueryAndActionsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	adminStore, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	access := admin.NewService(adminStore, nil)
	var group int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ('editors') RETURNING id`).Scan(&group); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, group, f.userID)
	execSQL(t, db, `INSERT INTO group_database_access (group_id, database_id, access_level) VALUES ($1, $2, 'VIEW_EDIT')`, group, f.databaseID)

	submits := database.NewSubmitPostgresStore(db)
	result, err := submitWith(submits, approvedRequest(t, db, f), jobA, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := database.NewJobPostgresStore(db)
	author := domain.User{ID: f.userID, Username: "author", IsActive: true}
	query := workflow.NewJobQueryService(access, jobs)
	advisor := workflow.NewActionAdvisor(access, database.NewExecutionPostgresStore(db), submits)

	job, err := query.GetJob(ctx, author, jobA)
	if err != nil || job.ExecutionID != result.Job.ExecutionID {
		t.Fatalf("job = %+v, err = %v", job, err)
	}
	if actions, err := advisor.Actions(ctx, author, job); err != nil || actions != (workflow.JobActions{}) {
		t.Fatalf("pending execution actions = %+v, err = %v; nothing to resume or revert yet", actions, err)
	}
	execSQL(t, db, `UPDATE migration_executions SET status = 'PARTIAL' WHERE id = $1`, job.ExecutionID)
	execSQL(t, db, `UPDATE migration_jobs SET status = 'FAILED' WHERE id = $1`, jobA)
	if actions, err := advisor.Actions(ctx, author, job); err != nil || actions != (workflow.JobActions{CanResume: true}) {
		t.Fatalf("partial execution actions = %+v, err = %v; the fixture revision is irreversible", actions, err)
	}

	execSQL(t, db, `UPDATE group_database_access SET access_level = 'VIEW_ONLY' WHERE group_id = $1`, group)
	if _, err := query.GetJob(ctx, author, jobA); err != nil {
		t.Fatalf("view-only users may poll: %v", err)
	}
	if actions, _ := advisor.Actions(ctx, author, job); actions != (workflow.JobActions{}) {
		t.Fatalf("view-only actions = %+v", actions)
	}
	execSQL(t, db, `DELETE FROM group_database_access WHERE group_id = $1`, group)
	if _, err := query.GetJob(ctx, author, jobA); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("no access: err = %v, want not found", err)
	}
}
