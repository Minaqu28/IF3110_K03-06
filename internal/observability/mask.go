package observability

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

const RedactedValue = "[REDACTED]"

var sensitiveKeySubstrings = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"cookie",
	"session",
	"authorization",
	"credential",
	"dsn",
	"database_url",
	"api_key",
	"apikey",
}

var (
	dsnPattern        = regexp.MustCompile(`(?i)\bpostgres(?:ql)?://[^\s"'` + "`" + `]+`)
	kvPasswordPattern = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|session_id|cookie_session)\s*=\s*([^\s;,&'"]+)`)
)

func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, sub := range sensitiveKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func MaskString(s string) string {
	if s == "" {
		return s
	}
	s = dsnPattern.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			return raw
		}
		if _, hasPass := u.User.Password(); !hasPass {
			return raw
		}
		u.User = url.UserPassword(u.User.Username(), RedactedValue)
		return u.String()
	})
	s = kvPasswordPattern.ReplaceAllString(s, "$1="+RedactedValue)
	return s
}

func MaskAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, RedactedValue)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, MaskString(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		masked := make([]any, len(attrs))
		for i, ga := range attrs {
			masked[i] = MaskAttr(ga)
		}
		return slog.Group(a.Key, masked...)
	default:
		return a
	}
}

func MaskSQLParams(params []any) []any {
	if len(params) == 0 {
		return nil
	}
	out := make([]any, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case string:
			out[i] = MaskString(v)
		case []byte:
			out[i] = RedactedValue
		default:
			out[i] = v
		}
	}
	return out
}
