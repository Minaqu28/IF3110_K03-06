package authentication

import "time"

type Verdict int

const (
	Reject Verdict = iota
	Accept
	AcceptAndRotate
)

type SessionState struct {
	Revoked            bool
	UserActive         bool
	MatchedPrevious    bool // the presented identifier is the pre-rotation one
	RotatedAt          time.Time
	PreviousValidUntil time.Time
	LastSeenAt         time.Time
	AbsoluteExpiresAt  time.Time
}

func Decide(s SessionState, now time.Time) Verdict {
	switch {
	case s.Revoked, !s.UserActive:
		return Reject
	case !now.Before(s.AbsoluteExpiresAt):
		return Reject
	case now.Sub(s.LastSeenAt) > IdleTimeout:
		return Reject
	case s.MatchedPrevious:
		if s.PreviousValidUntil.IsZero() || !now.Before(s.PreviousValidUntil) {
			return Reject
		}
		return Accept
	case now.Sub(s.RotatedAt) >= RotationInterval:
		return AcceptAndRotate
	}
	return Accept
}
