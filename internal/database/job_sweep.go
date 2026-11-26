package database

import "context"

func (s *JobPostgresStore) SweepExpiredJobs(ctx context.Context) (int, error) {
	var swept int
	err := s.db.QueryRowContext(ctx, `
		WITH expired AS (
		    UPDATE migration_jobs j
		    SET status = 'NEEDS_RECONCILIATION', published_at = NULL, next_attempt_at = now(), updated_at = now(),
		        error_code = 'LEASE_EXPIRED', error_message = 'Worker berhenti sebelum hasil eksekusi tersimpan'
		    WHERE j.status = 'RUNNING'
		      AND NOT EXISTS (
		          SELECT 1 FROM database_locks l
		          WHERE l.database_id = j.database_id AND l.execution_id = j.execution_id
		            AND l.lease_expires_at > now()
		      )
		    RETURNING j.execution_id
		), executions AS (
		    UPDATE migration_executions e
		    SET status = 'UNKNOWN'
		    WHERE e.id IN (SELECT execution_id FROM expired) AND e.status NOT IN ('SUCCEEDED', 'FAILED')
		)
		SELECT count(*) FROM expired`).Scan(&swept)
	return swept, err
}
