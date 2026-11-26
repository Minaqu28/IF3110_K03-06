package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

func msg(id string) domain.MigrationJobMessage {
	return domain.MigrationJobMessage{SchemaVersion: domain.MigrationJobSchemaVersion, JobID: id, ExecutionID: "exec-" + id}
}

func receive(t *testing.T, q *Memory) worker.JobDelivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := q.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return d
}

func TestMemoryDeliversInOrderAndAcks(t *testing.T) {
	q := NewMemory()
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := q.Publish(ctx, msg(id)); err != nil {
			t.Fatal(err)
		}
	}
	first, second := receive(t, q), receive(t, q)
	if first.Message() != msg("a") || second.Message() != msg("b") || first.Redelivered() || second.Redelivered() {
		t.Fatalf("got %+v then %+v", first.Message(), second.Message())
	}
	if ready, unacked, _ := q.Counts(); ready != 0 || unacked != 2 {
		t.Fatalf("ready=%d unacked=%d, want 0 and 2", ready, unacked)
	}
	if err := first.Ack(); err != nil {
		t.Fatal(err)
	}
	if err := first.Ack(); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("second ack: err = %v, want already settled", err)
	}
	if err := first.Requeue(); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("requeue after ack: err = %v, want already settled", err)
	}
	if _, unacked, _ := q.Counts(); unacked != 1 {
		t.Fatalf("unacked = %d, want 1", unacked)
	}
}

func TestMemoryRequeueRedelivers(t *testing.T) {
	q := NewMemory()
	if err := q.Publish(context.Background(), msg("a")); err != nil {
		t.Fatal(err)
	}
	d := receive(t, q)
	if err := d.Requeue(); err != nil {
		t.Fatal(err)
	}
	again := receive(t, q)
	if again.Message() != msg("a") || !again.Redelivered() {
		t.Fatalf("redelivery = %+v redelivered=%v", again.Message(), again.Redelivered())
	}
	if err := again.Ack(); err != nil {
		t.Fatal(err)
	}
	if ready, unacked, _ := q.Counts(); ready != 0 || unacked != 0 {
		t.Fatalf("ready=%d unacked=%d after ack", ready, unacked)
	}
}

func TestMemoryRejectDropsToDeadLetters(t *testing.T) {
	q := NewMemory()
	if err := q.Publish(context.Background(), msg("bad")); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, q).Reject(); err != nil {
		t.Fatal(err)
	}
	if ready, unacked, rejected := q.Counts(); ready != 0 || unacked != 0 || rejected != 1 {
		t.Fatalf("ready=%d unacked=%d rejected=%d", ready, unacked, rejected)
	}
	if r := q.Rejected(); len(r) != 1 || r[0] != msg("bad") {
		t.Fatalf("rejected = %+v", r)
	}
}

func TestMemoryRecoverUnackedSimulatesAConsumerCrash(t *testing.T) {
	q := NewMemory()
	ctx := context.Background()
	_ = q.Publish(ctx, msg("a"))
	_ = q.Publish(ctx, msg("b"))
	lost := receive(t, q)
	if n := q.RecoverUnacked(); n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}
	if err := lost.Ack(); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("ack from the crashed consumer: err = %v, want already settled", err)
	}
	next, redelivered := receive(t, q), receive(t, q)
	if next.Message() != msg("b") || next.Redelivered() || redelivered.Message() != msg("a") || !redelivered.Redelivered() {
		t.Fatalf("after recovery got %+v (%v) then %+v (%v)", next.Message(), next.Redelivered(), redelivered.Message(), redelivered.Redelivered())
	}
}

func TestMemoryReceiveWaitsAndHonoursCancel(t *testing.T) {
	q := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := q.Receive(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Receive returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	got := make(chan worker.JobDelivery, 1)
	go func() {
		d, _ := q.Receive(context.Background())
		got <- d
	}()
	time.Sleep(10 * time.Millisecond)
	_ = q.Publish(context.Background(), msg("late"))
	select {
	case d := <-got:
		if d.Message() != msg("late") {
			t.Fatalf("got %+v", d.Message())
		}
	case <-time.After(time.Second):
		t.Fatal("a waiting Receive was not woken by Publish")
	}
	if err := q.Publish(ctx, msg("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish with a cancelled context: err = %v", err)
	}
}

func TestMemoryConcurrentConsumersGetEachMessageOnce(t *testing.T) {
	q := NewMemory()
	const n = 200
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				d, err := q.Receive(ctx)
				if err != nil {
					return
				}
				mu.Lock()
				seen[d.Message().JobID]++
				total := len(seen)
				mu.Unlock()
				_ = d.Ack()
				if total == n {
					cancel()
				}
			}
		}()
	}
	for i := range n {
		_ = q.Publish(context.Background(), msg(string(rune('A'+i%26))+string(rune('a'+i/26))))
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("consumers saw %d distinct messages, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("message %s delivered %d times", id, c)
		}
	}
}
