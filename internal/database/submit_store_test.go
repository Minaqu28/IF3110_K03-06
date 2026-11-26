package database_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func approvedRequest(t *testing.T, db *sql.DB, f asyncFixture) domain.ExecutionRequest {
	t.Helper()
	revision := f.insertRevision(t, db, 1, true)
	return domain.ExecutionRequest{
		DatabaseID: f.databaseID, MigrationID: f.migrationID, RevisionID: revision,
		ApprovalRequestID: f.insertReview(t, db, revision), ActorID: f.userID,
		Action: domain.MigrationActionApply, Mode: domain.ExecutionModeNormal,
	}
}

func submitWith(store *database.SubmitPostgresStore, req domain.ExecutionRequest, jobID string, scope *workflow.BatchScope) (workflow.SubmitResult, error) {
	return store.SubmitExecution(context.Background(), workflow.Submission{Request: req, JobID: jobID, Scope: scope},
		func(s workflow.SubmitState) error { return workflow.CanSubmit(s, req) })
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubmitExecutionPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewSubmitPostgresStore(db)
	req := approvedRequest(t, db, f)

	result, err := submitWith(store, req, jobA, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, msg := result.Job, result.Message
	if !result.Created || job.ID != jobA || job.Status != domain.JobStatusQueued || job.DatabaseID != f.databaseID || job.ExecutionID == "" {
		t.Fatalf("result = %+v", result)
	}
	if msg.SchemaVersion != domain.MigrationJobSchemaVersion || msg.JobID != jobA || msg.ExecutionID != job.ExecutionID ||
		msg.RevisionID != req.RevisionID || msg.ActorID != f.userID || msg.Action != domain.MigrationActionApply || msg.RequestedAt.IsZero() {
		t.Fatalf("message = %+v", msg)
	}
	execReq, execRes, err := database.NewExecutionPostgresStore(db).GetExecution(ctx, job.ExecutionID)
	if err != nil || execRes.Status != domain.ExecutionStatusPending || execReq.RequestedAt != msg.RequestedAt {
		t.Fatalf("execution = %+v / %+v, err = %v", execReq, execRes, err)
	}
	if jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("the store marked the job published; that is the service's job after Publish")
	}

	if _, err := submitWith(store, req, jobB, nil); !errors.Is(err, workflow.ErrDatabaseHeld) {
		t.Fatalf("second submit: err = %v, want database held", err)
	}
	if countRows(t, db, "migration_executions") != 1 || countRows(t, db, "migration_jobs") != 1 {
		t.Fatal("a refused submit wrote rows")
	}

	replay, err := submitWith(store, req, jobA, nil)
	if err != nil || replay.Created || replay.Job.ID != jobA || replay.Job.ExecutionID != job.ExecutionID {
		t.Fatalf("replay = %+v, err = %v", replay, err)
	}
	other := req
	other.Action = domain.MigrationActionForward
	if _, err := submitWith(store, other, jobA, nil); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("job id reused for another request: err = %v, want conflict", err)
	}
	stranger := req
	stranger.ActorID = insertUser(t, db, "stranger", "stranger-long-password", false).ID
	if _, err := submitWith(store, stranger, jobA, nil); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("job id reused by another user: err = %v, want conflict", err)
	}
	if _, err := submitWith(store, req, "not-a-uuid", nil); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("malformed job id: err = %v, want invalid input", err)
	}

	execSQL(t, db, `UPDATE migration_executions SET status = 'PARTIAL' WHERE id = $1`, job.ExecutionID)
	resume := req
	resume.ExecutionID = job.ExecutionID
	if _, err := submitWith(store, resume, jobC, nil); !errors.Is(err, workflow.ErrDatabaseHeld) {
		t.Fatalf("resume while the first job is still open: err = %v, want database held", err)
	}
	execSQL(t, db, `UPDATE migration_jobs SET status = 'FAILED' WHERE id = $1`, jobA)
	resumed, err := submitWith(store, resume, jobC, nil)
	if err != nil || !resumed.Created || resumed.Job.ExecutionID != job.ExecutionID || resumed.Message.RequestedAt != msg.RequestedAt {
		t.Fatalf("resume = %+v, err = %v", resumed, err)
	}
	if countRows(t, db, "migration_executions") != 1 || countRows(t, db, "migration_jobs") != 2 {
		t.Fatal("resume should add a job, not an execution")
	}

	unknown := req
	unknown.DatabaseID += 1000
	if _, err := submitWith(store, unknown, jobD, nil); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown database: err = %v, want not found", err)
	}
	if _, err := store.SubmitExecution(ctx, workflow.Submission{Request: req}, func(workflow.SubmitState) error { return nil }); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("missing job id: err = %v, want invalid input", err)
	}
}

func TestSubmitExecutionStoresScopePostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	store := database.NewSubmitPostgresStore(db)
	req := approvedRequest(t, db, f)
	result, err := store.SubmitExecution(context.Background(), workflow.Submission{Request: req, JobID: jobA, Scope: &workflow.BatchScope{Min: 4, Max: 20}},
		func(workflow.SubmitState) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var lo, hi sql.NullInt64
	if err := db.QueryRowContext(context.Background(), `SELECT scope_min, scope_max FROM migration_executions WHERE id = $1`, result.Job.ExecutionID).Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo.Int64 != 4 || hi.Int64 != 20 || !lo.Valid || !hi.Valid {
		t.Fatalf("scope = %v..%v, want 4..20", lo, hi)
	}
}

func TestSubmitExecutionRefusedCheckWritesNothingPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	store := database.NewSubmitPostgresStore(db)
	req := approvedRequest(t, db, f)
	var seen workflow.SubmitState
	_, err := store.SubmitExecution(context.Background(), workflow.Submission{Request: req, JobID: jobA}, func(s workflow.SubmitState) error {
		seen = s
		return workflow.ErrActionNotAllowed
	})
	if !errors.Is(err, workflow.ErrActionNotAllowed) {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if countRows(t, db, "migration_executions") != 0 || countRows(t, db, "migration_jobs") != 0 {
		t.Fatal("a refused check wrote rows")
	}
	if seen.Revision.ID != req.RevisionID || seen.LatestRevision != 1 || seen.MigrationVersion != 1 ||
		seen.Review == nil || seen.Review.ID != req.ApprovalRequestID || len(seen.Executions) != 0 {
		t.Fatalf("state given to the check = %+v", seen)
	}
}

func TestSubmitExecutionConcurrentPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	store := database.NewSubmitPostgresStore(db)
	req := approvedRequest(t, db, f)
	ids := []string{jobA, jobB, jobC, jobD, "55555555-5555-5555-5555-555555555555", "66666666-6666-6666-6666-666666666666"}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := submitWith(store, req, id, nil)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, workflow.ErrDatabaseHeld):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || countRows(t, db, "migration_executions") != 1 {
		t.Fatalf("%d concurrent submits succeeded and %d executions exist, want exactly 1", ok, countRows(t, db, "migration_executions"))
	}
}
