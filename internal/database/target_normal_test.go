package database

import (
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCommitRejected(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"deferred constraint":  {&pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23503"}, true},
		"localized severity":   {&pgconn.PgError{Severity: "ERROR", Code: "40001"}, true},
		"wrapped error":        {fmt.Errorf("commit: %w", &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "23505"}), true},
		"terminated backend":   {&pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}, false},
		"server crash":         {&pgconn.PgError{SeverityUnlocalized: "PANIC", Code: "XX000"}, false},
		"connection dropped":   {io.ErrUnexpectedEOF, false},
		"translated FATAL msg": {&pgconn.PgError{Severity: "FATAL", Code: "57P01"}, false},
	}
	for name, c := range cases {
		if _, got := commitRejected(c.err); got != c.want {
			t.Errorf("%s: commitRejected = %v, want %v", name, got, c.want)
		}
	}
}
