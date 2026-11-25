package domain

import "time"

const MigrationJobSchemaVersion = 1

type JobStatus string

const (
	JobStatusQueued              JobStatus = "QUEUED"
	JobStatusRunning             JobStatus = "RUNNING"
	JobStatusRetryWait           JobStatus = "RETRY_WAIT"
	JobStatusSucceeded           JobStatus = "SUCCEEDED"
	JobStatusFailed              JobStatus = "FAILED"
	JobStatusNeedsReconciliation JobStatus = "NEEDS_RECONCILIATION"
)

type BatchStatus string

const (
	BatchStatusPending     BatchStatus = "PENDING"
	BatchStatusRunning     BatchStatus = "RUNNING"
	BatchStatusCommitted   BatchStatus = "COMMITTED"
	BatchStatusFailed      BatchStatus = "FAILED"
	BatchStatusCompensated BatchStatus = "COMPENSATED"
)

type BatchProgress struct {
	Status       BatchStatus `json:"status"`
	Completed    int         `json:"completed"`
	Total        *int        `json:"total,omitempty"`
	LastCursor   string      `json:"last_cursor,omitempty"`
	AffectedRows int64       `json:"affected_rows"`
}

type Job struct {
	ID           string         `json:"id"`
	ExecutionID  string         `json:"execution_id"`
	DatabaseID   int64          `json:"database_id"`
	Status       JobStatus      `json:"status"`
	AttemptCount int            `json:"attempt_count"`
	Progress     *BatchProgress `json:"progress,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type MigrationJobMessage struct {
	SchemaVersion     int             `json:"schema_version"`
	JobID             string          `json:"job_id"`
	ExecutionID       string          `json:"execution_id"`
	DatabaseID        int64           `json:"database_id"`
	MigrationID       int64           `json:"migration_id"`
	RevisionID        int64           `json:"revision_id"`
	ApprovalRequestID int64           `json:"approval_request_id"`
	ActorID           int64           `json:"actor_id"`
	Action            MigrationAction `json:"action"`
	Mode              ExecutionMode   `json:"mode"`
	TargetExecutionID *string         `json:"target_execution_id,omitempty"`
	RequestedAt       time.Time       `json:"requested_at"`
}
