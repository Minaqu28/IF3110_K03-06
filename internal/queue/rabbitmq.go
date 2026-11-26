package queue

import (
	"encoding/json"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
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
