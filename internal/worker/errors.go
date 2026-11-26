package worker

import "errors"

var ErrLockLost = errors.New("database lock lost")
