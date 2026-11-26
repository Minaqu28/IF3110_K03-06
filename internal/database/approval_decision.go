package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var _ workflow.ApprovalStore = (*ApprovalPostgresStore)(nil)

func (s *ApprovalPostgresStore) SaveDecision(ctx context.Context, decision domain.ApprovalDecision) (domain.ApprovalDecision, error) {
	saved, err := scanDecision(s.db.QueryRowContext(ctx, `
		INSERT INTO approval_decisions (review_id, group_id, leader_id, status, comment, decided_at)
		SELECT q.id, $2, $3, $4::text, $5,
		       CASE WHEN $4::text IN ('APPROVED', 'REJECTED') THEN now() END
		FROM review_requests q
		WHERE q.id = $1 AND (q.status = 'PENDING' OR $4::text = 'STALE')
		ON CONFLICT (review_id, group_id) DO UPDATE SET
		    leader_id  = EXCLUDED.leader_id,
		    status     = EXCLUDED.status,
		    comment    = EXCLUDED.comment,
		    decided_at = CASE WHEN EXCLUDED.status = 'STALE' THEN approval_decisions.decided_at
		                      ELSE EXCLUDED.decided_at END
		RETURNING `+decisionColumns,
		decision.ReviewID, decision.GroupID, decision.LeaderID, string(decision.Status), nullString(decision.Comment)))
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if qErr := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM review_requests WHERE id = $1)`, decision.ReviewID).Scan(&exists); qErr != nil {
			return domain.ApprovalDecision{}, qErr
		}
		if exists {
			return domain.ApprovalDecision{}, admin.ErrConflict
		}
		return domain.ApprovalDecision{}, admin.ErrNotFound
	}
	if err != nil {
		return domain.ApprovalDecision{}, adminError(err)
	}
	return saved, nil
}
