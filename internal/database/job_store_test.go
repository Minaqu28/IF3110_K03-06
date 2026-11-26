package database_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	jobA = "11111111-1111-1111-1111-111111111111"
	jobB = "22222222-2222-2222-2222-222222222222"
	jobC = "33333333-3333-3333-3333-333333333333"
)

type executionMirror struct {
	status  string
	started bool
	batches int
	rows    int64
	cursor  sql.NullString
}

func readExecutionMirror(t *testing.T, db *sql.DB, id string) executionMirror {
	t.Helper()
	var m executionMirror
	if err := db.QueryRowContext(context.Background(), `
		SELECT status, started_at IS NOT NULL, applied_batches, affected_rows, last_cursor
		FROM migration_executions WHERE id = $1`, id).Scan(&m.status, &m.started, &m.batches, &m.rows, &m.cursor); err != nil {
		t.Fatal(err)
	}
	return m
}

func newJobFixture(t *testing.T, db *sql.DB) (asyncFixture, string) {
	t.Helper()
	f := newAsyncFixture(t, db)
	revision := f.insertRevision(t, db, 1, true)
	execution, err := f.insertExecution(db, revision, f.insertReview(t, db, revision), "APPLY", "PENDING", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, execution
}

func TestJobStoreCreateAndGetPostgres(t *testing.T) {
	db := integrationDB(t)
	f, execution := newJobFixture(t, db)
	ctx := context.Background()
	store := database.NewJobPostgresStore(db)

	if err := store.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: execution, DatabaseID: f.databaseID, Status: domain.JobStatusFailed, AttemptCount: 7}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetJob(ctx, jobA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != jobA || got.ExecutionID != execution || got.DatabaseID != f.databaseID || got.Status != domain.JobStatusQueued ||
		got.AttemptCount != 0 || got.Progress != nil || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("job = %+v", got)
	}

	total := 40
	progress := &domain.BatchProgress{Status: domain.BatchStatusPending, Total: &total, LastCursor: "0"}
	if err := store.CreateJob(ctx, domain.Job{ID: jobB, ExecutionID: execution, DatabaseID: f.databaseID, Progress: progress}); err != nil {
		t.Fatal(err)
	}
	withProgress, err := store.GetJob(ctx, jobB)
	if err != nil || !reflect.DeepEqual(withProgress.Progress, progress) {
		t.Fatalf("progress = %+v, err = %v, want %+v", withProgress.Progress, err, progress)
	}

	cases := map[string]struct {
		job  domain.Job
		want error
	}{
		"duplicate id":   {domain.Job{ID: jobA, ExecutionID: execution, DatabaseID: f.databaseID}, admin.ErrConflict},
		"empty id":       {domain.Job{ExecutionID: execution, DatabaseID: f.databaseID}, admin.ErrInvalidInput},
		"malformed id":   {domain.Job{ID: "job-1", ExecutionID: execution, DatabaseID: f.databaseID}, admin.ErrInvalidInput},
		"other database": {domain.Job{ID: jobC, ExecutionID: execution, DatabaseID: f.databaseID + 1000}, admin.ErrNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := store.CreateJob(ctx, c.job); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := store.GetJob(ctx, jobC); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown job: err = %v, want not found", err)
	}
}

func TestJobStoreSavePostgres(t *testing.T) {
	db := integrationDB(t)
	f, execution := newJobFixture(t, db)
	ctx := context.Background()
	store := database.NewJobPostgresStore(db)
	if err := store.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: execution, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}

	running := domain.Job{ID: jobA, Status: domain.JobStatusRunning, AttemptCount: 1,
		Progress: &domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: 3, LastCursor: "3000", AffectedRows: 2990}}
	if err := store.SaveJob(ctx, running); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetJob(ctx, jobA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.JobStatusRunning || got.AttemptCount != 1 || !reflect.DeepEqual(got.Progress, running.Progress) ||
		got.ExecutionID != execution || !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("saved job = %+v", got)
	}
	want := executionMirror{status: "RUNNING", started: true, batches: 3, rows: 2990, cursor: sql.NullString{String: "3000", Valid: true}}
	if m := readExecutionMirror(t, db, execution); m != want {
		t.Fatalf("execution mirror = %+v, want %+v", m, want)
	}

	if err := store.SaveJob(ctx, domain.Job{ID: jobA, Status: "DONE", AttemptCount: 9}); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("bad status: err = %v, want invalid input", err)
	}
	if again, _ := store.GetJob(ctx, jobA); again.AttemptCount != 1 {
		t.Fatalf("rejected save changed the job: %+v", again)
	}

	uncertain := domain.Job{ID: jobA, Status: domain.JobStatusNeedsReconciliation, AttemptCount: 1, ErrorCode: "08006", ErrorMessage: "connection lost"}
	if err := store.SaveJob(ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetJob(ctx, jobA); got.Progress != nil || got.ErrorCode != "08006" || got.ErrorMessage != "connection lost" {
		t.Fatalf("uncertain job = %+v", got)
	}
	want.status = "UNKNOWN"
	if m := readExecutionMirror(t, db, execution); m != want {
		t.Fatalf("execution mirror after reconciliation = %+v, want %+v", m, want)
	}

	if err := store.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusSucceeded, AttemptCount: 2}); err != nil {
		t.Fatal(err)
	}
	want.status = "SUCCEEDED"
	if m := readExecutionMirror(t, db, execution); m != want {
		t.Fatalf("execution after the job succeeded = %+v, want %+v", m, want)
	}
	if err := store.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusRunning, AttemptCount: 3}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("saving a terminal job: err = %v, want conflict", err)
	}
	if err := store.SaveJob(ctx, domain.Job{ID: jobC, Status: domain.JobStatusRunning}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown job: err = %v, want not found", err)
	}

	if err := store.CreateJob(ctx, domain.Job{ID: jobB, ExecutionID: execution, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJob(ctx, domain.Job{ID: jobB, Status: domain.JobStatusRunning, Progress: &domain.BatchProgress{Completed: 99}}); err != nil {
		t.Fatal(err)
	}
	if m := readExecutionMirror(t, db, execution); m != want {
		t.Fatalf("finished execution was overwritten: %+v, want %+v", m, want)
	}
}
