package authorization

import (
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func TestAllows(t *testing.T) {
	cases := []struct {
		level, required AccessLevel
		want            bool
	}{
		{AccessViewEdit, AccessViewEdit, true},
		{AccessViewEdit, AccessViewOnly, true},
		{AccessViewOnly, AccessViewOnly, true},
		{AccessViewOnly, AccessViewEdit, false},
		{AccessNone, AccessViewOnly, false},
		{AccessViewEdit, AccessNone, false},
		{AccessLevel("ADMIN"), AccessViewOnly, false},
	}
	for _, c := range cases {
		if got := c.level.Allows(c.required); got != c.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", c.level, c.required, got, c.want)
		}
	}
}

func TestEffectiveAccessPicksHighestGrant(t *testing.T) {
	if got := EffectiveAccess(); got != AccessNone {
		t.Errorf("no grants = %q, want none", got)
	}
	if got := EffectiveAccess(AccessViewOnly, AccessViewEdit, AccessViewOnly); got != AccessViewEdit {
		t.Errorf("got %q, want VIEW_EDIT", got)
	}
	if got := EffectiveAccess(AccessViewOnly, "BOGUS"); got != AccessViewOnly {
		t.Errorf("got %q, want VIEW_ONLY", got)
	}
}

func TestCanManagePlatform(t *testing.T) {
	cases := []struct {
		user domain.User
		want bool
	}{
		{domain.User{IsActive: true, IsPlatformAdmin: true}, true},
		{domain.User{IsActive: false, IsPlatformAdmin: true}, false},
		{domain.User{IsActive: true, IsPlatformAdmin: false}, false},
	}
	for _, c := range cases {
		if got := CanManagePlatform(c.user); got != c.want {
			t.Errorf("CanManagePlatform(%+v) = %v, want %v", c.user, got, c.want)
		}
	}
}
