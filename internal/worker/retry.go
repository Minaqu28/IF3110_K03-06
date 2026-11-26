package worker

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type JobStore interface {
	JobStateStore
	SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error
}

type RetryPolicy struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int
	Jitter      func() float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: 2 * time.Second, Max: 2 * time.Minute, MaxAttempts: 5, Jitter: rand.Float64}
}

func (p RetryPolicy) Backoff(attempt int) time.Duration {
	delay := p.Max
	if attempt < 1 {
		attempt = 1
	}
	if shift := attempt - 1; shift < 30 && p.Base<<shift < p.Max {
		delay = p.Base << shift
	}
	if p.Jitter != nil {
		delay += time.Duration(p.Jitter() * float64(delay) / 5)
	}
	return min(delay, p.Max)
}

func (p RetryPolicy) schedule(job domain.Job) (domain.Job, time.Duration) {
	switch job.Status {
	case domain.JobStatusRetryWait:
		if job.AttemptCount >= p.MaxAttempts {
			job.Status = domain.JobStatusFailed
			job.ErrorMessage = fmt.Sprintf("gave up after %d attempts: %s", job.AttemptCount, job.ErrorMessage)
			return job, 0
		}
		return job, p.Backoff(job.AttemptCount)
	case domain.JobStatusNeedsReconciliation:
		return job, p.Backoff(job.AttemptCount)
	}
	return job, 0
}
