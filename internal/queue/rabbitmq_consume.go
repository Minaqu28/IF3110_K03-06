package queue

import (
	"context"
	"errors"
	"log"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrConsumerClosed = errors.New("broker closed the consumer")

type RabbitConsumer struct {
	ch         *amqp.Channel
	deliveries <-chan amqp.Delivery
}

var _ worker.JobConsumer = (*RabbitConsumer)(nil)

func NewRabbitConsumer(ctx context.Context, conn *amqp.Connection, queue string, prefetch int) (*RabbitConsumer, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := declareQueues(ch, queue); err != nil {
		_ = ch.Close()
		return nil, err
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	deliveries, err := ch.ConsumeWithContext(ctx, queue, "", false, false, false, false, nil)
	if err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &RabbitConsumer{ch: ch, deliveries: deliveries}, nil
}

func (c *RabbitConsumer) Receive(ctx context.Context) (worker.JobDelivery, error) {
	return receiveFrom(ctx, c.deliveries)
}

func (c *RabbitConsumer) Close() error {
	return c.ch.Close()
}

func receiveFrom(ctx context.Context, deliveries <-chan amqp.Delivery) (worker.JobDelivery, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return nil, ErrConsumerClosed
			}
			msg, err := decodeMessage(d.Body)
			if err != nil {
				log.Printf("rejecting malformed migration job message %q", d.MessageId)
				_ = d.Reject(false)
				continue
			}
			return &rabbitDelivery{d: d, msg: msg}, nil
		}
	}
}

type rabbitDelivery struct {
	d   amqp.Delivery
	msg domain.MigrationJobMessage
}

func (r *rabbitDelivery) Message() domain.MigrationJobMessage { return r.msg }
func (r *rabbitDelivery) Redelivered() bool                   { return r.d.Redelivered }
func (r *rabbitDelivery) Ack() error                          { return r.d.Ack(false) }
func (r *rabbitDelivery) Requeue() error                      { return r.d.Nack(false, true) }
func (r *rabbitDelivery) Reject() error                       { return r.d.Reject(false) }
