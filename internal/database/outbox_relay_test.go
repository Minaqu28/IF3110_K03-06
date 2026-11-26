package database_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func TestOutboxRelayPublishesUnpublishedJobsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	result, err := submitWith(database.NewSubmitPostgresStore(db), approvedRequest(t, db, f), jobA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("job is already published before the relay ran")
	}

	broker := queue.NewMemory()
	relay := workflow.NewOutboxRelay(database.NewJobPostgresStore(db), broker, time.Second, 10)
	n, err := relay.Flush(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("flushed %d, err = %v; want 1", n, err)
	}
	if !jobColumn[sql.NullTime](t, db, jobA, "published_at").Valid {
		t.Fatal("the relay did not mark the job published")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := broker.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Message(), result.Message) {
		t.Fatalf("relayed %+v, want the submitted message %+v", d.Message(), result.Message)
	}

	if n, err := relay.Flush(context.Background()); err != nil || n != 0 {
		t.Fatalf("second flush published %d, err = %v; want nothing", n, err)
	}
	if ready, _, _ := broker.Counts(); ready != 0 {
		t.Fatalf("%d message(s) relayed twice", ready)
	}
}
