package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
)

func newLockFixture(t *testing.T, db *sql.DB) (asyncFixture, string, string) {
	t.Helper()
	f, first := newJobFixture(t, db)
	var revision, review int64
	if err := db.QueryRowContext(context.Background(), `
		SELECT revision_id, approval_request_id FROM migration_executions WHERE id = $1`, first).Scan(&revision, &review); err != nil {
		t.Fatal(err)
	}
	second, err := f.insertExecution(db, revision, review, "APPLY", "PENDING", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, first, second
}

func insertConnection(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO database_connections (name, host, db_name, db_user, password_encrypted)
		VALUES ($1, 'localhost', 'target', 'target', $2) RETURNING id`, name, []byte{0}).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func expireLock(t *testing.T, db *sql.DB, databaseID int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE database_locks
		SET acquired_at = now() - interval '1 hour', lease_expires_at = now() - interval '1 minute'
		WHERE database_id = $1`, databaseID); err != nil {
		t.Fatal(err)
	}
}

func mustAcquire(t *testing.T, store *database.LockPostgresStore, databaseID int64, execution, owner string, want bool) int64 {
	t.Helper()
	token, ok, err := store.Acquire(context.Background(), databaseID, execution, owner)
	if err != nil || ok != want || (ok && token <= 0) || (!ok && token != 0) {
		t.Fatalf("Acquire(%d, %s) = %d, %v, %v; want acquired=%v", databaseID, owner, token, ok, err, want)
	}
	return token
}

func TestLockStoreAcquireReleasePostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	store := database.NewLockPostgresStore(db, 30*time.Second)

	t1 := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	mustAcquire(t, store, f.databaseID, first, "worker-b", false)
	t2 := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	if t2 <= t1 {
		t.Fatalf("re-acquire token %d is not newer than %d", t2, t1)
	}

	otherDB := insertConnection(t, db, "other target")
	mustAcquire(t, store, otherDB, second, "worker-b", true)

	if err := store.Release(ctx, f.databaseID, "worker-a", t1); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	if err := store.Release(ctx, f.databaseID, "worker-b", t2); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", false)
	if err := store.Release(ctx, f.databaseID, "worker-a", t2); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, store, f.databaseID, second, "worker-b", true)

	if _, _, err := store.Acquire(ctx, insertConnection(t, db, "third"), "00000000-0000-0000-0000-000000000000", "worker-a"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown execution: err = %v, want not found", err)
	}
	if _, _, err := store.Acquire(ctx, insertConnection(t, db, "fourth"), first, "  "); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("blank owner: err = %v, want invalid input", err)
	}
}

func TestLockStoreConcurrentAcquirePostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	store := database.NewLockPostgresStore(db, 30*time.Second)

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	start := make(chan struct{})
	for i := range workers {
		execution := first
		if i%2 == 1 {
			execution = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := store.Acquire(context.Background(), f.databaseID, execution, fmt.Sprintf("worker-%d", i))
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	acquired := 0
	for ok := range results {
		if ok {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("%d workers acquired the same database lock, want exactly 1", acquired)
	}
}

func TestLockStoreHeartbeatAndTakeoverPostgres(t *testing.T) {
	db := integrationDB(t)
	f, first, second := newLockFixture(t, db)
	ctx := context.Background()
	store := database.NewLockPostgresStore(db, 30*time.Second)
	leaseOf := func() time.Time {
		var lease time.Time
		if err := db.QueryRowContext(ctx, `SELECT lease_expires_at FROM database_locks WHERE database_id = $1`, f.databaseID).Scan(&lease); err != nil {
			t.Fatal(err)
		}
		return lease
	}

	old := mustAcquire(t, store, f.databaseID, first, "worker-a", true)
	before := leaseOf()
	time.Sleep(5 * time.Millisecond)
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || !held {
		t.Fatalf("holder heartbeat = %v, %v", held, err)
	}
	if after := leaseOf(); !after.After(before) {
		t.Fatalf("heartbeat did not extend the lease: %v -> %v", before, after)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old+1000); err != nil || held {
		t.Fatalf("wrong token heartbeat = %v, %v", held, err)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-b", old); err != nil || held {
		t.Fatalf("wrong owner heartbeat = %v, %v", held, err)
	}

	expireLock(t, db, f.databaseID)
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || held {
		t.Fatalf("heartbeat revived an expired lease: %v, %v", held, err)
	}
	fresh := mustAcquire(t, store, f.databaseID, second, "worker-b", true)
	if fresh <= old {
		t.Fatalf("takeover token %d is not newer than %d", fresh, old)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-a", old); err != nil || held {
		t.Fatalf("old owner still holds the lock after takeover: %v, %v", held, err)
	}
	if err := store.Release(ctx, f.databaseID, "worker-a", old); err != nil {
		t.Fatal(err)
	}
	if held, err := store.Heartbeat(ctx, f.databaseID, "worker-b", fresh); err != nil || !held {
		t.Fatalf("old owner's release removed the new lock: %v, %v", held, err)
	}
}
