package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeJobGetter struct {
	job domain.Job
	err error
}

func (f fakeJobGetter) GetJob(context.Context, string) (domain.Job, error) { return f.job, f.err }

func TestGetJobNeedsViewAccess(t *testing.T) {
	job := domain.Job{ID: "job-1", DatabaseID: 4, Status: domain.JobStatusRunning}
	for _, level := range []authorization.AccessLevel{authorization.AccessViewOnly, authorization.AccessViewEdit} {
		got, err := NewJobQueryService(fakeAccess{level: level}, fakeJobGetter{job: job}).GetJob(context.Background(), editor, "job-1")
		if err != nil || got != job {
			t.Fatalf("%v: job = %+v, err = %v", level, got, err)
		}
	}
	for name, access := range map[string]fakeAccess{
		"no grant":       {level: authorization.AccessNone},
		"no connection":  {err: admin.ErrForbidden},
		"inactive actor": {err: admin.ErrForbidden},
	} {
		if _, err := NewJobQueryService(access, fakeJobGetter{job: job}).GetJob(context.Background(), editor, "job-1"); !errors.Is(err, admin.ErrNotFound) {
			t.Fatalf("%s: err = %v, want not found so the job's existence is not revealed", name, err)
		}
	}
	boom := errors.New("platform down")
	if _, err := NewJobQueryService(fakeAccess{err: boom}, fakeJobGetter{job: job}).GetJob(context.Background(), editor, "job-1"); !errors.Is(err, boom) {
		t.Fatalf("access error = %v", err)
	}
	if _, err := NewJobQueryService(fakeAccess{level: authorization.AccessViewEdit}, fakeJobGetter{err: admin.ErrNotFound}).GetJob(context.Background(), editor, "x"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}
}
