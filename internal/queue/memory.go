package queue

import (
	"context"
	"errors"
	"sync"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var ErrAlreadySettled = errors.New("delivery already acknowledged, requeued, or rejected")

type entry struct {
	msg         domain.MigrationJobMessage
	redelivered bool
}

type Memory struct {
	mu       sync.Mutex
	ready    []entry
	unacked  map[uint64]entry
	rejected []domain.MigrationJobMessage
	nextTag  uint64
	wake     chan struct{}
}

var (
	_ workflow.JobPublisher = (*Memory)(nil)
	_ worker.JobConsumer    = (*Memory)(nil)
)

func NewMemory() *Memory {
	return &Memory{unacked: map[uint64]entry{}, wake: make(chan struct{}, 1)}
}

func (m *Memory) Publish(ctx context.Context, msg domain.MigrationJobMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.ready = append(m.ready, entry{msg: msg})
	m.mu.Unlock()
	m.signal()
	return nil
}

func (m *Memory) Receive(ctx context.Context) (worker.JobDelivery, error) {
	for {
		m.mu.Lock()
		if len(m.ready) > 0 {
			e := m.ready[0]
			m.ready = m.ready[1:]
			m.nextTag++
			tag := m.nextTag
			m.unacked[tag] = e
			more := len(m.ready) > 0
			m.mu.Unlock()
			if more {
				m.signal()
			}
			return &memoryDelivery{queue: m, tag: tag, entry: e}, nil
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.wake:
		}
	}
}

func (m *Memory) RecoverUnacked() int {
	m.mu.Lock()
	n := len(m.unacked)
	for tag, e := range m.unacked {
		e.redelivered = true
		m.ready = append(m.ready, e)
		delete(m.unacked, tag)
	}
	m.mu.Unlock()
	if n > 0 {
		m.signal()
	}
	return n
}

func (m *Memory) Counts() (ready, unacked, rejected int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ready), len(m.unacked), len(m.rejected)
}

func (m *Memory) Rejected() []domain.MigrationJobMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.MigrationJobMessage(nil), m.rejected...)
}

func (m *Memory) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Memory) settle(tag uint64) (entry, error) {
	e, ok := m.unacked[tag]
	if !ok {
		return entry{}, ErrAlreadySettled
	}
	delete(m.unacked, tag)
	return e, nil
}

type memoryDelivery struct {
	queue *Memory
	tag   uint64
	entry entry
}

func (d *memoryDelivery) Message() domain.MigrationJobMessage { return d.entry.msg }
func (d *memoryDelivery) Redelivered() bool                   { return d.entry.redelivered }

func (d *memoryDelivery) Ack() error {
	d.queue.mu.Lock()
	defer d.queue.mu.Unlock()
	_, err := d.queue.settle(d.tag)
	return err
}

func (d *memoryDelivery) Requeue() error {
	d.queue.mu.Lock()
	e, err := d.queue.settle(d.tag)
	if err == nil {
		e.redelivered = true
		d.queue.ready = append(d.queue.ready, e)
	}
	d.queue.mu.Unlock()
	if err == nil {
		d.queue.signal()
	}
	return err
}

func (d *memoryDelivery) Reject() error {
	d.queue.mu.Lock()
	defer d.queue.mu.Unlock()
	e, err := d.queue.settle(d.tag)
	if err == nil {
		d.queue.rejected = append(d.queue.rejected, e.msg)
	}
	return err
}
