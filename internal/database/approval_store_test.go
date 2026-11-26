package database_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func insertGroupWithLeader(t *testing.T, db *sql.DB, name string) (groupID, leaderID int64) {
	t.Helper()
	leaderID = insertUser(t, db, name+"-leader", "leader-long-password", false).ID
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO groups (name, leader_id) VALUES ($1, $2) RETURNING id`, name, leaderID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	return groupID, leaderID
}

func TestApprovalStoreCreateReviewPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewApprovalPostgresStore(db)
	locked := f.insertRevision(t, db, 1, true)
	draft := f.insertRevision(t, db, 2, false)

	review, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: locked, DefinitionHash: selectOneHash, Status: domain.ReviewStatusApproved, RequestedBy: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	if review.ID == 0 || review.Status != domain.ReviewStatusPending || review.RequestedAt.IsZero() ||
		review.RequestedBy != f.userID || review.ResolvedAt != nil || review.DefinitionHash != selectOneHash {
		t.Fatalf("review = %+v", review)
	}
	if _, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: locked, DefinitionHash: selectOneHash}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("second pending review: err = %v, want conflict", err)
	}
	if _, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: draft, DefinitionHash: selectOneHash}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("review of an unlocked draft: err = %v, want conflict", err)
	}
	other := f.insertRevision(t, db, 3, true)
	if _, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: other, DefinitionHash: "stale"}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("hash mismatch: err = %v, want conflict", err)
	}
	if _, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: other + 1000, DefinitionHash: selectOneHash}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown revision: err = %v, want not found", err)
	}
	decisions, err := store.ListDecisions(ctx, review.ID)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("decisions of a new review = %+v, err = %v", decisions, err)
	}
}

func TestApprovalStoreSaveDecisionPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewApprovalPostgresStore(db)
	review, err := store.CreateReview(ctx, domain.ReviewRequest{RevisionID: f.insertRevision(t, db, 1, true), DefinitionHash: selectOneHash})
	if err != nil {
		t.Fatal(err)
	}
	groupA, leaderA := insertGroupWithLeader(t, db, "group-a")
	groupB, leaderB := insertGroupWithLeader(t, db, "group-b")

	for _, d := range []domain.ApprovalDecision{
		{ReviewID: review.ID, GroupID: groupB, LeaderID: leaderB, Status: domain.ApprovalStatusPending},
		{ReviewID: review.ID, GroupID: groupA, LeaderID: leaderA, Status: domain.ApprovalStatusPending},
	} {
		saved, err := store.SaveDecision(ctx, d)
		if err != nil || saved.ID == 0 || saved.DecidedAt != nil {
			t.Fatalf("pending decision = %+v, err = %v", saved, err)
		}
	}
	approved, err := store.SaveDecision(ctx, domain.ApprovalDecision{ReviewID: review.ID, GroupID: groupA, LeaderID: leaderA, Status: domain.ApprovalStatusApproved, Comment: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != domain.ApprovalStatusApproved || approved.DecidedAt == nil || approved.Comment != "ok" {
		t.Fatalf("approved = %+v", approved)
	}

	decisions, err := store.ListDecisions(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].GroupID != groupA || decisions[1].GroupID != groupB ||
		decisions[0].ID != approved.ID || decisions[1].Status != domain.ApprovalStatusPending {
		t.Fatalf("decisions = %+v", decisions)
	}

	if _, err := db.ExecContext(ctx, `UPDATE review_requests SET status = 'REJECTED', resolved_at = now() WHERE id = $1`, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveDecision(ctx, domain.ApprovalDecision{ReviewID: review.ID, GroupID: groupB, LeaderID: leaderB, Status: domain.ApprovalStatusApproved}); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("decision on a resolved review: err = %v, want conflict", err)
	}
	stale, err := store.SaveDecision(ctx, domain.ApprovalDecision{ReviewID: review.ID, GroupID: groupA, LeaderID: leaderA, Status: domain.ApprovalStatusStale})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != domain.ApprovalStatusStale || stale.DecidedAt == nil || !stale.DecidedAt.Equal(*approved.DecidedAt) {
		t.Fatalf("stale decision lost its decided_at: %+v", stale)
	}
	if _, err := store.SaveDecision(ctx, domain.ApprovalDecision{ReviewID: review.ID + 1000, GroupID: groupA, LeaderID: leaderA, Status: domain.ApprovalStatusPending}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown review: err = %v, want not found", err)
	}
}
