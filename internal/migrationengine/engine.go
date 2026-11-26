package migrationengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	ErrorCodeDefinitionChanged = "DEFINITION_CHANGED"
	ErrorCodeInvalidBatch      = "INVALID_BATCH"
	ErrorCodeTargetError       = "TARGET_ERROR"
	ErrorCodeTransientError    = "TRANSIENT_ERROR"
	ErrorCodeCommitUncertain   = "COMMIT_UNCERTAIN"
)

var ErrAttemptNotRecorded = errors.New("the execution attempt could not be recorded")

type Engine struct {
	revisions RevisionStore
	history   HistoryStore
	target    TargetExecutor
	now       func() time.Time
}

func NewEngine(revisions RevisionStore, history HistoryStore, target TargetExecutor) *Engine {
	return &Engine{revisions: revisions, history: history, target: target, now: time.Now}
}

func (e *Engine) Execute(ctx context.Context, req domain.ExecutionRequest) (domain.ExecutionResult, error) {
	started := e.now()
	rev, err := e.revisions.GetRevision(ctx, req.DatabaseID, req.RevisionID)
	if err != nil {
		return domain.ExecutionResult{}, err
	}
	var (
		result domain.ExecutionResult
		runErr error
	)
	if refusal := checkRevision(rev); refusal.ErrorCode != "" {
		result = e.finish(refusal, req, started)
	} else {
		result, runErr = e.target.Execute(ctx, req, rev)
	}
	if err := e.history.AppendAttempt(context.WithoutCancel(ctx), e.attemptFor(req, rev, result, runErr, started)); err != nil {
		return result, errors.Join(runErr, fmt.Errorf("%w: %w", ErrAttemptNotRecorded, err))
	}
	return result, runErr
}

func (e *Engine) attemptFor(req domain.ExecutionRequest, rev domain.MigrationRevision, result domain.ExecutionResult, runErr error, started time.Time) domain.ExecutionAttempt {
	attempt := domain.ExecutionAttempt{
		ExecutionID: req.ExecutionID, Status: result.Status, SQLSnapshot: rev.UpSQL,
		ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage, StartedAt: started, FinishedAt: result.FinishedAt,
	}
	if req.Action == domain.MigrationActionRollback {
		attempt.SQLSnapshot = rev.DownSQL
	}
	if !result.StartedAt.IsZero() {
		attempt.StartedAt = result.StartedAt
	}
	if runErr != nil {
		attempt.Status, attempt.ErrorCode, attempt.ErrorMessage = domain.ExecutionStatusFailed, ErrorCodeTargetError, runErr.Error()
		switch ClassifyError(runErr) {
		case ErrorTransient:
			attempt.ErrorCode = ErrorCodeTransientError
		case ErrorUncertain:
			attempt.Status, attempt.ErrorCode = domain.ExecutionStatusUnknown, ErrorCodeCommitUncertain
		}
	}
	if attempt.FinishedAt == nil {
		finished := e.now()
		attempt.FinishedAt = &finished
	}
	return attempt
}

func checkRevision(rev domain.MigrationRevision) domain.ExecutionResult {
	if rev.DefinitionHash == "" || domain.ComputeDefinitionHash(rev) != rev.DefinitionHash {
		return domain.ExecutionResult{
			Status: domain.ExecutionStatusFailed, ErrorCode: ErrorCodeDefinitionChanged,
			ErrorMessage: "the stored revision no longer matches its approved definition hash",
		}
	}
	if rev.Batch != nil {
		if err := ValidateBatch(rev); err != nil {
			return domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: ErrorCodeInvalidBatch, ErrorMessage: err.Error()}
		}
	}
	return domain.ExecutionResult{}
}

func (e *Engine) finish(result domain.ExecutionResult, req domain.ExecutionRequest, started time.Time) domain.ExecutionResult {
	finished := e.now()
	result.ExecutionID, result.RevertsExecutionID = req.ExecutionID, req.TargetExecutionID
	result.StartedAt, result.FinishedAt = started, &finished
	return result
}
