package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type recordingAccess struct {
	level authorization.AccessLevel
	err   error
	seen  []domain.User
}

func (r *recordingAccess) ConnectionWithAccess(_ context.Context, actor domain.User, _ int64) (domain.Connection, authorization.AccessLevel, error) {
	r.seen = append(r.seen, actor)
	return domain.Connection{}, r.level, r.err
}

type fakeRunState struct {
	actor domain.User
	state SubmitState
	err   error
	reqs  []domain.ExecutionRequest
}

func (f *fakeRunState) LoadRunState(_ context.Context, req domain.ExecutionRequest) (domain.User, SubmitState, error) {
	f.reqs = append(f.reqs, req)
	return f.actor, f.state, f.err
}

func TestAuthorizeExecution(t *testing.T) {
	queued := func() *fakeRunState {
		s := baseState()
		s.Executions = []ExecutionSummary{exec("x", 0, domain.MigrationActionApply, domain.ExecutionStatusPending)}
		return &fakeRunState{actor: domain.User{ID: 1, IsActive: true, Username: "fresh"}, state: s}
	}
	req := request(domain.MigrationActionApply, "")
	req.ExecutionID = "x"

	t.Run("authorized with the freshly loaded user", func(t *testing.T) {
		states, access := queued(), &recordingAccess{level: authorization.AccessViewEdit}
		if err := NewExecutionAuthorizer(access, states).AuthorizeExecution(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(access.seen) != 1 || access.seen[0].Username != "fresh" || len(states.reqs) != 1 {
			t.Fatalf("access saw %+v, state loads %d", access.seen, len(states.reqs))
		}
	})

	boom := errors.New("boom")
	cases := []struct {
		name   string
		req    func() domain.ExecutionRequest
		states func() *fakeRunState
		access *recordingAccess
		want   error
	}{
		{"user or execution gone", nil, func() *fakeRunState { s := queued(); s.err = admin.ErrNotFound; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrForbidden},
		{"state load fails", nil, func() *fakeRunState { s := queued(); s.err = boom; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, boom},
		{"edit access revoked", nil, queued, &recordingAccess{level: authorization.AccessViewOnly}, admin.ErrForbidden},
		{"user deactivated", nil, queued, &recordingAccess{err: admin.ErrForbidden}, admin.ErrForbidden},
		{"rules refuse", nil, func() *fakeRunState { s := queued(); s.state.LatestRevision = 3; return s },
			&recordingAccess{level: authorization.AccessViewEdit}, ErrNotLatestRevision},
		{"no execution id", func() domain.ExecutionRequest { r := req; r.ExecutionID = ""; return r }, queued,
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrInvalidInput},
		{"no actor", func() domain.ExecutionRequest { r := req; r.ActorID = 0; return r }, queued,
			&recordingAccess{level: authorization.AccessViewEdit}, admin.ErrInvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req
			if c.req != nil {
				r = c.req()
			}
			if err := NewExecutionAuthorizer(c.access, c.states()).AuthorizeExecution(context.Background(), r); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
