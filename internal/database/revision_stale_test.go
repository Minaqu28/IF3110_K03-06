package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func TestNewRevisionStalesOlderReviewsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	revisions := database.NewRevisionPostgresStore(db)
	approvals := database.NewApprovalPostgresStore(db)
	groupA, leaderA := insertGroupWithLeader(t, db, "group-a")
	groupB, leaderB := insertGroupWithLeader(t, db, "group-b")
	approvers := []workflow.RequiredApprover{{GroupID: groupA, LeaderID: leaderA}, {GroupID: groupB, LeaderID: leaderB}}

	var otherMigration int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO migrations (database_id, version, sql) VALUES ($1, 2, 'SELECT 2') RETURNING id`, f.databaseID).Scan(&otherMigration); err != nil {
		t.Fatal(err)
	}
	newRevision := func(migration int64) domain.MigrationRevision {
		rev, err := revisions.CreateRevision(ctx, domain.MigrationRevision{MigrationID: migration, DatabaseID: f.databaseID, UpSQL: "SELECT 1", IrreversibleReason: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	submit := func(rev domain.MigrationRevision) domain.ReviewRequest {
		review, err := approvals.SubmitForReview(ctx, f.databaseID, rev.ID, f.userID, approvers)
		if err != nil {
			t.Fatal(err)
		}
		return review
	}
	approve := func(review domain.ReviewRequest, leader int64) {
		if _, err := approvals.DecideReview(ctx, review.ID, leader, domain.ApprovalStatusApproved, "", workflow.ResolveReview); err != nil {
			t.Fatal(err)
		}
	}
	reviewStatus := func(id int64) (string, bool) {
		var status string
		var resolved bool
		if err := db.QueryRowContext(ctx, `SELECT status, resolved_at IS NOT NULL FROM review_requests WHERE id = $1`, id).Scan(&status, &resolved); err != nil {
			t.Fatal(err)
		}
		return status, resolved
	}

	approvedReview := submit(newRevision(f.migrationID))
	approve(approvedReview, leaderA)
	approve(approvedReview, leaderB)
	openReview := submit(newRevision(f.migrationID))
	approve(openReview, leaderA)
	otherReview := submit(newRevision(otherMigration))

	third := newRevision(f.migrationID)
	if third.Revision != 3 {
		t.Fatalf("new revision number = %d, want 3", third.Revision)
	}

	if status, resolved := reviewStatus(openReview.ID); status != "STALE" || !resolved {
		t.Fatalf("open review on revision 2 = %s resolved=%v, want STALE and resolved", status, resolved)
	}
	decisions, err := approvals.ListDecisions(ctx, openReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if d.Status != domain.ApprovalStatusStale {
			t.Errorf("decision %+v, want STALE", d)
		}
		if (d.LeaderID == leaderA) != (d.DecidedAt != nil) {
			t.Errorf("decision %+v: only leader A's earlier approval should keep decided_at", d)
		}
	}
	if _, err := approvals.DecideReview(ctx, openReview.ID, leaderB, domain.ApprovalStatusApproved, "", workflow.ResolveReview); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("deciding a stale review: err = %v, want conflict", err)
	}

	if status, _ := reviewStatus(approvedReview.ID); status != "APPROVED" {
		t.Fatalf("resolved review on revision 1 = %s, want it left APPROVED", status)
	}
	if kept, err := approvals.ListDecisions(ctx, approvedReview.ID); err != nil || kept[0].Status != domain.ApprovalStatusApproved || kept[1].Status != domain.ApprovalStatusApproved {
		t.Fatalf("decisions of the approved review = %+v, %v", kept, err)
	}
	if status, _ := reviewStatus(otherReview.ID); status != "PENDING" {
		t.Fatalf("review of another migration = %s, want PENDING", status)
	}

	if review := submit(third); review.Status != domain.ReviewStatusPending {
		t.Fatalf("review of the new revision = %+v", review)
	}
}
