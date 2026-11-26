package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
)

type SubmitPostgresStore struct {
	db *sql.DB
}

var _ workflow.ExecutionSubmitter = (*SubmitPostgresStore)(nil)

func NewSubmitPostgresStore(db *sql.DB) *SubmitPostgresStore {
	return &SubmitPostgresStore{db: db}
}

func (s *SubmitPostgresStore) SubmitExecution(ctx context.Context, sub workflow.Submission, check func(workflow.SubmitState) error) (workflow.SubmitResult, error) {
	req := sub.Request
	if sub.JobID == "" {
		return workflow.SubmitResult{}, admin.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return workflow.SubmitResult{}, err
	}
	defer tx.Rollback()
	var locked int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM database_connections WHERE id = $1 FOR UPDATE`, req.DatabaseID).Scan(&locked); err != nil {
		return workflow.SubmitResult{}, adminError(err)
	}
	if replay, found, err := replayedJob(ctx, tx, sub); err != nil || found {
		return replay, err
	}
	state, err := loadSubmitState(ctx, tx, req)
	if err != nil {
		return workflow.SubmitResult{}, err
	}
	if err := check(state); err != nil {
		return workflow.SubmitResult{}, err
	}
	execution := req
	if req.ExecutionID == "" {
		if execution, err = createExecution(ctx, tx, req, sub.Scope); err != nil {
			return workflow.SubmitResult{}, err
		}
	} else {
		var open bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM migration_jobs
			               WHERE execution_id = $1 AND status NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED'))`,
			req.ExecutionID).Scan(&open); err != nil {
			return workflow.SubmitResult{}, adminError(err)
		}
		if open {
			return workflow.SubmitResult{}, workflow.ErrDatabaseHeld
		}
		if err := tx.QueryRowContext(ctx, `SELECT requested_at FROM migration_executions WHERE id = $1`, req.ExecutionID).Scan(&execution.RequestedAt); err != nil {
			return workflow.SubmitResult{}, adminError(err)
		}
	}
	if err := createJob(ctx, tx, domain.Job{ID: sub.JobID, ExecutionID: execution.ExecutionID, DatabaseID: req.DatabaseID}); err != nil {
		return workflow.SubmitResult{}, err
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id = $1`, sub.JobID))
	if err != nil {
		return workflow.SubmitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return workflow.SubmitResult{}, err
	}
	return workflow.SubmitResult{Job: job, Message: jobMessage(job.ID, execution), Created: true}, nil
}

func jobMessage(jobID string, e domain.ExecutionRequest) domain.MigrationJobMessage {
	return domain.MigrationJobMessage{
		SchemaVersion: domain.MigrationJobSchemaVersion, JobID: jobID, ExecutionID: e.ExecutionID,
		DatabaseID: e.DatabaseID, MigrationID: e.MigrationID, RevisionID: e.RevisionID,
		ApprovalRequestID: e.ApprovalRequestID, ActorID: e.ActorID, Action: e.Action, Mode: e.Mode,
		TargetExecutionID: e.TargetExecutionID, RequestedAt: e.RequestedAt,
	}
}

func loadSubmitState(ctx context.Context, tx dbtx, req domain.ExecutionRequest) (workflow.SubmitState, error) {
	var state workflow.SubmitState
	rev, err := scanRevision(tx.QueryRowContext(ctx, `
		SELECT `+revisionColumns+` FROM migration_revisions
		WHERE id = $1 AND database_id = $2`, req.RevisionID, req.DatabaseID))
	if err != nil {
		return state, adminError(err)
	}
	state.Revision = rev
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT max(revision) FROM migration_revisions WHERE migration_id = m.id), m.version
		FROM migrations m WHERE m.id = $1`, rev.MigrationID).Scan(&state.LatestRevision, &state.MigrationVersion); err != nil {
		return state, adminError(err)
	}
	review, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM review_requests WHERE id = $1`, req.ApprovalRequestID))
	if err == nil {
		state.Review = &review
	} else if !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.migration_id, m.version, e.revision_id, e.action, e.mode, e.status,
		       COALESCE(e.target_execution_id::text, ''), e.requested_at
		FROM migration_executions e
		JOIN migrations m ON m.id = e.migration_id
		WHERE e.database_id = $1`, req.DatabaseID)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var e workflow.ExecutionSummary
		var action, mode, status string
		if err := rows.Scan(&e.ID, &e.MigrationID, &e.MigrationVersion, &e.RevisionID, &action, &mode, &status,
			&e.TargetExecutionID, &e.RequestedAt); err != nil {
			return state, err
		}
		e.Action, e.Mode, e.Status = domain.MigrationAction(action), domain.ExecutionMode(mode), domain.ExecutionStatus(status)
		state.Executions = append(state.Executions, e)
	}
	return state, rows.Err()
}
