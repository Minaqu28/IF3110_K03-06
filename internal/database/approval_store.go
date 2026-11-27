package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
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
	created, err := createReviewTx(ctx, tx, review)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewRequest{}, err
	}
	return created, nil
}

func createReviewTx(ctx context.Context, tx *sql.Tx, review domain.ReviewRequest) (domain.ReviewRequest, error) {
	var hash string
	var locked bool
	err := tx.QueryRowContext(ctx, `
		SELECT definition_hash, locked_at IS NOT NULL FROM migration_revisions
		WHERE id = $1 FOR SHARE`, review.RevisionID).Scan(&hash, &locked)
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	if !locked || hash != review.DefinitionHash {
		return domain.ReviewRequest{}, admin.ErrConflict
	}
	action := review.Action
	if action == "" {
		action = domain.MigrationActionApply
	}
	if !action.Valid() {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	created := domain.ReviewRequest{
		RevisionID:        review.RevisionID,
		Action:            action,
		TargetExecutionID: review.TargetExecutionID,
		DefinitionHash:    hash,
		Status:            domain.ReviewStatusPending,
		RequestedBy:       review.RequestedBy,
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO review_requests (revision_id, action, target_execution_id, definition_hash, requested_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, requested_at`,
		review.RevisionID, string(action), review.TargetExecutionID, hash, sql.NullInt64{Int64: review.RequestedBy, Valid: review.RequestedBy > 0}).
		Scan(&created.ID, &created.RequestedAt)
	if err != nil {
		return domain.ReviewRequest{}, adminError(err)
	}
	return created, nil
}

func (s *ApprovalPostgresStore) ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error) {
	return listDecisions(ctx, s.db, reviewID)
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listDecisions(ctx context.Context, q queryer, reviewID int64) ([]domain.ApprovalDecision, error) {
	rows, err := q.QueryContext(ctx, `
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

func (s *ApprovalPostgresStore) ListReviewRequests(ctx context.Context, databaseID int64) ([]domain.ReviewRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT q.id, q.revision_id, q.action, q.target_execution_id::text, q.definition_hash, q.status, q.requested_by, q.requested_at, q.resolved_at
		FROM review_requests q
		JOIN migration_revisions r ON r.id = q.revision_id
		WHERE r.database_id = $1
		ORDER BY q.requested_at DESC, q.id DESC`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []domain.ReviewRequest
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, r)
	}
	return reviews, rows.Err()
}

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

var (
	_ workflow.GrantReader           = (*ApprovalPostgresStore)(nil)
	_ workflow.ReviewSubmitter       = (*ApprovalPostgresStore)(nil)
	_ workflow.ActionReviewSubmitter = (*ApprovalPostgresStore)(nil)
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
	return s.SubmitActionForReview(ctx, databaseID, revisionID, requestedBy, domain.MigrationActionApply, nil, approvers)
}

func (s *ApprovalPostgresStore) SubmitActionForReview(ctx context.Context, databaseID, revisionID, requestedBy int64, action domain.MigrationAction, targetExecutionID *string, approvers []workflow.RequiredApprover) (domain.ReviewRequest, error) {
	if len(approvers) == 0 {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if action == "" {
		action = domain.MigrationActionApply
	}
	if !action.Valid() {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if action == domain.MigrationActionRollback && (targetExecutionID == nil || *targetExecutionID == "") {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	if action != domain.MigrationActionRollback && targetExecutionID != nil {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewRequest{}, err
	}
	defer tx.Rollback()

	var rev domain.MigrationRevision
	if action == domain.MigrationActionApply {
		rev, err = lockRevisionTx(ctx, tx, databaseID, revisionID)
		if err != nil {
			return domain.ReviewRequest{}, err
		}
	} else {
		rev, err = scanRevision(tx.QueryRowContext(ctx, `
			SELECT `+revisionColumns+` FROM migration_revisions
			WHERE database_id = $1 AND id = $2 FOR SHARE`, databaseID, revisionID))
		if err != nil {
			return domain.ReviewRequest{}, adminError(err)
		}
		if rev.Status != domain.RevisionStatusApproved || rev.LockedAt == nil {
			return domain.ReviewRequest{}, admin.ErrConflict
		}
		if action == domain.MigrationActionRollback && !rev.Reversible {
			return domain.ReviewRequest{}, workflow.ErrIrreversible
		}
	}
	review, err := createReviewTx(ctx, tx, domain.ReviewRequest{
		RevisionID:        rev.ID,
		Action:            action,
		TargetExecutionID: targetExecutionID,
		DefinitionHash:    rev.DefinitionHash,
		RequestedBy:       requestedBy,
	})
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
