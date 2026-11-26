package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type settled struct {
	acks, requeues, rejects int
}

func (s *settled) total() int { return s.acks + s.requeues + s.rejects }

func delivery(msg domain.MigrationJobMessage, s *settled) *mockJobDelivery {
	return &mockJobDelivery{
		message: msg,
		ack:     func() error { s.acks++; return nil },
		requeue: func() error { s.requeues++; return nil },
		reject:  func() error { s.rejects++; return nil },
	}
}

func message() domain.MigrationJobMessage {
	return domain.MigrationJobMessage{
		SchemaVersion: domain.MigrationJobSchemaVersion, JobID: "job-1", ExecutionID: "exec-1", DatabaseID: 4,
		MigrationID: 9, RevisionID: 90, ApprovalRequestID: 3, ActorID: 2, Action: domain.MigrationActionApply, Mode: domain.ExecutionModeNormal,
	}
}

func queuedJob() domain.Job {
	return domain.Job{ID: "job-1", ExecutionID: "exec-1", DatabaseID: 4, Status: domain.JobStatusQueued}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func admitWith(t *testing.T, msg domain.MigrationJobMessage, getJob func(context.Context, string) (domain.Job, error)) (domain.Job, bool, *settled) {
	t.Helper()
	s := &settled{}
	w := New(Dependencies{Jobs: &delayingJobs{mockJobStateStore: &mockJobStateStore{getJob: getJob}}}, "w1", quietLog())
	job, ok := w.admit(context.Background(), w.log, delivery(msg, s))
	return job, ok, s
}

func TestAdmitRunsAnOpenJob(t *testing.T) {
	for _, status := range []domain.JobStatus{domain.JobStatusQueued, domain.JobStatusRetryWait, domain.JobStatusNeedsReconciliation, domain.JobStatusRunning} {
		job, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			j.Status = status
			return j, nil
		})
		if !ok || job.ID != "job-1" || s.total() != 0 {
			t.Fatalf("%s: admitted = %v, settled %+v; an open job runs and is not settled yet", status, ok, s)
		}
	}
}

func TestAdmitAcksADuplicateOfAFinishedJob(t *testing.T) {
	for _, status := range []domain.JobStatus{domain.JobStatusSucceeded, domain.JobStatusFailed, domain.JobStatusCancelled} {
		_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			j.Status = status
			return j, nil
		})
		if ok || s.acks != 1 || s.total() != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v; want a single ack", status, ok, s)
		}
	}
}

func TestAdmitRejectsBadMessages(t *testing.T) {
	cases := map[string]func(*domain.MigrationJobMessage){
		"old schema":     func(m *domain.MigrationJobMessage) { m.SchemaVersion = 0 },
		"future schema":  func(m *domain.MigrationJobMessage) { m.SchemaVersion = domain.MigrationJobSchemaVersion + 1 },
		"no job id":      func(m *domain.MigrationJobMessage) { m.JobID = "" },
		"no execution":   func(m *domain.MigrationJobMessage) { m.ExecutionID = "" },
		"no database":    func(m *domain.MigrationJobMessage) { m.DatabaseID = 0 },
		"no revision":    func(m *domain.MigrationJobMessage) { m.RevisionID = 0 },
		"unknown action": func(m *domain.MigrationJobMessage) { m.Action = "DROP" },
		"unknown mode":   func(m *domain.MigrationJobMessage) { m.Mode = "FAST" },
	}
	for name, mutate := range cases {
		msg := message()
		mutate(&msg)
		_, ok, s := admitWith(t, msg, nil)
		if ok || s.rejects != 1 || s.total() != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v; want a single reject", name, ok, s)
		}
	}
}

func TestAdmitRejectsAMessageThatDoesNotMatchItsJob(t *testing.T) {
	for name, mutate := range map[string]func(*domain.Job){
		"other execution": func(j *domain.Job) { j.ExecutionID = "exec-2" },
		"other database":  func(j *domain.Job) { j.DatabaseID = 5 },
	} {
		_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) {
			j := queuedJob()
			mutate(&j)
			return j, nil
		})
		if ok || s.rejects != 1 {
			t.Fatalf("%s: admitted = %v, settled %+v", name, ok, s)
		}
	}
	_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) { return domain.Job{}, admin.ErrNotFound })
	if ok || s.rejects != 1 {
		t.Fatalf("unknown job: admitted = %v, settled %+v", ok, s)
	}
}

func TestAdmitRequeuesWhenTheJobCannotBeLoaded(t *testing.T) {
	_, ok, s := admitWith(t, message(), func(context.Context, string) (domain.Job, error) { return domain.Job{}, errors.New("platform down") })
	if ok || s.requeues != 1 || s.total() != 1 {
		t.Fatalf("admitted = %v, settled %+v; want a single requeue", ok, s)
	}
}
