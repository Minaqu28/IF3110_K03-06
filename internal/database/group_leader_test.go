package database_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
)

func groupLeader(t *testing.T, db *sql.DB, groupID int64) *int64 {
	t.Helper()
	var leader sql.NullInt64
	if err := db.QueryRowContext(context.Background(), `SELECT leader_id FROM groups WHERE id = $1`, groupID).Scan(&leader); err != nil {
		t.Fatal(err)
	}
	if !leader.Valid {
		return nil
	}
	return &leader.Int64
}

func isMember(t *testing.T, db *sql.DB, groupID, userID int64) bool {
	t.Helper()
	var member bool
	if err := db.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2)`, groupID, userID).Scan(&member); err != nil {
		t.Fatal(err)
	}
	return member
}

func TestGroupLeaderMustBeMemberPostgres(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	store, err := database.NewAdminPostgresStore(db, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	lead := insertUser(t, db, "lead", "lead-long-password", false)
	outsider := insertUser(t, db, "outsider", "outsider-long-password", false)
	group, err := store.CreateGroup(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetGroupLeader(ctx, group.ID, &lead.ID); !errors.Is(err, admin.ErrLeaderNotMember) || !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("non-member as leader: err = %v, want ErrLeaderNotMember", err)
	}
	if groupLeader(t, db, group.ID) != nil {
		t.Fatal("a non-member became the leader")
	}
	if err := store.SetGroupLeader(ctx, group.ID+1000, &lead.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown group: err = %v, want not found", err)
	}

	if err := store.AddMember(ctx, group.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupLeader(ctx, group.ID, &lead.ID); err != nil {
		t.Fatalf("member as leader: %v", err)
	}
	if err := store.RemoveMember(ctx, group.ID, lead.ID); !errors.Is(err, admin.ErrMemberIsLeader) || !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("removing the leader: err = %v, want ErrMemberIsLeader", err)
	}
	if !isMember(t, db, group.ID, lead.ID) {
		t.Fatal("the leader was removed from the group")
	}

	_, err = db.ExecContext(ctx, `UPDATE groups SET leader_id = $2 WHERE id = $1`, group.ID, outsider.ID)
	requireSQLState(t, err, "23503")

	if _, err := db.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, group.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if groupLeader(t, db, group.ID) != nil {
		t.Fatal("deleting the leader's membership left them as leader")
	}

	if err := store.AddMember(ctx, group.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupLeader(ctx, group.ID, &lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupLeader(ctx, group.ID, nil); err != nil {
		t.Fatalf("clearing the leader: %v", err)
	}
	if err := store.RemoveMember(ctx, group.ID, lead.ID); err != nil {
		t.Fatalf("removing a former leader: %v", err)
	}

	if err := store.AddMember(ctx, group.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupLeader(ctx, group.ID, &lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(ctx, lead.ID); err != nil {
		t.Fatalf("deleting the leader's account: %v", err)
	}
	if groupLeader(t, db, group.ID) != nil {
		t.Fatal("a deleted user is still the leader")
	}
	if err := store.DeleteGroup(ctx, group.ID); err != nil {
		t.Fatalf("deleting the group: %v", err)
	}
}

func TestLeaderMembershipMigrationKeepsExistingLeadersPostgres(t *testing.T) {
	db := isolatedDB(t, false)
	ctx := context.Background()
	before := fstest.MapFS{}
	names, err := fs.Glob(migration.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "012" {
			continue
		}
		body, err := fs.ReadFile(migration.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = &fstest.MapFile{Data: body}
	}
	if err := database.Migrate(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	lead := insertUser(t, db, "old-lead", "old-lead-long-password", false)
	var groupID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name, leader_id) VALUES ('legacy', $1) RETURNING id`, lead.ID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}

	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		t.Fatal(err)
	}
	if !isMember(t, db, groupID, lead.ID) {
		t.Fatal("the migration did not add the existing leader as a member")
	}
	if got := groupLeader(t, db, groupID); got == nil || *got != lead.ID {
		t.Fatalf("leader after migration = %v, want %d", got, lead.ID)
	}
}
