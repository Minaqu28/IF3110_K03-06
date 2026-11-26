package database_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestJobOutcomeIsMirroredIntoTheExecutionPostgres(t *testing.T) {
	progress := func(n int) *domain.BatchProgress {
		return &domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: n, AffectedRows: int64(n * 10)}
	}
	cases := []struct {
		name     string
		action   string
		job      domain.Job
		want     string
		finished bool
	}{
		{"success", "APPLY", domain.Job{Status: domain.JobStatusSucceeded}, "SUCCEEDED", true},
		{"failure", "APPLY", domain.Job{Status: domain.JobStatusFailed, ErrorCode: "42P01", ErrorMessage: "missing"}, "FAILED", true},
		{"failure after batches", "APPLY", domain.Job{Status: domain.JobStatusFailed, ErrorCode: "22012", Progress: progress(6)}, "PARTIAL", true},
		{"revert failure after batches", "ROLLBACK", domain.Job{Status: domain.JobStatusFailed, ErrorCode: "22012", Progress: progress(3)}, "PARTIALLY_REVERTED", true},
		{"retry before any batch", "APPLY", domain.Job{Status: domain.JobStatusRetryWait, ErrorCode: "TRANSIENT_ERROR"}, "RUNNING", false},
		{"retry after batches", "APPLY", domain.Job{Status: domain.JobStatusRetryWait, ErrorCode: "40P01", Progress: progress(2)}, "PARTIAL", false},
		{"uncertain", "APPLY", domain.Job{Status: domain.JobStatusNeedsReconciliation, ErrorCode: "COMMIT_UNCERTAIN"}, "UNKNOWN", false},
		{"cancelled", "APPLY", domain.Job{Status: domain.JobStatusCancelled}, "FAILED", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := integrationDB(t)
			f := newAsyncFixture(t, db)
			ctx := context.Background()
			revision := f.insertRevision(t, db, 1, true)
			review := f.insertReview(t, db, revision)
			var target *string
			if c.action == "ROLLBACK" {
				applied, err := f.insertExecution(db, revision, review, "APPLY", "SUCCEEDED", nil)
				if err != nil {
					t.Fatal(err)
				}
				target = &applied
			}
			execution, err := f.insertExecution(db, revision, review, c.action, "PENDING", target)
			if err != nil {
				t.Fatal(err)
			}
			store := database.NewJobPostgresStore(db)
			if err := store.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: execution, DatabaseID: f.databaseID}); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusRunning, AttemptCount: 1}); err != nil {
				t.Fatal(err)
			}
			c.job.ID, c.job.AttemptCount = jobA, 1
			if err := store.SaveJob(ctx, c.job); err != nil {
				t.Fatal(err)
			}
			var status string
			var code, message sql.NullString
			var finished bool
			if err := db.QueryRowContext(ctx, `
				SELECT status, error_code, error_message, finished_at IS NOT NULL FROM migration_executions WHERE id = $1`,
				execution).Scan(&status, &code, &message, &finished); err != nil {
				t.Fatal(err)
			}
			if status != c.want || finished != c.finished || code.String != c.job.ErrorCode || message.String != c.job.ErrorMessage {
				t.Fatalf("execution = %s (code %q, finished %v), want %s (finished %v)", status, code.String, finished, c.want, c.finished)
			}
		})
	}
}
