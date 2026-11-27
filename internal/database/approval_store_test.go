package database_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

func insertGroupWithLeader(t *testing.T, db *sql.DB, name string) (groupID, leaderID int64) {
	t.Helper()
	leaderID = insertUser(t, db, name+"-leader", "leader-long-password", false).ID
	return insertGroupLedBy(t, db, name, leaderID), leaderID
}

func insertGroupLedBy(t *testing.T, db *sql.DB, name string, leaderID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var groupID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id`, name).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, leaderID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE groups SET leader_id = $2 WHERE id = $1`, groupID, leaderID); err != nil {
		t.Fatal(err)
	}
	return groupID
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
	groupC := insertGroupLedBy(t, db, "group-c", leaderA)
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
