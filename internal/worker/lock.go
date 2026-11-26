package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const DefaultHeartbeatInterval = 10 * time.Second

type lease struct {
	locks    DatabaseLocker
	database int64
	owner    string
	token    int64
	log      *slog.Logger
	cancel   context.CancelCauseFunc
	done     chan struct{}
	stopOnce sync.Once
}

func (w *Worker) acquire(ctx context.Context, log *slog.Logger, databaseID int64, executionID string) (*lease, bool, error) {
	token, acquired, err := w.deps.Locks.Acquire(ctx, databaseID, executionID, w.id)
	if err != nil || !acquired {
		return nil, false, err
	}
	return &lease{locks: w.deps.Locks, database: databaseID, owner: w.id, token: token, log: log.With("fencing_token", token)}, true, nil
}

func (l *lease) keepAlive(ctx context.Context, every time.Duration) context.Context {
	runCtx, cancel := context.WithCancelCause(ctx)
	l.cancel, l.done = cancel, make(chan struct{})
	go func() {
		defer close(l.done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			held, err := l.locks.Heartbeat(context.WithoutCancel(runCtx), l.database, l.owner, l.token)
			if err != nil {
				l.log.Warn("lock heartbeat failed; will retry", "error", err)
				continue
			}
			if !held {
				l.log.Error("database lock lost; stopping after the current batch")
				cancel(ErrLockLost)
				return
			}
		}
	}()
	return runCtx
}

func (l *lease) stop() {
	l.stopOnce.Do(func() {
		if l.cancel != nil {
			l.cancel(context.Canceled)
			<-l.done
		}
	})
}

func (l *lease) release(ctx context.Context) {
	l.stop()
	if err := l.locks.Release(context.WithoutCancel(ctx), l.database, l.owner, l.token); err != nil {
		l.log.Warn("could not release the database lock; it expires with its lease", "error", err)
	}
}
