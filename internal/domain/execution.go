package domain

import (
	"time"
)

type ExecutionStatus string

const (
	ExecutionStatusPending           ExecutionStatus = "PENDING"
	ExecutionStatusRunning           ExecutionStatus = "RUNNING"
	ExecutionStatusSucceeded         ExecutionStatus = "SUCCEEDED"
	ExecutionStatusFailed            ExecutionStatus = "FAILED"
	ExecutionStatusPartial           ExecutionStatus = "PARTIAL"
	ExecutionStatusUnknown           ExecutionStatus = "UNKNOWN"
	ExecutionStatusPartiallyReverted ExecutionStatus = "PARTIALLY_REVERTED"
)

type ExecutionRequest struct {
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

type ExecutionResult struct {
	ExecutionID        string          `json:"execution_id"`
	Status             ExecutionStatus `json:"status"`
	RevertsExecutionID *string         `json:"reverts_execution_id,omitempty"`
	AppliedBatches     int             `json:"applied_batches,omitempty"`
	AffectedRows       int64           `json:"affected_rows,omitempty"`
	ErrorCode          string          `json:"error_code,omitempty"`
	ErrorMessage       string          `json:"error_message,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
}

type ExecutionAttempt struct {
	AttemptID          int64           `json:"attempt_id"`
	ExecutionID        string          `json:"execution_id"`
	MigrationID        int64           `json:"migration_id,omitempty"`
	MigrationVersion   int             `json:"migration_version,omitempty"`
	RevisionID         int64           `json:"revision_id,omitempty"`
	Revision           int             `json:"revision,omitempty"`
	Action             MigrationAction `json:"action,omitempty"`
	Mode               ExecutionMode   `json:"mode,omitempty"`
	ActorID            int64           `json:"actor_id,omitempty"`
	ActorUsername      string          `json:"actor_username,omitempty"`
	RevertsExecutionID *string         `json:"reverts_execution_id,omitempty"`
	AttemptNumber      int             `json:"attempt_number"`
	Status             ExecutionStatus `json:"status"`
	SQLSnapshot        string          `json:"sql_snapshot"`
	ErrorCode          string          `json:"error_code,omitempty"`
	ErrorMessage       string          `json:"error_message,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
}

const MigrationJobSchemaVersion = 1

type JobStatus string

const (
	JobStatusQueued              JobStatus = "QUEUED"
	JobStatusRunning             JobStatus = "RUNNING"
	JobStatusRetryWait           JobStatus = "RETRY_WAIT"
	JobStatusSucceeded           JobStatus = "SUCCEEDED"
	JobStatusFailed              JobStatus = "FAILED"
	JobStatusNeedsReconciliation JobStatus = "NEEDS_RECONCILIATION"
	JobStatusCancelled           JobStatus = "CANCELLED"
)

func (s JobStatus) Terminal() bool {
	switch s {
	case JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
		return true
	}
	return false
}

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
	SchemaVersion     int               `json:"schema_version"`
	JobID             string            `json:"job_id"`
	ExecutionID       string            `json:"execution_id"`
	DatabaseID        int64             `json:"database_id"`
	MigrationID       int64             `json:"migration_id"`
	RevisionID        int64             `json:"revision_id"`
	ApprovalRequestID int64             `json:"approval_request_id"`
	ActorID           int64             `json:"actor_id"`
	Action            MigrationAction   `json:"action"`
	Mode              ExecutionMode     `json:"mode"`
	TargetExecutionID *string           `json:"target_execution_id,omitempty"`
	RequestedAt       time.Time         `json:"requested_at"`
	TraceContext      map[string]string `json:"trace_context,omitempty"`
}
