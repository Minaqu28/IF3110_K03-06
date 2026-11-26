package database_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

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
