package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type LockPostgresStore struct {
	db    *sql.DB
	lease time.Duration
}

func NewLockPostgresStore(db *sql.DB, lease time.Duration) *LockPostgresStore {
	return &LockPostgresStore{db: db, lease: lease}
}

func (s *LockPostgresStore) leaseMillis() int64 {
	return s.lease.Milliseconds()
}

func (s *LockPostgresStore) Acquire(ctx context.Context, databaseID int64, executionID, owner string) (int64, bool, error) {
	if s.lease <= 0 {
		return 0, false, errors.New("lock lease must be positive")
	}
	var token int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO database_locks (database_id, execution_id, owner, fencing_token, lease_expires_at)
		VALUES ($1, $2, $3, nextval('database_lock_fencing_seq'), now() + $4 * interval '1 millisecond')
		ON CONFLICT (database_id) DO UPDATE
		SET execution_id     = EXCLUDED.execution_id,
		    owner            = EXCLUDED.owner,
		    fencing_token    = EXCLUDED.fencing_token,
		    lease_expires_at = EXCLUDED.lease_expires_at,
		    acquired_at      = now()
		WHERE database_locks.lease_expires_at <= now()
		   OR (database_locks.execution_id = EXCLUDED.execution_id AND database_locks.owner = EXCLUDED.owner)
		RETURNING fencing_token`,
		databaseID, executionID, owner, s.leaseMillis()).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, adminError(err)
	}
	return token, true, nil
}

func (s *LockPostgresStore) Release(ctx context.Context, databaseID int64, owner string, token int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM database_locks
		WHERE database_id = $1 AND owner = $2 AND fencing_token = $3`, databaseID, owner, token)
	return adminError(err)
}
