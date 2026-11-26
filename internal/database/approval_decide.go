package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var _ workflow.ReviewDecider = (*ApprovalPostgresStore)(nil)

func (s *ApprovalPostgresStore) DecideReview(ctx context.Context, reviewID, leaderID int64, status domain.ApprovalStatus, comment string, resolve func([]domain.ApprovalDecision) domain.ReviewStatus) (domain.ReviewRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	defer tx.Rollback()
	review, err := scanReview(tx.QueryRowContext(ctx, `
		SELECT `+reviewColumns+` FROM review_requests WHERE id = $1 FOR UPDATE`, reviewID))
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	if review.Status != domain.ReviewStatusPending {
		return domain.ReviewRequest{}, admin.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE approval_decisions SET status = $3, comment = $4, decided_at = now()
		WHERE review_id = $1 AND leader_id = $2 AND status = 'PENDING'`,
		reviewID, leaderID, string(status), nullString(comment))
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return domain.ReviewRequest{}, err
	} else if n == 0 {
		var isLeader bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM approval_decisions WHERE review_id = $1 AND leader_id = $2)`,
			reviewID, leaderID).Scan(&isLeader); err != nil {
			return domain.ReviewRequest{}, err
		}
		if isLeader {
			return domain.ReviewRequest{}, admin.ErrConflict
		}
		return domain.ReviewRequest{}, admin.ErrForbidden
	}
	decisions, err := listDecisions(ctx, tx, reviewID)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	if resolved := resolve(decisions); resolved != domain.ReviewStatusPending {
		review, err = scanReview(tx.QueryRowContext(ctx, `
			UPDATE review_requests SET status = $2, resolved_at = now()
			WHERE id = $1 RETURNING `+reviewColumns, reviewID, string(resolved)))
		if err != nil {
			return domain.ReviewRequest{}, adminError(err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE migration_revisions SET status = $2 WHERE id = $1 AND status = 'IN_REVIEW'`,
			review.RevisionID, string(resolved)); err != nil {
			return domain.ReviewRequest{}, adminError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewRequest{}, err
	}
	return review, nil
}

const reviewColumns = `id, revision_id, action, target_execution_id::text, definition_hash, status, requested_by, requested_at, resolved_at`

func scanReview(row rowScanner) (domain.ReviewRequest, error) {
	var (
		r           domain.ReviewRequest
		action      string
		target      sql.NullString
		status      string
		requestedBy sql.NullInt64
		resolvedAt  sql.NullTime
	)
	if err := row.Scan(&r.ID, &r.RevisionID, &action, &target, &r.DefinitionHash, &status, &requestedBy, &r.RequestedAt, &resolvedAt); err != nil {
		return domain.ReviewRequest{}, err
	}
	r.Action = domain.MigrationAction(action)
	if target.Valid {
		t := target.String
		r.TargetExecutionID = &t
	}
	r.Status = domain.ReviewStatus(status)
	r.RequestedBy = requestedBy.Int64
	if resolvedAt.Valid {
		t := resolvedAt.Time
		r.ResolvedAt = &t
	}
	return r, nil
}
