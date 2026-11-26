package database

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func (s *JobPostgresStore) MarkPublished(ctx context.Context, jobID string) error {
	return rowsAffected(s.db.ExecContext(ctx, `
		UPDATE migration_jobs SET published_at = COALESCE(published_at, now())
		WHERE id = $1`, jobID))
}

func (s *JobPostgresStore) PublishDue(ctx context.Context, limit int, publish func(context.Context, domain.MigrationJobMessage) error) (int, error) {
	if limit <= 0 {
		return 0, admin.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, e.id, e.database_id, e.migration_id, e.revision_id, e.approval_request_id,
		       e.requested_by, e.action, e.mode, e.target_execution_id, e.requested_at, j.trace_context
		FROM migration_jobs j
		JOIN migration_executions e ON e.id = j.execution_id
		WHERE j.published_at IS NULL
		  AND j.status IN ('QUEUED', 'RETRY_WAIT', 'NEEDS_RECONCILIATION')
		  AND j.next_attempt_at <= now()
		ORDER BY j.next_attempt_at, j.created_at
		LIMIT $1
		FOR UPDATE OF j SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	messages, err := scanJobMessages(rows)
	if err != nil {
		return 0, err
	}
	published := 0
	var firstErr error
	for _, msg := range messages {
		if err := publish(ctx, msg); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE migration_jobs SET published_at = now() WHERE id = $1`, msg.JobID); err != nil {
			return 0, err
		}
		published++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return published, firstErr
}

func scanJobMessages(rows *sql.Rows) ([]domain.MigrationJobMessage, error) {
	defer rows.Close()
	var messages []domain.MigrationJobMessage
	for rows.Next() {
		var (
			msg          domain.MigrationJobMessage
			actor        sql.NullInt64
			action, mode string
			target       sql.NullString
			traceRaw     []byte
		)
		if err := rows.Scan(&msg.JobID, &msg.ExecutionID, &msg.DatabaseID, &msg.MigrationID, &msg.RevisionID,
			&msg.ApprovalRequestID, &actor, &action, &mode, &target, &msg.RequestedAt, &traceRaw); err != nil {
			return nil, err
		}
		msg.SchemaVersion = domain.MigrationJobSchemaVersion
		msg.ActorID = actor.Int64
		msg.Action = domain.MigrationAction(action)
		msg.Mode = domain.ExecutionMode(mode)
		if target.Valid {
			t := target.String
			msg.TargetExecutionID = &t
		}
		if len(traceRaw) > 0 {
			var tc map[string]string
			if err := json.Unmarshal(traceRaw, &tc); err == nil && len(tc) > 0 {
				msg.TraceContext = tc
			}
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}
