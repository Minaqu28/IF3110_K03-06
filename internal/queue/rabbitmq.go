package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrMalformedMessage = errors.New("malformed migration job message")

func DeadLetterQueue(queue string) string {
	return queue + ".dead"
}

func declareQueues(ch *amqp.Channel, queue string) error {
	if _, err := ch.QueueDeclare(DeadLetterQueue(queue), true, false, false, false, nil); err != nil {
		return err
	}
	_, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": DeadLetterQueue(queue),
	})
	return err
}

func encodeMessage(msg domain.MigrationJobMessage) (amqp.Publishing, error) {
	if msg.JobID == "" || msg.ExecutionID == "" {
		return amqp.Publishing{}, ErrMalformedMessage
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return amqp.Publishing{}, err
	}
	return amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    msg.JobID,
		Type:         "migration.job",
		Body:         body,
	}, nil
}

func decodeMessage(body []byte) (domain.MigrationJobMessage, error) {
	var msg domain.MigrationJobMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.JobID == "" || msg.ExecutionID == "" {
		return domain.MigrationJobMessage{}, ErrMalformedMessage
	}
	return msg, nil
}

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
