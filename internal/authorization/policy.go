package authorization

import "github.com/Minaqu28/IF3110_K03-06/internal/domain"

type AccessLevel string

const (
	AccessNone     AccessLevel = ""
	AccessViewOnly AccessLevel = "VIEW_ONLY"
	AccessViewEdit AccessLevel = "VIEW_EDIT"
)

func EffectiveAccess(grants ...AccessLevel) AccessLevel {
	effective := AccessNone
	for _, grant := range grants {
		if accessRank(grant) > accessRank(effective) {
			effective = grant
		}
	}
	return effective
}

func (level AccessLevel) Allows(required AccessLevel) bool {
	levelRank := accessRank(level)
	requiredRank := accessRank(required)
	return levelRank >= requiredRank && levelRank > 0 && requiredRank > 0
}

func CanManagePlatform(user domain.User) bool {
	return user.IsActive && user.IsPlatformAdmin
}

func accessRank(level AccessLevel) int {
	switch level {
	case AccessViewOnly:
		return 1
	case AccessViewEdit:
		return 2
	default:
		return 0
	}
}
