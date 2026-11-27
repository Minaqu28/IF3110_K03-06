package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	amqp "github.com/rabbitmq/amqp091-go"
)

type ackCall struct {
	op       string
	tag      uint64
	multiple bool
	requeue  bool
}

type fakeAcknowledger struct{ calls []ackCall }

func (f *fakeAcknowledger) Ack(tag uint64, multiple bool) error {
	f.calls = append(f.calls, ackCall{"ack", tag, multiple, false})
	return nil
}

func (f *fakeAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	f.calls = append(f.calls, ackCall{"nack", tag, multiple, requeue})
	return nil
}

func (f *fakeAcknowledger) Reject(tag uint64, requeue bool) error {
	f.calls = append(f.calls, ackCall{"reject", tag, false, requeue})
	return nil
}

func fullMessage() domain.MigrationJobMessage {
	target := "exec-0"
	return domain.MigrationJobMessage{
		SchemaVersion: domain.MigrationJobSchemaVersion, JobID: "job-1", ExecutionID: "exec-1",
		DatabaseID: 7, MigrationID: 3, RevisionID: 10, ApprovalRequestID: 20, ActorID: 1,
		Action: domain.MigrationActionRollback, Mode: domain.ExecutionModeBatch, TargetExecutionID: &target,
		RequestedAt: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC),
	}
}

func TestEncodeDecodeMessage(t *testing.T) {
	in := fullMessage()
	p, err := encodeMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.DeliveryMode != amqp.Persistent || p.ContentType != "application/json" || p.MessageId != "job-1" || p.Type != "migration.job" {
		t.Fatalf("publishing = %+v", p)
	}
	out, err := decodeMessage(p.Body)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, %v; want %+v", out, err, in)
	}
	for _, bad := range []domain.MigrationJobMessage{{ExecutionID: "e"}, {JobID: "j"}} {
		if _, err := encodeMessage(bad); !errors.Is(err, ErrMalformedMessage) {
			t.Errorf("encode %+v: err = %v", bad, err)
		}
	}
	for _, body := range []string{"not json", `{"job_id":""}`, `{"job_id":"j"}`, `[]`} {
		if _, err := decodeMessage([]byte(body)); !errors.Is(err, ErrMalformedMessage) {
			t.Errorf("decode %q: err = %v", body, err)
		}
	}
}

func TestReceiveFromMapsDeliveries(t *testing.T) {
	good, err := encodeMessage(fullMessage())
	if err != nil {
		t.Fatal(err)
	}
	ack := &fakeAcknowledger{}
	deliveries := make(chan amqp.Delivery, 2)
	deliveries <- amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, Body: []byte("garbage")}
	deliveries <- amqp.Delivery{Acknowledger: ack, DeliveryTag: 2, Body: good.Body, Redelivered: true}

	d, err := receiveFrom(context.Background(), deliveries)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Message(), fullMessage()) || !d.Redelivered() {
		t.Fatalf("delivery = %+v redelivered=%v", d.Message(), d.Redelivered())
	}
	_ = d.Ack()
	_ = d.Requeue()
	_ = d.Reject()
	want := []ackCall{
		{"reject", 1, false, false},
		{"ack", 2, false, false},
		{"nack", 2, false, true},
		{"reject", 2, false, false},
	}
	if !reflect.DeepEqual(ack.calls, want) {
		t.Fatalf("acknowledger calls = %+v, want %+v", ack.calls, want)
	}

	close(deliveries)
	if _, err := receiveFrom(context.Background(), deliveries); !errors.Is(err, ErrConsumerClosed) {
		t.Fatalf("closed channel: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiveFrom(ctx, make(chan amqp.Delivery)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v", err)
	}
}

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
