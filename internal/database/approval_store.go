package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type ApprovalPostgresStore struct {
	db *sql.DB
}

func NewApprovalPostgresStore(db *sql.DB) *ApprovalPostgresStore {
	return &ApprovalPostgresStore{db: db}
}

func (s *ApprovalPostgresStore) CreateReview(ctx context.Context, review domain.ReviewRequest) (domain.ReviewRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	defer tx.Rollback()
	var hash string
	var locked bool
	err = tx.QueryRowContext(ctx, `
		SELECT definition_hash, locked_at IS NOT NULL FROM migration_revisions
		WHERE id = $1 FOR SHARE`, review.RevisionID).Scan(&hash, &locked)
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	if !locked || hash != review.DefinitionHash {
		return domain.ReviewRequest{}, admin.ErrConflict
	}
	created := domain.ReviewRequest{RevisionID: review.RevisionID, DefinitionHash: hash, Status: domain.ReviewStatusPending, RequestedBy: review.RequestedBy}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO review_requests (revision_id, definition_hash, requested_by)
		VALUES ($1, $2, $3) RETURNING id, requested_at`,
		review.RevisionID, hash, sql.NullInt64{Int64: review.RequestedBy, Valid: review.RequestedBy > 0}).
		Scan(&created.ID, &created.RequestedAt)
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewRequest{}, err
	}
	return created, nil
}

func (s *ApprovalPostgresStore) ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+decisionColumns+` FROM approval_decisions
		WHERE review_id = $1 ORDER BY group_id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []domain.ApprovalDecision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

const decisionColumns = `id, review_id, group_id, leader_id, status, comment, decided_at`

func scanDecision(row rowScanner) (domain.ApprovalDecision, error) {
	var (
		d         domain.ApprovalDecision
		status    string
		comment   sql.NullString
		decidedAt sql.NullTime
	)
	if err := row.Scan(&d.ID, &d.ReviewID, &d.GroupID, &d.LeaderID, &status, &comment, &decidedAt); err != nil {
		return domain.ApprovalDecision{}, err
	}
	d.Status = domain.ApprovalStatus(status)
	d.Comment = comment.String
	if decidedAt.Valid {
		t := decidedAt.Time
		d.DecidedAt = &t
	}
	return d, nil
}
