package authentication

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fresh := SessionState{
		UserActive:        true,
		RotatedAt:         now.Add(-time.Minute),
		LastSeenAt:        now.Add(-time.Minute),
		AbsoluteExpiresAt: now.Add(time.Hour),
	}
	with := func(f func(*SessionState)) SessionState { s := fresh; f(&s); return s }

	cases := []struct {
		name  string
		state SessionState
		want  Verdict
	}{
		{"fresh session", fresh, Accept},
		{"revoked", with(func(s *SessionState) { s.Revoked = true }), Reject},
		{"inactive user", with(func(s *SessionState) { s.UserActive = false }), Reject},
		{"absolute expiry reached", with(func(s *SessionState) { s.AbsoluteExpiresAt = now }), Reject},
		{"idle too long", with(func(s *SessionState) { s.LastSeenAt = now.Add(-IdleTimeout - time.Second) }), Reject},
		{"due for rotation", with(func(s *SessionState) { s.RotatedAt = now.Add(-RotationInterval) }), AcceptAndRotate},
		{"previous id within grace", with(func(s *SessionState) {
			s.MatchedPrevious = true
			s.PreviousValidUntil = now.Add(PreviousGrace)
		}), Accept},
		{"previous id after grace", with(func(s *SessionState) {
			s.MatchedPrevious = true
			s.PreviousValidUntil = now
		}), Reject},
		{"previous id without grace window", with(func(s *SessionState) { s.MatchedPrevious = true }), Reject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.state, now); got != c.want {
				t.Errorf("Decide = %v, want %v", got, c.want)
			}
		})
	}
}

func TestHashPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("expected error for password < 12 bytes")
	}
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "wrong horse battery") {
		t.Error("wrong password accepted")
	}
}
