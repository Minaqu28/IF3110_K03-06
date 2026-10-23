package authentication

import "time"

type Verdict int

const (
	Reject Verdict = iota
	Accept
	AcceptAndRotate
)

type SessionState struct {
	Revoked           bool
	UserActive        bool
	MatchedPrevious   bool // the presented identifier is the pre-rotation one
	RotatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
}

func Decide(s SessionState, now time.Time) Verdict {

}
