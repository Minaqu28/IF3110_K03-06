package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeOutbox struct {
	mu      sync.Mutex
	pending []string
	limits  []int
	err     error
	calls   chan struct{}
}

func (f *fakeOutbox) PublishDue(ctx context.Context, limit int, publish func(context.Context, domain.MigrationJobMessage) error) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	if f.calls != nil {
		select {
		case f.calls <- struct{}{}:
		default:
		}
	}
	if f.err != nil {
		return 0, f.err
	}
	n := min(limit, len(f.pending))
	published := 0
	var firstErr error
	for _, id := range f.pending[:n] {
		if err := publish(ctx, domain.MigrationJobMessage{JobID: id}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		published++
	}
	if firstErr == nil {
		f.pending = f.pending[n:]
	}
	return published, firstErr
}

func TestOutboxRelayFlushDrainsInBatches(t *testing.T) {
	store := &fakeOutbox{pending: []string{"a", "b", "c", "d", "e"}}
	pub := &fakePublisher{}
	n, err := NewOutboxRelay(store, pub, time.Second, 2).Flush(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("flushed %d, err = %v; want 5", n, err)
	}
	if len(pub.messages) != 5 || pub.messages[0].JobID != "a" || pub.messages[4].JobID != "e" {
		t.Fatalf("published = %+v", pub.messages)
	}
	if len(store.limits) != 3 || store.limits[0] != 2 {
		t.Fatalf("PublishDue limits = %v, want three calls of 2", store.limits)
	}
}

func TestOutboxRelayFlushStopsOnErrors(t *testing.T) {
	store := &fakeOutbox{pending: []string{"a", "b", "c", "d"}}
	pub := &fakePublisher{err: errors.New("broker down")}
	n, err := NewOutboxRelay(store, pub, time.Second, 2).Flush(context.Background())
	if n != 0 || err == nil || len(store.limits) != 1 {
		t.Fatalf("flushed %d, err = %v, calls = %d; want one failed round", n, err, len(store.limits))
	}

	boom := errors.New("db down")
	store = &fakeOutbox{err: boom}
	if _, err := NewOutboxRelay(store, &fakePublisher{}, time.Second, 2).Flush(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store error", err)
	}
}

func TestOutboxRelayDefaults(t *testing.T) {
	r := NewOutboxRelay(&fakeOutbox{}, &fakePublisher{}, 0, -1)
	if r.interval != DefaultRelayInterval || r.batch != DefaultRelayBatch {
		t.Fatalf("interval = %v, batch = %d", r.interval, r.batch)
	}
}

func TestOutboxRelayRunTicksUntilCancelled(t *testing.T) {
	store := &fakeOutbox{calls: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		NewOutboxRelay(store, &fakePublisher{}, 5*time.Millisecond, 10).Run(ctx)
		close(done)
	}()
	for range 3 {
		select {
		case <-store.calls:
		case <-time.After(time.Second):
			t.Fatal("relay did not keep polling")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
