package queue

import (
	"context"
	"errors"
	"sync"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrNotConfirmed = errors.New("broker did not confirm the message")

type RabbitPublisher struct {
	mu    sync.Mutex
	ch    *amqp.Channel
	queue string
}

var _ workflow.JobPublisher = (*RabbitPublisher)(nil)

func NewRabbitPublisher(conn *amqp.Connection, queue string) (*RabbitPublisher, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := declareQueues(ch, queue); err != nil {
		_ = ch.Close()
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &RabbitPublisher{ch: ch, queue: queue}, nil
}

func (p *RabbitPublisher) Publish(ctx context.Context, msg domain.MigrationJobMessage) error {
	publishing, err := encodeMessage(msg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	confirm, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, "", p.queue, true, false, publishing)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !acked {
		return ErrNotConfirmed
	}
	return nil
}

func (p *RabbitPublisher) Close() error {
	return p.ch.Close()
}
