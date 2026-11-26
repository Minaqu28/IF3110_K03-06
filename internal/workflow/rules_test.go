package workflow

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestRequiredGroups(t *testing.T) {
	const db = 7
	edit, view := authorization.AccessViewEdit, authorization.AccessViewOnly
	cases := []struct {
		name   string
		grants []GroupGrant
		want   []RequiredApprover
		err    error
	}{
		{
			name: "every edit group with a leader, sorted by group",
			grants: []GroupGrant{
				{GroupID: 30, DatabaseID: db, LeaderID: 300, Level: edit},
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: edit},
				{GroupID: 20, DatabaseID: db, LeaderID: 200, Level: edit},
			},
			want: []RequiredApprover{{10, 100}, {20, 200}, {30, 300}},
		},
		{
			name: "view-only and view-edit groups are both required",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: edit},
				{GroupID: 20, DatabaseID: db, LeaderID: 200, Level: view},
			},
			want: []RequiredApprover{{10, 100}, {20, 200}},
		},
		{
			name: "group with access but without a leader is rejected",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db, LeaderID: 0, Level: edit},
				{GroupID: 20, DatabaseID: db, LeaderID: 200, Level: edit},
			},
			err: ErrGroupMissingLeader,
		},
		{
			name: "grants on other databases are ignored",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db + 1, LeaderID: 100, Level: edit},
				{GroupID: 20, DatabaseID: db, LeaderID: 200, Level: edit},
			},
			want: []RequiredApprover{{20, 200}},
		},
		{
			name: "a group listed twice is required once",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: edit},
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: edit},
			},
			want: []RequiredApprover{{10, 100}},
		},
		{
			name: "one leader of two groups approves once per group",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: edit},
				{GroupID: 20, DatabaseID: db, LeaderID: 100, Level: edit},
			},
			want: []RequiredApprover{{10, 100}, {20, 100}},
		},
		{
			name: "unknown access level is not edit",
			grants: []GroupGrant{
				{GroupID: 10, DatabaseID: db, LeaderID: 100, Level: authorization.AccessLevel("ADMIN")},
			},
			err: ErrNoApprovers,
		},
		{
			name:   "view-only group without a leader is rejected",
			grants: []GroupGrant{{GroupID: 10, DatabaseID: db, LeaderID: 0, Level: view}},
			err:    ErrGroupMissingLeader,
		},
		{name: "no grants", err: ErrNoApprovers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RequiredGroups(db, c.grants)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestResolveReview(t *testing.T) {
	d := func(statuses ...domain.ApprovalStatus) []domain.ApprovalDecision {
		var out []domain.ApprovalDecision
		for _, s := range statuses {
			out = append(out, domain.ApprovalDecision{Status: s})
		}
		return out
	}
	approved, rejected, pending, stale := domain.ApprovalStatusApproved, domain.ApprovalStatusRejected, domain.ApprovalStatusPending, domain.ApprovalStatusStale
	cases := []struct {
		name      string
		decisions []domain.ApprovalDecision
		want      domain.ReviewStatus
	}{
		{"all approved", d(approved, approved), domain.ReviewStatusApproved},
		{"single approval", d(approved), domain.ReviewStatusApproved},
		{"one still pending", d(approved, pending), domain.ReviewStatusPending},
		{"one rejection wins over approvals", d(approved, rejected, approved), domain.ReviewStatusRejected},
		{"rejection wins over pending", d(pending, rejected), domain.ReviewStatusRejected},
		{"stale never counts as approved", d(approved, stale), domain.ReviewStatusPending},
		{"no decisions never approve", nil, domain.ReviewStatusPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveReview(c.decisions); got != c.want {
				t.Fatalf("ResolveReview = %s, want %s", got, c.want)
			}
		})
	}
}
