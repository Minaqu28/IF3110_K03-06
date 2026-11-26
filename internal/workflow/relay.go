package workflow

import (
	"context"
	"log"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	DefaultRelayInterval = 5 * time.Second
	DefaultRelayBatch    = 100
)

type OutboxStore interface {
	PublishDue(ctx context.Context, limit int, publish func(context.Context, domain.MigrationJobMessage) error) (int, error)
}

type OutboxRelay struct {
	store     OutboxStore
	publisher JobPublisher
	interval  time.Duration
	batch     int
}

func NewOutboxRelay(store OutboxStore, publisher JobPublisher, interval time.Duration, batch int) *OutboxRelay {
	if interval <= 0 {
		interval = DefaultRelayInterval
	}
	if batch <= 0 {
		batch = DefaultRelayBatch
	}
	return &OutboxRelay{store: store, publisher: publisher, interval: interval, batch: batch}
}

func (r *OutboxRelay) Flush(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := r.store.PublishDue(ctx, r.batch, r.publisher.Publish)
		total += n
		if err != nil || n < r.batch {
			return total, err
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

func (r *OutboxRelay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if n, err := r.Flush(ctx); err != nil && ctx.Err() == nil {
			log.Printf("outbox relay: published %d job(s), then: %v", n, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
