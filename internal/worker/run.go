package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

func (w *Worker) Run(ctx context.Context, slots int) error {
	if slots < 1 {
		slots = 1
	}
	w.log.Info("worker started", "slots", slots)
	free := make(chan struct{}, slots)
	var running sync.WaitGroup
	defer running.Wait()
	for {
		select {
		case free <- struct{}{}:
		case <-ctx.Done():
			w.log.Info("worker stopping; waiting for running jobs", "reason", context.Cause(ctx))
			return nil
		}
		d, err := w.deps.Consumer.Receive(ctx)
		if err != nil {
			<-free
			if ctx.Err() != nil {
				w.log.Info("worker stopping; waiting for running jobs", "reason", context.Cause(ctx))
				return nil
			}
			return fmt.Errorf("receive migration job: %w", err)
		}
		running.Add(1)
		go func() {
			defer running.Done()
			defer func() { <-free }()
			w.handle(ctx, d)
		}()
	}
}

const DefaultSweepInterval = 10 * time.Second

type ExpiredJobSweeper interface {
	SweepExpiredJobs(ctx context.Context) (int, error)
}

func RunSweeper(ctx context.Context, sweeper ExpiredJobSweeper, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = DefaultSweepInterval
	}
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		swept, err := sweeper.SweepExpiredJobs(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("sweeping expired jobs failed", "error", err)
		case swept > 0:
			log.Warn("jobs whose worker stopped were marked for reconciliation", "jobs", swept)
		}
	}
}
