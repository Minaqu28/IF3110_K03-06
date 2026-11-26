package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeAccess struct {
	level authorization.AccessLevel
	err   error
}

func (f fakeAccess) ConnectionWithAccess(context.Context, domain.User, int64) (domain.Connection, authorization.AccessLevel, error) {
	return domain.Connection{}, f.level, f.err
}

type fakeGrants struct {
	grants []GroupGrant
	err    error
}

func (f fakeGrants) ListGroupGrants(context.Context, int64) ([]GroupGrant, error) {
	return f.grants, f.err
}

type submitCall struct {
	databaseID, revisionID, requestedBy int64
	approvers                           []RequiredApprover
}

type fakeSubmitter struct {
	calls  []submitCall
	review domain.ReviewRequest
	err    error
}

func (f *fakeSubmitter) SubmitForReview(_ context.Context, databaseID, revisionID, requestedBy int64, approvers []RequiredApprover) (domain.ReviewRequest, error) {
	f.calls = append(f.calls, submitCall{databaseID, revisionID, requestedBy, approvers})
	return f.review, f.err
}

func TestSubmitForReview(t *testing.T) {
	ctx := context.Background()
	actor := domain.User{ID: 42}
	editGrants := []GroupGrant{
		{GroupID: 2, DatabaseID: 7, LeaderID: 20, Level: authorization.AccessViewEdit},
		{GroupID: 1, DatabaseID: 7, LeaderID: 10, Level: authorization.AccessViewEdit},
		{GroupID: 3, DatabaseID: 7, LeaderID: 30, Level: authorization.AccessViewOnly},
	}

	t.Run("locks through the submitter with every required approver", func(t *testing.T) {
		sub := &fakeSubmitter{review: domain.ReviewRequest{ID: 5, Status: domain.ReviewStatusPending}}
		svc := NewReviewService(fakeAccess{level: authorization.AccessViewEdit}, fakeGrants{grants: editGrants}, sub, nil)
		got, err := svc.SubmitForReview(ctx, actor, 7, 99)
		if err != nil || got.ID != 5 {
			t.Fatalf("review = %+v, err = %v", got, err)
		}
		want := []submitCall{{7, 99, 42, []RequiredApprover{{1, 10}, {2, 20}}}}
		if !reflect.DeepEqual(sub.calls, want) {
			t.Fatalf("submitter calls = %+v, want %+v", sub.calls, want)
		}
	})

	boom := errors.New("boom")
	refused := []struct {
		name   string
		access fakeAccess
		grants fakeGrants
		want   error
	}{
		{"view-only actor", fakeAccess{level: authorization.AccessViewOnly}, fakeGrants{grants: editGrants}, admin.ErrForbidden},
		{"no access", fakeAccess{level: authorization.AccessNone}, fakeGrants{grants: editGrants}, admin.ErrForbidden},
		{"access lookup fails", fakeAccess{err: admin.ErrNotFound}, fakeGrants{grants: editGrants}, admin.ErrNotFound},
		{"grant lookup fails", fakeAccess{level: authorization.AccessViewEdit}, fakeGrants{err: boom}, boom},
		{"nobody can approve", fakeAccess{level: authorization.AccessViewEdit}, fakeGrants{grants: editGrants[2:]}, ErrNoApprovers},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			sub := &fakeSubmitter{}
			_, err := NewReviewService(c.access, c.grants, sub, nil).SubmitForReview(ctx, actor, 7, 99)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if len(sub.calls) != 0 {
				t.Fatalf("revision was submitted anyway: %+v", sub.calls)
			}
		})
	}

	t.Run("submitter error is returned", func(t *testing.T) {
		sub := &fakeSubmitter{err: admin.ErrConflict}
		_, err := NewReviewService(fakeAccess{level: authorization.AccessViewEdit}, fakeGrants{grants: editGrants}, sub, nil).SubmitForReview(ctx, actor, 7, 99)
		if !errors.Is(err, admin.ErrConflict) {
			t.Fatalf("err = %v, want conflict", err)
		}
	})
}

type decideCall struct {
	reviewID, leaderID int64
	status             domain.ApprovalStatus
	comment            string
	resolve            func([]domain.ApprovalDecision) domain.ReviewStatus
}

type fakeDecider struct {
	calls []decideCall
}

func (f *fakeDecider) DecideReview(_ context.Context, reviewID, leaderID int64, status domain.ApprovalStatus, comment string, resolve func([]domain.ApprovalDecision) domain.ReviewStatus) (domain.ReviewRequest, error) {
	f.calls = append(f.calls, decideCall{reviewID, leaderID, status, comment, resolve})
	return domain.ReviewRequest{ID: reviewID, Status: domain.ReviewStatusPending}, nil
}

func TestDecideApproval(t *testing.T) {
	ctx := context.Background()
	leader := domain.User{ID: 10, IsActive: true}

	for _, status := range []domain.ApprovalStatus{domain.ApprovalStatusApproved, domain.ApprovalStatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			dec := &fakeDecider{}
			got, err := NewReviewService(nil, nil, nil, dec).DecideApproval(ctx, leader, 5, status, "looks fine")
			if err != nil || got.ID != 5 || len(dec.calls) != 1 {
				t.Fatalf("review = %+v, err = %v, calls = %d", got, err, len(dec.calls))
			}
			c := dec.calls[0]
			if c.reviewID != 5 || c.leaderID != 10 || c.status != status || c.comment != "looks fine" || c.resolve == nil {
				t.Fatalf("call = %+v", c)
			}
			if c.resolve([]domain.ApprovalDecision{{Status: domain.ApprovalStatusApproved}}) != domain.ReviewStatusApproved {
				t.Fatal("decider was not given ResolveReview")
			}
		})
	}

	refused := []struct {
		name   string
		actor  domain.User
		status domain.ApprovalStatus
		want   error
	}{
		{"pending is not a decision", leader, domain.ApprovalStatusPending, admin.ErrInvalidInput},
		{"stale is not a decision", leader, domain.ApprovalStatusStale, admin.ErrInvalidInput},
		{"unknown status", leader, domain.ApprovalStatus("MAYBE"), admin.ErrInvalidInput},
		{"inactive user", domain.User{ID: 10}, domain.ApprovalStatusApproved, admin.ErrForbidden},
		{"no user", domain.User{IsActive: true}, domain.ApprovalStatusApproved, admin.ErrForbidden},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			dec := &fakeDecider{}
			if _, err := NewReviewService(nil, nil, nil, dec).DecideApproval(ctx, c.actor, 5, c.status, ""); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if len(dec.calls) != 0 {
				t.Fatal("decider was called for a refused decision")
			}
		})
	}
}
