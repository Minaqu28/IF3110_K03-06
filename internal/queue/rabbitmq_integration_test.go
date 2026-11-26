package queue

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRabbitRoundTripIntegration(t *testing.T) {
	url := os.Getenv("TEST_AMQP_URL")
	if url == "" {
		t.Skip("set TEST_AMQP_URL to run RabbitMQ integration tests")
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	queue := fmt.Sprintf("dbmp-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if ch, err := conn.Channel(); err == nil {
			_, _ = ch.QueueDelete(queue, false, false, false)
			_, _ = ch.QueueDelete(DeadLetterQueue(queue), false, false, false)
			_ = ch.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pub, err := NewRabbitPublisher(conn, queue)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	consumer, err := NewRabbitConsumer(ctx, conn, queue, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()

	if err := pub.Publish(ctx, fullMessage()); err != nil {
		t.Fatal(err)
	}
	first, err := consumer.Receive(ctx)
	if err != nil || first.Redelivered() || !reflect.DeepEqual(first.Message(), fullMessage()) {
		t.Fatalf("first delivery = %+v, %v", first, err)
	}
	if err := first.Requeue(); err != nil {
		t.Fatal(err)
	}
	again, err := consumer.Receive(ctx)
	if err != nil || !again.Redelivered() {
		t.Fatalf("requeued delivery = %+v, %v; want redelivered", again, err)
	}
	if err := again.Reject(); err != nil {
		t.Fatal(err)
	}

	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, ok, err := ch.Get(DeadLetterQueue(queue), true)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if decoded, err := decodeMessage(d.Body); err != nil || decoded.JobID != "job-1" {
				t.Fatalf("dead letter = %+v, %v", decoded, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("rejected message never reached the dead-letter queue")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
