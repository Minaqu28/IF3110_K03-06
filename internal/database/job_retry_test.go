package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

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
