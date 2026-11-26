package database

import (
	"context"
	"database/sql"
)

func staleOlderReviewsTx(ctx context.Context, tx *sql.Tx, migrationID int64, revision int) error {
	_, err := tx.ExecContext(ctx, `
		WITH staled AS (
		    UPDATE review_requests q
		    SET status = 'STALE', resolved_at = now()
		    FROM migration_revisions r
		    WHERE q.revision_id = r.id AND r.migration_id = $1 AND r.revision < $2 AND q.status = 'PENDING'
		    RETURNING q.id
		)
		UPDATE approval_decisions d
		SET status = 'STALE'
		WHERE d.review_id IN (SELECT id FROM staled)`, migrationID, revision)
	return adminError(err)
}
