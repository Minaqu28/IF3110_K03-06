package database_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

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
