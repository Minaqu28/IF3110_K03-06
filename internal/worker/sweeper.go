package worker

import (
	"context"
	"log/slog"
	"time"
)

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
