package worker

import (
	"context"
	"fmt"
	"sync"
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
