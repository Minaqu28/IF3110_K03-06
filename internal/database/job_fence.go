package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

type FencedJobStore struct {
	jobs  *JobPostgresStore
	owner string
	token int64
}

var _ worker.JobStateStore = (*FencedJobStore)(nil)

func (s *JobPostgresStore) Fenced(owner string, token int64) *FencedJobStore {
	return &FencedJobStore{jobs: s, owner: owner, token: token}
}

func (f *FencedJobStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	return f.jobs.GetJob(ctx, jobID)
}

func (f *FencedJobStore) SaveJob(ctx context.Context, job domain.Job) error {
	return f.jobs.saveJob(ctx, job, func(ctx context.Context, tx *sql.Tx) error {
		var held bool
		err := tx.QueryRowContext(ctx, `
			SELECT TRUE
			FROM migration_jobs j
			JOIN database_locks l ON l.database_id = j.database_id AND l.execution_id = j.execution_id
			WHERE j.id = $1 AND l.owner = $2 AND l.fencing_token = $3 AND l.lease_expires_at > now()
			FOR SHARE OF l`, job.ID, f.owner, f.token).Scan(&held)
		if errors.Is(err, sql.ErrNoRows) {
			return worker.ErrLockLost
		}
		return adminError(err)
	})
}
