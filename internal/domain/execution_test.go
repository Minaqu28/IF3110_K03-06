package domain

import (
	"testing"
)

func TestJobStatusTerminal(t *testing.T) {
	cases := map[JobStatus]bool{
		JobStatusQueued:              false,
		JobStatusRunning:             false,
		JobStatusRetryWait:           false,
		JobStatusNeedsReconciliation: false,
		JobStatusSucceeded:           true,
		JobStatusFailed:              true,
		JobStatusCancelled:           true,
		JobStatus("UNKNOWN_VALUE"):   false,
	}
	for status, want := range cases {
		if got := status.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", status, got, want)
		}
	}
}
