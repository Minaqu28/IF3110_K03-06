package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestMigrationRecorderExposesMetrics(t *testing.T) {
	r := NewRegistry()
	m, err := NewMigrationRecorder(r)
	if err != nil {
		t.Fatal(err)
	}
	m.QueueWait(300 * time.Millisecond)
	m.LockWait(20*time.Millisecond, true)
	m.LockWait(5*time.Second, false)
	m.JobFinished(domain.MigrationActionApply, domain.ExecutionModeBatch, domain.JobStatusSucceeded, 2*time.Second)
	m.Retry("transient")
	m.Retry("locked")
	m.BatchesCommitted(3, 30000)

	body := scrape(t, r)
	for _, want := range []string{
		"dbmp_job_queue_wait_seconds_count 1",
		`dbmp_job_lock_wait_seconds_count{acquired="true"} 1`,
		`dbmp_job_lock_wait_seconds_count{acquired="false"} 1`,
		`dbmp_migration_duration_seconds_count{action="APPLY",mode="BATCH",status="SUCCEEDED"} 1`,
		`dbmp_job_retries_total{reason="transient"} 1`,
		`dbmp_job_retries_total{reason="locked"} 1`,
		"dbmp_migration_batches_processed_total 3",
		"dbmp_migration_rows_processed_total 30000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %s", want)
		}
	}
	if _, err := NewMigrationRecorder(r); err == nil {
		t.Fatal("registering a second recorder on the same registry should fail")
	}
}

func TestJobStatusCollector(t *testing.T) {
	r := NewRegistry()
	counts := map[domain.JobStatus]int64{domain.JobStatusQueued: 4, domain.JobStatusRunning: 1, domain.JobStatusFailed: 2}
	var fail bool
	if err := r.Register(NewJobStatusCollector(func(context.Context) (map[domain.JobStatus]int64, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return counts, nil
	})); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, r)
	for _, want := range []string{
		`dbmp_jobs{status="QUEUED"} 4`,
		`dbmp_jobs{status="RUNNING"} 1`,
		`dbmp_jobs{status="FAILED"} 2`,
		`dbmp_jobs{status="SUCCEEDED"} 0`,
		"dbmp_jobs_scrape_success 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %s", want)
		}
	}
	fail = true
	body = scrape(t, r)
	if !strings.Contains(body, "dbmp_jobs_scrape_success 0") || strings.Contains(body, `dbmp_jobs{status="QUEUED"}`) {
		t.Fatalf("failed scrape should report success 0 and no counts:\n%s", body)
	}
}
