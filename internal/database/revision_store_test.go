package database_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestRevisionStoreReadsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewRevisionPostgresStore(db)

	const upSQL = "UPDATE public.ördërs SET total = 0\r\nWHERE id > $1 AND id <= $2;  \n"
	const downSQL = "UPDATE public.ördërs SET total = NULL WHERE id > $1 AND id <= $2"
	batch := &domain.BatchConfig{Table: "public.ördërs", CursorColumn: "id", BatchSize: 500}
	hash := domain.ComputeDefinitionHash(domain.MigrationRevision{UpSQL: upSQL, DownSQL: downSQL, Reversible: true, Batch: batch})
	var batchID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO migration_revisions
		    (migration_id, database_id, revision, title, up_sql, down_sql, reversible, status, definition_hash,
		     batch_table, batch_cursor_column, batch_size, created_by, locked_at)
		VALUES ($1, $2, 2, 'backfill', $3, $4, TRUE, 'APPROVED', $5, $6, $7, $8, $9, now()) RETURNING id`,
		f.migrationID, f.databaseID, upSQL, downSQL, hash, batch.Table, batch.CursorColumn, batch.BatchSize, f.userID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	normalID := f.insertRevision(t, db, 1, false)

	got, err := store.GetRevision(ctx, f.databaseID, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpSQL != upSQL || got.DownSQL != downSQL || got.DefinitionHash != hash {
		t.Fatalf("revision SQL or hash changed: %+v", got)
	}
	if got.ID != batchID || got.MigrationID != f.migrationID || got.DatabaseID != f.databaseID || got.Revision != 2 ||
		got.Title != "backfill" || !got.Reversible || got.Status != domain.RevisionStatusApproved ||
		got.CreatedBy != f.userID || got.CreatedAt.IsZero() || got.LockedAt == nil {
		t.Fatalf("revision fields = %+v", got)
	}
	if !reflect.DeepEqual(got.Batch, batch) {
		t.Fatalf("batch = %+v, want %+v", got.Batch, batch)
	}

	normal, err := store.GetRevision(ctx, f.databaseID, normalID)
	if err != nil {
		t.Fatal(err)
	}
	if normal.Batch != nil || normal.DownSQL != "" || normal.IrreversibleReason != "test" || normal.LockedAt != nil {
		t.Fatalf("normal revision = %+v", normal)
	}

	if _, err := store.GetRevision(ctx, f.databaseID+1000, batchID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("revision from another database: err = %v, want not found", err)
	}

	list, err := store.ListRevisions(ctx, f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != normalID || list[1].ID != batchID {
		t.Fatalf("list = %+v, want revision 1 then 2", list)
	}
	empty, err := store.ListRevisions(ctx, f.databaseID+1000)
	if err != nil || len(empty) != 0 {
		t.Fatalf("other database list = %+v, err = %v", empty, err)
	}
}

func TestRevisionStoreCreatePostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewRevisionPostgresStore(db)

	in := domain.MigrationRevision{
		MigrationID: f.migrationID, DatabaseID: f.databaseID, Title: "first",
		UpSQL: "UPDATE t SET x = 1 WHERE id > $1 AND id <= $2", DownSQL: "UPDATE t SET x = 0 WHERE id > $1 AND id <= $2",
		Reversible: true, DefinitionHash: "ignored", CreatedBy: f.userID,
		Batch: &domain.BatchConfig{Table: "public.t", CursorColumn: "id", BatchSize: 100},
	}
	first, err := store.CreateRevision(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.Status != domain.RevisionStatusDraft || first.LockedAt != nil ||
		first.DefinitionHash != domain.ComputeDefinitionHash(in) || !reflect.DeepEqual(first.Batch, in.Batch) {
		t.Fatalf("first = %+v", first)
	}
	reread, err := store.GetRevision(ctx, f.databaseID, first.ID)
	if err != nil || !reflect.DeepEqual(reread, first) {
		t.Fatalf("reread = %+v, err = %v, want %+v", reread, err, first)
	}

	second, err := store.CreateRevision(ctx, domain.MigrationRevision{
		MigrationID: f.migrationID, DatabaseID: f.databaseID, UpSQL: "SELECT 1", IrreversibleReason: "read only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.Batch != nil || second.CreatedBy != 0 {
		t.Fatalf("second = %+v", second)
	}

	wrongDB := in
	wrongDB.DatabaseID = f.databaseID + 1000
	if _, err := store.CreateRevision(ctx, wrongDB); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("migration of another database: err = %v, want not found", err)
	}
	noDown := in
	noDown.DownSQL = ""
	if _, err := store.CreateRevision(ctx, noDown); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("reversible without down SQL: err = %v, want invalid input", err)
	}
}

func TestRevisionStoreLockPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewRevisionPostgresStore(db)

	draft, err := store.CreateRevision(ctx, domain.MigrationRevision{
		MigrationID: f.migrationID, DatabaseID: f.databaseID, UpSQL: "SELECT 1", IrreversibleReason: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_revisions SET up_sql = 'SELECT 2' WHERE id = $1`, draft.ID); err != nil {
		t.Fatal(err)
	}

	locked, err := store.LockRevision(ctx, f.databaseID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if locked.Status != domain.RevisionStatusInReview || locked.LockedAt == nil || locked.UpSQL != "SELECT 2" ||
		locked.DefinitionHash != domain.ComputeDefinitionHash(domain.MigrationRevision{UpSQL: "SELECT 2"}) {
		t.Fatalf("locked = %+v", locked)
	}
	if _, err := store.LockRevision(ctx, f.databaseID, draft.ID); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("second lock: err = %v, want conflict", err)
	}
	if _, err := store.LockRevision(ctx, f.databaseID+1000, draft.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("lock in another database: err = %v, want not found", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_revisions SET up_sql = 'SELECT 3' WHERE id = $1`, draft.ID); err == nil {
		t.Fatal("locked revision SQL changed after LockRevision")
	}
}
