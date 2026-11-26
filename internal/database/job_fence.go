package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

type FencedJobStore struct {
	jobs  *JobPostgresStore
	owner string
	token int64
}

var _ worker.JobStore = (*FencedJobStore)(nil)

func (s *JobPostgresStore) Fenced(owner string, token int64) *FencedJobStore {
	return &FencedJobStore{jobs: s, owner: owner, token: token}
}

func (f *FencedJobStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	return f.jobs.GetJob(ctx, jobID)
}

func (f *FencedJobStore) SaveJob(ctx context.Context, job domain.Job) error {
	return f.jobs.saveJob(ctx, job, f.guard, nil)
}

func (f *FencedJobStore) SaveJobAfter(ctx context.Context, job domain.Job, notBefore time.Time) error {
	return f.jobs.saveJob(ctx, job, f.guard, &notBefore)
}

func (f *FencedJobStore) guard(ctx context.Context, tx *sql.Tx, jobID string) error {
	var held bool
	err := tx.QueryRowContext(ctx, `
		SELECT TRUE
		FROM migration_jobs j
		JOIN database_locks l ON l.database_id = j.database_id AND l.execution_id = j.execution_id
		WHERE j.id = $1 AND l.owner = $2 AND l.fencing_token = $3 AND l.lease_expires_at > now()
		FOR SHARE OF l`, jobID, f.owner, f.token).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return worker.ErrLockLost
	}
	return adminError(err)
}
