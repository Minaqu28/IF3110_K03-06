package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	amqp "github.com/rabbitmq/amqp091-go"
)

func initWebQueue() (workflow.JobPublisher, worker.JobConsumer, func(), error) {
	amqpURL := strings.TrimSpace(os.Getenv("AMQP_URL"))
	if amqpURL == "" {
		mem := queue.NewMemory()
		return mem, mem, func() {}, nil
	}
	conn, err := amqp.DialConfig(amqpURL, amqp.Config{Dial: amqp.DefaultDial(brokerDialTimeout)})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to the message broker: %w", err)
	}
	publisher, err := queue.NewRabbitPublisher(conn, envOr("AMQP_QUEUE", defaultJobQueue))
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	cleanup := func() {
		_ = publisher.Close()
		_ = conn.Close()
	}
	return publisher, nil, cleanup, nil
}
