package metrics

import (
	"context"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "dbmp"

var waitBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

var durationBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 300, 900, 3600}

type MigrationRecorder struct {
	queueWait prometheus.Histogram
	lockWait  *prometheus.HistogramVec
	duration  *prometheus.HistogramVec
	retries   *prometheus.CounterVec
	batches   prometheus.Counter
	rows      prometheus.Counter
}

func NewMigrationRecorder(r *Registry) (*MigrationRecorder, error) {
	m := &MigrationRecorder{
		queueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "job_queue_wait_seconds",
			Help:    "Time from job creation until a worker picks it up for its first attempt.",
			Buckets: waitBuckets,
		}),
		lockWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "job_lock_wait_seconds",
			Help:    "Time spent acquiring the per-database migration lock.",
			Buckets: waitBuckets,
		}, []string{"acquired"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "migration_duration_seconds",
			Help:    "Migration execution time per attempt.",
			Buckets: durationBuckets,
		}, []string{"action", "mode", "status"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "job_retries_total",
			Help: "Jobs scheduled for another attempt.",
		}, []string{"reason"}),
		batches: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "migration_batches_processed_total",
			Help: "Batches committed by batch-mode migrations, including compensating batches.",
		}),
		rows: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "migration_rows_processed_total",
			Help: "Rows affected by committed batches.",
		}),
	}
	for _, c := range []prometheus.Collector{m.queueWait, m.lockWait, m.duration, m.retries, m.batches, m.rows} {
		if err := r.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *MigrationRecorder) QueueWait(d time.Duration) { m.queueWait.Observe(d.Seconds()) }

func (m *MigrationRecorder) LockWait(d time.Duration, acquired bool) {
	label := "false"
	if acquired {
		label = "true"
	}
	m.lockWait.WithLabelValues(label).Observe(d.Seconds())
}

func (m *MigrationRecorder) JobFinished(action domain.MigrationAction, mode domain.ExecutionMode, status domain.JobStatus, d time.Duration) {
	m.duration.WithLabelValues(string(action), string(mode), string(status)).Observe(d.Seconds())
}

func (m *MigrationRecorder) Retry(reason string) { m.retries.WithLabelValues(reason).Inc() }

func (m *MigrationRecorder) BatchesCommitted(batches int, rows int64) {
	m.batches.Add(float64(batches))
	m.rows.Add(float64(rows))
}

type JobCounter func(ctx context.Context) (map[domain.JobStatus]int64, error)

type jobStatusCollector struct {
	count JobCounter
	desc  *prometheus.Desc
	up    *prometheus.Desc
}

var jobStatuses = []domain.JobStatus{
	domain.JobStatusQueued, domain.JobStatusRunning, domain.JobStatusRetryWait, domain.JobStatusSucceeded,
	domain.JobStatusFailed, domain.JobStatusNeedsReconciliation, domain.JobStatusCancelled,
}

func NewJobStatusCollector(count JobCounter) prometheus.Collector {
	return &jobStatusCollector{
		count: count,
		desc:  prometheus.NewDesc(namespace+"_jobs", "Migration jobs currently in each status.", []string{"status"}, nil),
		up:    prometheus.NewDesc(namespace+"_jobs_scrape_success", "1 if the job counts were read from the platform database.", nil, nil),
	}
}

func (c *jobStatusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
	ch <- c.up
}

func (c *jobStatusCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	counts, err := c.count(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, s := range jobStatuses {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[s]), string(s))
	}
}
