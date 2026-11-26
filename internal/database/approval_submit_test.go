package database_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func TestListGroupGrantsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewApprovalPostgresStore(db)
	editGroup, editLeader := insertGroupWithLeader(t, db, "editors")
	viewGroup, viewLeader := insertGroupWithLeader(t, db, "viewers")
	var noLeader int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ('leaderless') RETURNING id`).Scan(&noLeader); err != nil {
		t.Fatal(err)
	}
	otherDB := insertConnection(t, db, "other target")
	for _, g := range []struct {
		group, database int64
		level           string
	}{
		{editGroup, f.databaseID, "VIEW_EDIT"},
		{viewGroup, f.databaseID, "VIEW_ONLY"},
		{noLeader, f.databaseID, "VIEW_EDIT"},
		{editGroup, otherDB, "VIEW_ONLY"},
	} {
		execSQL(t, db, `INSERT INTO group_database_access (group_id, database_id, access_level) VALUES ($1, $2, $3)`, g.group, g.database, g.level)
	}

	got, err := store.ListGroupGrants(ctx, f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := []workflow.GroupGrant{
		{GroupID: editGroup, DatabaseID: f.databaseID, LeaderID: editLeader, Level: authorization.AccessViewEdit},
		{GroupID: viewGroup, DatabaseID: f.databaseID, LeaderID: viewLeader, Level: authorization.AccessViewOnly},
		{GroupID: noLeader, DatabaseID: f.databaseID, LeaderID: 0, Level: authorization.AccessViewEdit},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grants = %+v, want %+v", got, want)
	}
}

func TestSubmitForReviewPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewApprovalPostgresStore(db)
	groupA, leaderA := insertGroupWithLeader(t, db, "group-a")
	groupB, leaderB := insertGroupWithLeader(t, db, "group-b")
	approvers := []workflow.RequiredApprover{{GroupID: groupA, LeaderID: leaderA}, {GroupID: groupB, LeaderID: leaderB}}
	revisionState := func(id int64) (string, bool) {
		var status string
		var locked bool
		if err := db.QueryRowContext(ctx, `SELECT status, locked_at IS NOT NULL FROM migration_revisions WHERE id = $1`, id).Scan(&status, &locked); err != nil {
			t.Fatal(err)
		}
		return status, locked
	}
	countReviews := func(revision int64) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM review_requests WHERE revision_id = $1`, revision).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	broken := f.insertRevision(t, db, 1, false)
	if _, err := store.SubmitForReview(ctx, f.databaseID, broken, f.userID, []workflow.RequiredApprover{{GroupID: groupA, LeaderID: leaderA}, {GroupID: groupB, LeaderID: leaderB + 1000}}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("approver with an unknown leader: err = %v, want not found", err)
	}
	if status, locked := revisionState(broken); status != "DRAFT" || locked || countReviews(broken) != 0 {
		t.Fatalf("failed submit left status=%s locked=%v reviews=%d; want everything rolled back", status, locked, countReviews(broken))
	}

	revision := f.insertRevision(t, db, 2, false)
	review, err := store.SubmitForReview(ctx, f.databaseID, revision, f.userID, approvers)
	if err != nil {
		t.Fatal(err)
	}
	if review.ID == 0 || review.RevisionID != revision || review.Status != domain.ReviewStatusPending ||
		review.RequestedBy != f.userID || review.DefinitionHash != selectOneHash {
		t.Fatalf("review = %+v", review)
	}
	if status, locked := revisionState(revision); status != "IN_REVIEW" || !locked {
		t.Fatalf("revision status=%s locked=%v, want IN_REVIEW and locked", status, locked)
	}
	decisions, err := store.ListDecisions(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want 2", decisions)
	}
	for i, d := range decisions {
		if d.GroupID != approvers[i].GroupID || d.LeaderID != approvers[i].LeaderID || d.Status != domain.ApprovalStatusPending || d.DecidedAt != nil {
			t.Errorf("decision %d = %+v, want pending for %+v", i, d, approvers[i])
		}
	}

	if _, err := store.SubmitForReview(ctx, f.databaseID, revision, f.userID, approvers); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("resubmitting: err = %v, want conflict", err)
	}
	if _, err := store.SubmitForReview(ctx, f.databaseID+1000, f.insertRevision(t, db, 3, false), f.userID, approvers); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("revision of another database: err = %v, want not found", err)
	}
	if _, err := store.SubmitForReview(ctx, f.databaseID, f.insertRevision(t, db, 4, false), f.userID, nil); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatalf("no approvers: err = %v, want invalid input", err)
	}
}
