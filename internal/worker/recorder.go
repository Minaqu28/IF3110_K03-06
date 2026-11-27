package worker

import (
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type Recorder interface {
	QueueWait(d time.Duration)
	LockWait(d time.Duration, acquired bool)
	JobFinished(action domain.MigrationAction, mode domain.ExecutionMode, status domain.JobStatus, d time.Duration)
	Retry(reason string)
	BatchesCommitted(batches int, rows int64)
}

type nopRecorder struct{}

func (nopRecorder) QueueWait(time.Duration)      {}
func (nopRecorder) LockWait(time.Duration, bool) {}
func (nopRecorder) JobFinished(domain.MigrationAction, domain.ExecutionMode, domain.JobStatus, time.Duration) {
}
func (nopRecorder) Retry(string)                {}
func (nopRecorder) BatchesCommitted(int, int64) {}

func (w *Worker) WithRecorder(r Recorder) *Worker {
	if r == nil {
		r = nopRecorder{}
	}
	w.rec = r
	return w
}
