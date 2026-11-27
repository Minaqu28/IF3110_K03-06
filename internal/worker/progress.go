package worker

import (
	"context"
	"sync"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type activeRun struct {
	mu   sync.Mutex
	job  domain.Job
	jobs JobStore
}

type activeRuns struct {
	mu   sync.Mutex
	runs map[string]*activeRun
}

func (a *activeRuns) track(executionID string, job domain.Job, jobs JobStore) *activeRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs == nil {
		a.runs = map[string]*activeRun{}
	}
	run := &activeRun{job: job, jobs: jobs}
	a.runs[executionID] = run
	return run
}

func (a *activeRuns) untrack(executionID string, run *activeRun) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs[executionID] == run {
		delete(a.runs, executionID)
	}
}

func (a *activeRuns) get(executionID string) *activeRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[executionID]
}

func (r *activeRun) latest() domain.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}

func (w *Worker) ReportProgress(ctx context.Context, executionID string, progress domain.BatchProgress) error {
	run := w.active.get(executionID)
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if progress.Total == nil && run.job.Progress != nil {
		progress.Total = run.job.Progress.Total
	}
	job := run.job
	job.Progress = &progress
	if err := run.jobs.SaveJob(ctx, job); err != nil {
		return err
	}
	batches, rows := progress.Completed, progress.AffectedRows
	if prev := run.job.Progress; prev != nil {
		batches, rows = batches-prev.Completed, rows-prev.AffectedRows
	}
	if batches > 0 || rows > 0 {
		w.rec.BatchesCommitted(max(batches, 0), max(rows, 0))
	}
	run.job = job
	return nil
}
