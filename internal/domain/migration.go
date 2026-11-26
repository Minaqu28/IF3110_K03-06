package domain

import "time"

type MigrationAction string

const (
	MigrationActionApply    MigrationAction = "APPLY"
	MigrationActionRollback MigrationAction = "ROLLBACK"
	MigrationActionForward  MigrationAction = "FORWARD"
)

type ExecutionMode string

const (
	ExecutionModeNormal ExecutionMode = "NORMAL"
	ExecutionModeBatch  ExecutionMode = "BATCH"
)

type RevisionStatus string

const (
	RevisionStatusDraft    RevisionStatus = "DRAFT"
	RevisionStatusInReview RevisionStatus = "IN_REVIEW"
	RevisionStatusApproved RevisionStatus = "APPROVED"
	RevisionStatusRejected RevisionStatus = "REJECTED"
)

type BatchConfig struct {
	Table        string `json:"table"`
	CursorColumn string `json:"cursor_column"`
	BatchSize    int    `json:"batch_size"`
}

type MigrationRevision struct {
	ID                 int64          `json:"id"`
	MigrationID        int64          `json:"migration_id"`
	DatabaseID         int64          `json:"database_id"`
	Revision           int            `json:"revision"`
	Title              string         `json:"title"`
	UpSQL              string         `json:"up_sql"`
	DownSQL            string         `json:"down_sql,omitempty"`
	Reversible         bool           `json:"reversible"`
	IrreversibleReason string         `json:"irreversible_reason,omitempty"`
	Status             RevisionStatus `json:"status"`
	DefinitionHash     string         `json:"definition_hash"`
	CreatedBy          int64          `json:"created_by"`
	CreatedAt          time.Time      `json:"created_at"`
	LockedAt           *time.Time     `json:"locked_at,omitempty"`
	Batch              *BatchConfig   `json:"batch,omitempty"`
}
