package database

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/worker"
)

var _ worker.DatabaseLocker = (*LockPostgresStore)(nil)

func (s *LockPostgresStore) Heartbeat(ctx context.Context, databaseID int64, owner string, token int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE database_locks
		SET lease_expires_at = now() + $4 * interval '1 millisecond'
		WHERE database_id = $1 AND owner = $2 AND fencing_token = $3 AND lease_expires_at > now()`,
		databaseID, owner, token, s.leaseMillis())
	if err != nil {
		return false, adminError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
