package worker

import (
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

const (
	ErrorCodeTransient        = "TRANSIENT_ERROR"
	ErrorCodeTarget           = "TARGET_ERROR"
	ErrorCodeCommitUncertain  = "COMMIT_UNCERTAIN"
	ErrorCodeUnexpectedResult = "UNEXPECTED_RESULT"
	errorCodeInterrupted      = "INTERRUPTED"
)

func outcome(job domain.Job, msg domain.MigrationJobMessage, result domain.ExecutionResult, err error) domain.Job {
	job.ErrorCode, job.ErrorMessage = "", ""
	if err != nil {
		job.ErrorMessage = err.Error()
		switch migrationengine.ClassifyError(err) {
		case migrationengine.ErrorTransient:
			job.Status, job.ErrorCode = domain.JobStatusRetryWait, ErrorCodeTransient
		case migrationengine.ErrorUncertain:
			job.Status, job.ErrorCode = domain.JobStatusNeedsReconciliation, ErrorCodeCommitUncertain
		default:
			job.Status, job.ErrorCode = domain.JobStatusFailed, ErrorCodeTarget
		}
		return job
	}
	job.ErrorCode, job.ErrorMessage = result.ErrorCode, result.ErrorMessage
	if msg.Mode == domain.ExecutionModeBatch {
		job.Progress = progressOf(job.Progress, msg.Action, result)
	}
	switch result.Status {
	case domain.ExecutionStatusSucceeded:
		job.Status = domain.JobStatusSucceeded
	case domain.ExecutionStatusUnknown:
		job.Status = domain.JobStatusNeedsReconciliation
	case domain.ExecutionStatusFailed, domain.ExecutionStatusPartial, domain.ExecutionStatusPartiallyReverted:
		job.Status = domain.JobStatusFailed
		if retryable(result.ErrorCode) {
			job.Status = domain.JobStatusRetryWait
		}
	default:
		job.Status, job.ErrorCode = domain.JobStatusFailed, ErrorCodeUnexpectedResult
		job.ErrorMessage = "the engine returned status " + string(result.Status)
	}
	return job
}

func retryable(code string) bool {
	return code == errorCodeInterrupted || migrationengine.ClassifySQLState(code) == migrationengine.ErrorTransient
}

func progressOf(previous *domain.BatchProgress, action domain.MigrationAction, result domain.ExecutionResult) *domain.BatchProgress {
	p := &domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: result.AppliedBatches, AffectedRows: result.AffectedRows}
	if action == domain.MigrationActionRollback {
		p.Status = domain.BatchStatusCompensated
	}
	if previous != nil {
		p.Total, p.LastCursor = previous.Total, previous.LastCursor
	}
	return p
}
