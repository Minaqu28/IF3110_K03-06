package database_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func submitWithApprovers(t *testing.T, db *sql.DB, f asyncFixture, revision int, approvers []workflow.RequiredApprover) domain.ReviewRequest {
	t.Helper()
	review, err := database.NewApprovalPostgresStore(db).SubmitForReview(context.Background(), f.databaseID, f.insertRevision(t, db, revision, false), f.userID, approvers)
	if err != nil {
		t.Fatal(err)
	}
	return review
}

func revisionStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM migration_revisions WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestDecideReviewPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	store := database.NewApprovalPostgresStore(db)
	groupA, leaderA := insertGroupWithLeader(t, db, "group-a")
	groupB, leaderB := insertGroupWithLeader(t, db, "group-b")
	var groupC int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name, leader_id) VALUES ('group-c', $1) RETURNING id`, leaderA).Scan(&groupC); err != nil {
		t.Fatal(err)
	}
	approvers := []workflow.RequiredApprover{{GroupID: groupA, LeaderID: leaderA}, {GroupID: groupB, LeaderID: leaderB}, {GroupID: groupC, LeaderID: leaderA}}
	decide := func(review, leader int64, status domain.ApprovalStatus) (domain.ReviewRequest, error) {
		return store.DecideReview(ctx, review, leader, status, "note", workflow.ResolveReview)
	}

	review := submitWithApprovers(t, db, f, 1, approvers)
	if _, err := decide(review.ID, f.userID, domain.ApprovalStatusApproved); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("non-leader: err = %v, want forbidden", err)
	}
	got, err := decide(review.ID, leaderA, domain.ApprovalStatusApproved)
	if err != nil || got.Status != domain.ReviewStatusPending || got.ResolvedAt != nil {
		t.Fatalf("after leader A = %+v, %v; want still pending", got, err)
	}
	decisions, err := store.ListDecisions(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		wantStatus := domain.ApprovalStatusPending
		if d.LeaderID == leaderA {
			wantStatus = domain.ApprovalStatusApproved
		}
		if d.Status != wantStatus || (wantStatus == domain.ApprovalStatusApproved && (d.DecidedAt == nil || d.Comment != "note")) {
			t.Errorf("decision %+v, want %s", d, wantStatus)
		}
	}
	if s := revisionStatus(t, db, review.RevisionID); s != "IN_REVIEW" {
		t.Fatalf("revision status = %s, want IN_REVIEW", s)
	}
	if _, err := decide(review.ID, leaderA, domain.ApprovalStatusRejected); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("leader deciding twice: err = %v, want conflict", err)
	}
	got, err = decide(review.ID, leaderB, domain.ApprovalStatusApproved)
	if err != nil || got.Status != domain.ReviewStatusApproved || got.ResolvedAt == nil {
		t.Fatalf("after leader B = %+v, %v; want approved", got, err)
	}
	if s := revisionStatus(t, db, review.RevisionID); s != "APPROVED" {
		t.Fatalf("revision status = %s, want APPROVED", s)
	}
	if _, err := decide(review.ID, leaderB, domain.ApprovalStatusApproved); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("deciding a resolved review: err = %v, want conflict", err)
	}

	rejected := submitWithApprovers(t, db, f, 2, approvers)
	got, err = decide(rejected.ID, leaderB, domain.ApprovalStatusRejected)
	if err != nil || got.Status != domain.ReviewStatusRejected {
		t.Fatalf("rejection = %+v, %v", got, err)
	}
	if s := revisionStatus(t, db, rejected.RevisionID); s != "REJECTED" {
		t.Fatalf("revision status = %s, want REJECTED", s)
	}

	if _, err := decide(rejected.ID+1000, leaderA, domain.ApprovalStatusApproved); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown review: err = %v, want not found", err)
	}
}

func TestDecideReviewConcurrentApprovalsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	store := database.NewApprovalPostgresStore(db)
	var approvers []workflow.RequiredApprover
	for _, name := range []string{"g1", "g2", "g3", "g4"} {
		g, l := insertGroupWithLeader(t, db, name)
		approvers = append(approvers, workflow.RequiredApprover{GroupID: g, LeaderID: l})
	}
	review := submitWithApprovers(t, db, f, 1, approvers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, a := range approvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := store.DecideReview(context.Background(), review.ID, a.LeaderID, domain.ApprovalStatusApproved, "", workflow.ResolveReview); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM review_requests WHERE id = $1`, review.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "APPROVED" || revisionStatus(t, db, review.RevisionID) != "APPROVED" {
		t.Fatalf("after concurrent approvals review=%s revision=%s, want both APPROVED", status, revisionStatus(t, db, review.RevisionID))
	}
}
