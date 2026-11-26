package migrationengine

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

type ErrorClass int

const (
	ErrorPermanent ErrorClass = iota
	ErrorTransient
	ErrorUncertain
)

func (c ErrorClass) String() string {
	switch c {
	case ErrorTransient:
		return "transient"
	case ErrorUncertain:
		return "uncertain"
	}
	return "permanent"
}

var ErrCommitUncertain = errors.New("the commit outcome is unknown")

var transientSQLStates = map[string]bool{
	"40001": true,
	"40P01": true,
	"55P03": true,
	"57P01": true,
	"57P02": true,
	"57P03": true,
	"53300": true,
}

func ClassifySQLState(code string) ErrorClass {
	if transientSQLStates[code] || (len(code) == 5 && strings.HasPrefix(code, "08")) {
		return ErrorTransient
	}
	return ErrorPermanent
}

func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorPermanent
	}
	if errors.Is(err, ErrCommitUncertain) {
		return ErrorUncertain
	}
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return ClassifySQLState(coded.SQLState())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return ErrorTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorTransient
	}
	return ErrorPermanent
}
