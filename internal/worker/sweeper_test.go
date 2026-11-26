package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type sweepFunc func(context.Context) (int, error)

func (f sweepFunc) SweepExpiredJobs(ctx context.Context) (int, error) { return f(ctx) }

func TestRunSweeperTicksUntilCancelled(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSweeper(ctx, sweepFunc(func(context.Context) (int, error) {
			if calls.Add(1) == 2 {
				return 0, errors.New("platform down")
			}
			return 1, nil
		}), time.Millisecond, quietLog())
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper stopped ticking after an error")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSweeper did not return after cancel")
	}
}
