package admin

import (
	"context"
	"errors"
	"testing"
)

func TestSetGroupLeader(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	svc := NewService(store, nil)

	leaderID := int64(5)
	if err := svc.SetGroupLeader(ctx, adminUser, 1, &leaderID); err != nil {
		t.Fatalf("assign leader: %v", err)
	}
	if err := svc.SetGroupLeader(ctx, adminUser, 1, nil); err != nil {
		t.Fatalf("clear leader: %v", err)
	}

	if err := svc.SetGroupLeader(ctx, plainUser, 1, &leaderID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin: err = %v, want ErrForbidden", err)
	}
	if err := svc.SetGroupLeader(ctx, frozenAdmn, 1, &leaderID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("inactive admin: err = %v, want ErrForbidden", err)
	}
	if err := svc.SetGroupLeader(ctx, adminUser, 0, &leaderID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid group id: err = %v, want ErrInvalidInput", err)
	}
	badLeader := int64(0)
	if err := svc.SetGroupLeader(ctx, adminUser, 1, &badLeader); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid leader id: err = %v, want ErrInvalidInput", err)
	}
}
