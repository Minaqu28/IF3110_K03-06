package migrationengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifySQLState(t *testing.T) {
	cases := map[string]ErrorClass{
		"40001": ErrorTransient,
		"40P01": ErrorTransient,
		"55P03": ErrorTransient,
		"57P01": ErrorTransient,
		"57P02": ErrorTransient,
		"57P03": ErrorTransient,
		"53300": ErrorTransient,
		"08000": ErrorTransient,
		"08003": ErrorTransient,
		"08006": ErrorTransient,
		"08P01": ErrorTransient,
		"42P01": ErrorPermanent,
		"42601": ErrorPermanent,
		"23505": ErrorPermanent,
		"23503": ErrorPermanent,
		"22P02": ErrorPermanent,
		"57014": ErrorPermanent,
		"28P01": ErrorPermanent,
		"53100": ErrorPermanent,
		"0800":  ErrorPermanent,
		"":      ErrorPermanent,
	}
	for code, want := range cases {
		if got := ClassifySQLState(code); got != want {
			t.Errorf("ClassifySQLState(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrorPermanent},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, ErrorTransient},
		{"wrapped lock timeout", fmt.Errorf("batch 3: %w", &pgconn.PgError{Code: "55P03"}), ErrorTransient},
		{"syntax error", &pgconn.PgError{Code: "42601"}, ErrorPermanent},
		{"bad password on connect", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"}), ErrorPermanent},
		{"uncertain commit", fmt.Errorf("%w: %w", ErrCommitUncertain, io.EOF), ErrorUncertain},
		{"uncertain beats sqlstate", fmt.Errorf("%w: %w", ErrCommitUncertain, &pgconn.PgError{Code: "42601"}), ErrorUncertain},
		{"target busy", fmt.Errorf("batch run: %w", ErrTargetBusy), ErrorTransient},
		{"cancelled", context.Canceled, ErrorTransient},
		{"deadline", fmt.Errorf("run: %w", context.DeadlineExceeded), ErrorTransient},
		{"eof", io.EOF, ErrorTransient},
		{"unexpected eof", io.ErrUnexpectedEOF, ErrorTransient},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ErrorTransient},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), ErrorTransient},
		{"dns", &net.DNSError{Err: "no such host", Name: "db"}, ErrorTransient},
		{"not found", admin.ErrNotFound, ErrorPermanent},
		{"invalid input", admin.ErrInvalidInput, ErrorPermanent},
		{"plain", errors.New("boom"), ErrorPermanent},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("%s: ClassifyError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
