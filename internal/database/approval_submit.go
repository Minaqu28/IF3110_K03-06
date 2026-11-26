package database

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

var (
	_ workflow.GrantReader     = (*ApprovalPostgresStore)(nil)
	_ workflow.ReviewSubmitter = (*ApprovalPostgresStore)(nil)
)

func (s *ApprovalPostgresStore) ListGroupGrants(ctx context.Context, databaseID int64) ([]workflow.GroupGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.group_id, a.database_id, COALESCE(g.leader_id, 0), a.access_level
		FROM group_database_access a
		JOIN groups g ON g.id = a.group_id
		WHERE a.database_id = $1
		ORDER BY a.group_id`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []workflow.GroupGrant
	for rows.Next() {
		var g workflow.GroupGrant
		var level string
		if err := rows.Scan(&g.GroupID, &g.DatabaseID, &g.LeaderID, &level); err != nil {
			return nil, err
		}
		g.Level = authorization.AccessLevel(level)
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

func (s *ApprovalPostgresStore) SubmitForReview(ctx context.Context, databaseID, revisionID, requestedBy int64, approvers []workflow.RequiredApprover) (domain.ReviewRequest, error) {
	if len(approvers) == 0 {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	defer tx.Rollback()
	rev, err := lockRevisionTx(ctx, tx, databaseID, revisionID)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	review, err := createReviewTx(ctx, tx, domain.ReviewRequest{RevisionID: rev.ID, DefinitionHash: rev.DefinitionHash, RequestedBy: requestedBy})
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	for _, a := range approvers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approval_decisions (review_id, group_id, leader_id) VALUES ($1, $2, $3)`,
			review.ID, a.GroupID, a.LeaderID); err != nil {
			return domain.ReviewRequest{}, adminError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewRequest{}, err
	}
	return review, nil
}
