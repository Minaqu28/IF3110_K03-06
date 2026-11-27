package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
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

func TestSaveJobAfterDelaysTheRetryPostgres(t *testing.T) {
	db := integrationDB(t)
	f, execution := newJobFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: execution, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkPublished(ctx, jobA); err != nil {
		t.Fatal(err)
	}
	var published []string
	publish := func(_ context.Context, m domain.MigrationJobMessage) error {
		published = append(published, m.JobID)
		return nil
	}

	retry := domain.Job{ID: jobA, Status: domain.JobStatusRetryWait, AttemptCount: 1, ErrorCode: "40P01"}
	if err := jobs.SaveJobAfter(ctx, retry, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PublishDue(ctx, 10, publish); err != nil || n != 0 {
		t.Fatalf("a retry due in an hour was published: n = %d, err = %v", n, err)
	}
	if got, _ := jobs.GetJob(ctx, jobA); got.Status != domain.JobStatusRetryWait || got.ErrorCode != "40P01" {
		t.Fatalf("job = %+v", got)
	}

	if err := jobs.SaveJobAfter(ctx, retry, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PublishDue(ctx, 10, publish); err != nil || n != 1 || published[0] != jobA {
		t.Fatalf("a due retry: n = %d, err = %v, published %v", n, err, published)
	}

	if err := jobs.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusRetryWait, AttemptCount: 2}); err != nil {
		t.Fatal(err)
	}
	if n, _ := jobs.PublishDue(ctx, 10, publish); n != 1 {
		t.Fatal("SaveJob must keep the earlier due time")
	}
}

func TestFencedSaveJobAfterPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, _ := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	locks := database.NewLockPostgresStore(db, 30*time.Second)
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: first, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	token := mustAcquire(t, locks, f.databaseID, first, "worker-a", true)
	retry := domain.Job{ID: jobA, Status: domain.JobStatusRetryWait, AttemptCount: 1}
	if err := jobs.Fenced("worker-b", token).SaveJobAfter(ctx, retry, time.Now().Add(time.Minute)); !errors.Is(err, worker.ErrLockLost) {
		t.Fatalf("another owner: err = %v, want lock lost", err)
	}
	if err := jobs.Fenced("worker-a", token).SaveJobAfter(ctx, retry, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("holder: %v", err)
	}
	var due bool
	if err := db.QueryRowContext(ctx, `SELECT next_attempt_at > now() + interval '30 seconds' FROM migration_jobs WHERE id = $1`, jobA).Scan(&due); err != nil || !due {
		t.Fatalf("next_attempt_at not delayed: %v, err = %v", due, err)
	}
}

const jobD = "44444444-4444-4444-4444-444444444444"

func jobColumn[T any](t *testing.T, db *sql.DB, jobID, column string) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), `SELECT `+column+` FROM migration_jobs WHERE id = $1`, jobID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestFencedJobStorePostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	locks := database.NewLockPostgresStore(db, 30*time.Second)
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: first, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobB, ExecutionID: second, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	token := mustAcquire(t, locks, f.databaseID, first, "worker-a", true)

	if err := jobs.Fenced("worker-a", token).SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusRunning, AttemptCount: 1}); err != nil {
		t.Fatalf("holder save: %v", err)
	}
	lost := map[string]struct {
		store *database.FencedJobStore
		job   string
	}{
		"stale token":     {jobs.Fenced("worker-a", token-1), jobA},
		"other owner":     {jobs.Fenced("worker-b", token), jobA},
		"other execution": {jobs.Fenced("worker-a", token), jobB},
		"unknown job":     {jobs.Fenced("worker-a", token), jobC},
	}
	for name, c := range lost {
		t.Run(name, func(t *testing.T) {
			if err := c.store.SaveJob(ctx, domain.Job{ID: c.job, Status: domain.JobStatusFailed, AttemptCount: 9}); !errors.Is(err, worker.ErrLockLost) {
				t.Fatalf("err = %v, want lock lost", err)
			}
		})
	}
	if got, _ := jobs.GetJob(ctx, jobA); got.Status != domain.JobStatusRunning || got.AttemptCount != 1 {
		t.Fatalf("a fenced-out save changed the job: %+v", got)
	}
	got, err := jobs.Fenced("worker-b", 1).GetJob(ctx, jobA)
	if err != nil || got.ID != jobA {
		t.Fatalf("fenced GetJob = %+v, %v", got, err)
	}

	expireLock(t, db, f.databaseID)
	if err := jobs.Fenced("worker-a", token).SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusSucceeded, AttemptCount: 1}); !errors.Is(err, worker.ErrLockLost) {
		t.Fatalf("save after lease expiry: err = %v, want lock lost", err)
	}
	if err := jobs.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusSucceeded, AttemptCount: 1}); err != nil {
		t.Fatalf("unfenced save: %v", err)
	}
}

func TestJobOutboxPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	for _, j := range []domain.Job{
		{ID: jobA, ExecutionID: first, DatabaseID: f.databaseID},
		{ID: jobB, ExecutionID: second, DatabaseID: f.databaseID},
		{ID: jobC, ExecutionID: first, DatabaseID: f.databaseID},
		{ID: jobD, ExecutionID: second, DatabaseID: f.databaseID},
	} {
		if err := jobs.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.MarkPublished(ctx, jobB); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `UPDATE migration_jobs SET status = 'RETRY_WAIT', next_attempt_at = now() + interval '1 hour' WHERE id = $1`, jobC)
	execSQL(t, db, `UPDATE migration_jobs SET status = 'SUCCEEDED' WHERE id = $1`, jobD)

	var sent []domain.MigrationJobMessage
	collect := func(_ context.Context, m domain.MigrationJobMessage) error { sent = append(sent, m); return nil }
	n, err := jobs.PublishDue(ctx, 10, collect)
	if err != nil || n != 1 || len(sent) != 1 {
		t.Fatalf("PublishDue = %d, %v, sent %+v; want only job A", n, err, sent)
	}
	msg := sent[0]
	var requestedAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT requested_at FROM migration_executions WHERE id = $1`, first).Scan(&requestedAt); err != nil {
		t.Fatal(err)
	}
	want := domain.MigrationJobMessage{
		SchemaVersion: domain.MigrationJobSchemaVersion, JobID: jobA, ExecutionID: first, DatabaseID: f.databaseID,
		MigrationID: f.migrationID, RevisionID: msg.RevisionID, ApprovalRequestID: msg.ApprovalRequestID, ActorID: f.userID,
		Action: domain.MigrationActionApply, Mode: domain.ExecutionModeNormal, RequestedAt: requestedAt,
	}
	if !reflect.DeepEqual(msg, want) || msg.RevisionID == 0 || msg.ApprovalRequestID == 0 {
		t.Fatalf("message = %+v, want %+v", msg, want)
	}
	if n, err := jobs.PublishDue(ctx, 10, collect); err != nil || n != 0 {
		t.Fatalf("second PublishDue = %d, %v; want nothing left", n, err)
	}
	if err := jobs.MarkPublished(ctx, jobA); err != nil {
		t.Fatalf("MarkPublished on an already published job: %v", err)
	}
	if err := jobs.MarkPublished(ctx, "55555555-5555-5555-5555-555555555555"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("MarkPublished unknown job: err = %v, want not found", err)
	}

	if err := jobs.SaveJob(ctx, domain.Job{ID: jobA, Status: domain.JobStatusRetryWait, AttemptCount: 1}); err != nil {
		t.Fatal(err)
	}
	if jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("RETRY_WAIT did not put the job back in the outbox")
	}
	if err := jobs.SaveJob(ctx, domain.Job{ID: jobB, Status: domain.JobStatusRunning, AttemptCount: 1}); err != nil {
		t.Fatal(err)
	}
	if !jobColumn[sql.NullTime](t, db, jobB, "published_at").Valid {
		t.Fatal("RUNNING cleared published_at")
	}

	boom := errors.New("broker down")
	execSQL(t, db, `UPDATE migration_jobs SET next_attempt_at = now(), published_at = NULL WHERE id IN ($1, $2)`, jobB, jobC)
	execSQL(t, db, `UPDATE migration_jobs SET status = 'QUEUED' WHERE id IN ($1, $2)`, jobB, jobC)
	sent = nil
	n, err = jobs.PublishDue(ctx, 10, func(ctx context.Context, m domain.MigrationJobMessage) error {
		if m.JobID == jobB {
			return boom
		}
		return collect(ctx, m)
	})
	if !errors.Is(err, boom) || n != 2 {
		t.Fatalf("partial failure = %d, %v; want 2 published and the broker error", n, err)
	}
	if jobColumn[sql.NullTime](t, db, jobB, "published_at").Valid {
		t.Fatal("a job whose publish failed was marked published")
	}
	ids := []string{sent[0].JobID, sent[1].JobID}
	sort.Strings(ids)
	if ids[0] != jobA || ids[1] != jobC {
		t.Fatalf("published %v, want jobs A and C", ids)
	}

	if _, err := jobs.PublishDue(ctx, 0, collect); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("zero limit: err = %v, want invalid input", err)
	}
}

func TestJobOutboxSkipsLockedRowsPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobA, ExecutionID: first, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateJob(ctx, domain.Job{ID: jobB, ExecutionID: second, DatabaseID: f.databaseID}); err != nil {
		t.Fatal(err)
	}
	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM migration_jobs WHERE id = $1 FOR UPDATE`, jobA); err != nil {
		t.Fatal(err)
	}

	done := make(chan []string, 1)
	go func() {
		var ids []string
		_, _ = jobs.PublishDue(ctx, 10, func(_ context.Context, m domain.MigrationJobMessage) error {
			ids = append(ids, m.JobID)
			return nil
		})
		done <- ids
	}()
	select {
	case ids := <-done:
		if len(ids) != 1 || ids[0] != jobB {
			t.Fatalf("published %v while job A was locked, want only job B", ids)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishDue blocked on a row locked by another relay")
	}
}

func TestSweepExpiredJobsPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	locks := database.NewLockPostgresStore(db, 30*time.Second)
	otherDB := insertConnection(t, db, "other target")
	for _, j := range []domain.Job{
		{ID: jobA, ExecutionID: first, DatabaseID: f.databaseID},
		{ID: jobB, ExecutionID: second, DatabaseID: f.databaseID},
		{ID: jobC, ExecutionID: second, DatabaseID: f.databaseID},
	} {
		if err := jobs.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if err := jobs.MarkPublished(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	mustAcquire(t, locks, f.databaseID, second, "worker-b", true)
	mustAcquire(t, locks, otherDB, first, "worker-a", true)
	for _, id := range []string{jobA, jobB} {
		if err := jobs.SaveJob(ctx, domain.Job{ID: id, Status: domain.JobStatusRunning, AttemptCount: 1}); err != nil {
			t.Fatal(err)
		}
	}

	swept, err := jobs.SweepExpiredJobs(ctx)
	if err != nil || swept != 1 {
		t.Fatalf("first sweep = %d, %v; want only job A (its lock is for another database)", swept, err)
	}
	if got, _ := jobs.GetJob(ctx, jobA); got.Status != domain.JobStatusNeedsReconciliation || got.ErrorCode != "LEASE_EXPIRED" {
		t.Fatalf("swept job = %+v", got)
	}
	if jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("swept job is not back in the outbox")
	}
	if m := readExecutionMirror(t, db, first); m.status != "UNKNOWN" {
		t.Fatalf("swept execution status = %s, want UNKNOWN", m.status)
	}
	if got, _ := jobs.GetJob(ctx, jobB); got.Status != domain.JobStatusRunning {
		t.Fatalf("job with a live lease was swept: %+v", got)
	}
	if got, _ := jobs.GetJob(ctx, jobC); got.Status != domain.JobStatusQueued {
		t.Fatalf("queued job was swept: %+v", got)
	}

	execSQL(t, db, `UPDATE migration_executions SET status = 'SUCCEEDED' WHERE id = $1`, second)
	expireLock(t, db, f.databaseID)
	if swept, err := jobs.SweepExpiredJobs(ctx); err != nil || swept != 1 {
		t.Fatalf("second sweep = %d, %v; want job B", swept, err)
	}
	if m := readExecutionMirror(t, db, second); m.status != "SUCCEEDED" {
		t.Fatalf("sweep overwrote a finished execution: %s", m.status)
	}
	if swept, err := jobs.SweepExpiredJobs(ctx); err != nil || swept != 0 {
		t.Fatalf("third sweep = %d, %v; want nothing", swept, err)
	}
}

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
	query := workflow.NewJobService(access, nil, nil, nil, jobs)
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
	if _, err := query.GetJob(ctx, author, jobA); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("no access: err = %v, want forbidden", err)
	}
}

func TestOutboxRelayPublishesUnpublishedJobsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	result, err := submitWith(database.NewSubmitPostgresStore(db), approvedRequest(t, db, f), jobA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("job is already published before the relay ran")
	}

	broker := queue.NewMemory()
	relay := workflow.NewOutboxRelay(database.NewJobPostgresStore(db), broker, time.Second, 10)
	n, err := relay.Flush(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("flushed %d, err = %v; want 1", n, err)
	}
	if !jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("the relay did not mark the job published")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := broker.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Message(), result.Message) {
		t.Fatalf("relayed %+v, want the submitted message %+v", d.Message(), result.Message)
	}

	if n, err := relay.Flush(context.Background()); err != nil || n != 0 {
		t.Fatalf("second flush published %d, err = %v; want nothing", n, err)
	}
	if ready, _, _ := broker.Counts(); ready != 0 {
		t.Fatalf("%d message(s) relayed twice", ready)
	}
}

func TestSweeperTickRecoversAStoppedWorkersJobPostgres(t *testing.T) {
	db := integrationDB(t)
	f, crashed, alive := newLockFixture(t, db)
	ctx := context.Background()
	jobs := database.NewJobPostgresStore(db)
	for id, execution := range map[string]string{jobA: crashed, jobB: alive} {
		if err := jobs.CreateJob(ctx, domain.Job{ID: id, ExecutionID: execution, DatabaseID: f.databaseID}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.SaveJob(ctx, domain.Job{ID: id, Status: domain.JobStatusRunning, AttemptCount: 1}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.MarkPublished(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	mustAcquire(t, database.NewLockPostgresStore(db, 30*time.Second), f.databaseID, alive, "worker-live", true)

	sweepCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		worker.RunSweeper(sweepCtx, jobs, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := jobs.GetJob(ctx, jobA)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == domain.JobStatusNeedsReconciliation {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("crashed job still %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done

	if got, _ := jobs.GetJob(ctx, jobB); got.Status != domain.JobStatusRunning {
		t.Fatalf("a job with a live lock was swept: %s", got.Status)
	}
	if m := readExecutionMirror(t, db, crashed); m.status != "UNKNOWN" {
		t.Fatalf("crashed execution = %s, want UNKNOWN", m.status)
	}
	var republished []string
	if _, err := jobs.PublishDue(ctx, 10, func(_ context.Context, m domain.MigrationJobMessage) error {
		republished = append(republished, m.JobID)
		return nil
	}); err != nil || len(republished) != 1 || republished[0] != jobA {
		t.Fatalf("republished %v, err = %v; the swept job goes back through the outbox", republished, err)
	}
}

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

func newLockFixture(t *testing.T, db *sql.DB) (asyncFixture, string, string) {
	t.Helper()
	f, first := newJobFixture(t, db)
	var revision, review int64
	if err := db.QueryRowContext(context.Background(), `
		SELECT revision_id, approval_request_id FROM migration_executions WHERE id = $1`, first).Scan(&revision, &review); err != nil {
		t.Fatal(err)
	}
	second, err := f.insertExecution(db, revision, review, "APPLY", "PENDING", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, first, second
}

func insertConnection(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO database_connections (name, host, db_name, db_user, password_encrypted)
		VALUES ($1, 'localhost', 'target', 'target', $2) RETURNING id`, name, []byte{0}).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func expireLock(t *testing.T, db *sql.DB, databaseID int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE database_locks
		SET acquired_at = now() - interval '1 hour', lease_expires_at = now() - interval '1 minute'
		WHERE database_id = $1`, databaseID); err != nil {
		t.Fatal(err)
	}
}

func mustAcquire(t *testing.T, store *database.LockPostgresStore, databaseID int64, execution, owner string, want bool) int64 {
	t.Helper()
	token, ok, err := store.Acquire(context.Background(), databaseID, execution, owner)
	if err != nil || ok != want || (ok && token <= 0) || (!ok && token != 0) {
		t.Fatalf("Acquire(%d, %s) = %d, %v, %v; want acquired=%v", databaseID, owner, token, ok, err, want)
	}
	return token
}

func TestLockStoreAcquireReleasePostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	store := database.NewLockPostgresStore(db, 30*time.Second)

	t1 := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	mustAcquire(t, store, f.databaseID, first, "worker-b", false)
	t2 := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	if t2 <= t1 {
		t.Fatalf("re-acquire token %d is not newer than %d", t2, t1)
	}

	otherDB := insertConnection(t, db, "other target")
	mustAcquire(t, store, otherDB, second, "worker-b", true)

	if err := store.Release(ctx, f.databaseID, "worker-a", t1); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	if err := store.Release(ctx, f.databaseID, "worker-b", t2); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	if err := store.Release(ctx, f.databaseID, "worker-a", t2); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", true)

	if _, _, err := store.Acquire(ctx, insertConnection(t, db, "third"), "00000000-0000-0000-0000-000000000000", "worker-a"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown execution: err = %v, want not found", err)
	}
	if _, _, err := store.Acquire(ctx, insertConnection(t, db, "fourth"), first, "  "); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("blank owner: err = %v, want invalid input", err)
	}
}

func TestLockStoreConcurrentAcquirePostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	store := database.NewLockPostgresStore(db, 30*time.Second)

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	start := make(chan struct{})
	for i := range workers {
		execution := first
		if i%2 == 1 {
			execution = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := store.Acquire(context.Background(), f.databaseID, execution, fmt.Sprintf("worker-%d", i))
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	acquired := 0
	for ok := range results {
		if ok {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("%d workers acquired the same database lock, want exactly 1", acquired)
	}
}

func TestLockStoreHeartbeatAndTakeoverPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	store := database.NewLockPostgresStore(db, 30*time.Second)
	leaseOf := func() time.Time {
		var lease time.Time
		if err := db.QueryRowContext(ctx, `SELECT lease_expires_at FROM database_locks WHERE database_id = $1`, f.databaseID).Scan(&lease); err != nil {
			t.Fatal(err)
		}
		return lease
	}

	old := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	before := leaseOf()
	time.Sleep(5 * time.Millisecond)
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || !held {
		t.Fatalf("holder heartbeat = %v, %v", held, err)
	}
	if after := leaseOf(); !after.After(before) {
		t.Fatalf("heartbeat did not extend the lease: %v -> %v", before, after)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old+1000); err != nil || held {
		t.Fatalf("wrong token heartbeat = %v, %v", held, err)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-b", old); err != nil || held {
		t.Fatalf("wrong owner heartbeat = %v, %v", held, err)
	}

	expireLock(t, db, f.databaseID)
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || held {
		t.Fatalf("heartbeat revived an expired lease: %v, %v", held, err)
	}
	fresh := mustAcquire(t, store, f.databaseID, second, "worker-b", true)
	if fresh <= old {
		t.Fatalf("takeover token %d is not newer than %d", fresh, old)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || held {
		t.Fatalf("old owner still holds the lock after takeover: %v, %v", held, err)
	}
	if err := store.Release(ctx, f.databaseID, "worker-a", old); err != nil {
		t.Fatal(err)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-b", fresh); err != nil || !held {
		t.Fatalf("old owner's release removed the new lock: %v, %v", held, err)
	}
}
