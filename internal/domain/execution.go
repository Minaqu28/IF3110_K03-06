package domain

import "time"

type ExecutionStatus string

const (
	ExecutionStatusPending   ExecutionStatus = "PENDING"
	ExecutionStatusRunning   ExecutionStatus = "RUNNING"
	ExecutionStatusSucceeded ExecutionStatus = "SUCCEEDED"
	ExecutionStatusFailed    ExecutionStatus = "FAILED"
	ExecutionStatusPartial   ExecutionStatus = "PARTIAL"
	ExecutionStatusUnknown   ExecutionStatus = "UNKNOWN"
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
	AttemptID     int64           `json:"attempt_id"`
	ExecutionID   string          `json:"execution_id"`
	AttemptNumber int             `json:"attempt_number"`
	Status        ExecutionStatus `json:"status"`
	SQLSnapshot   string          `json:"sql_snapshot"`
	ErrorCode     string          `json:"error_code,omitempty"`
	ErrorMessage  string          `json:"error_message,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
}
