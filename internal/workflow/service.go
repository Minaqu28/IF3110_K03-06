package workflow

import (
	"context"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

type RevisionLister interface {
	ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error)
}

type RevisionCreator interface {
	CreateRevision(ctx context.Context, rev domain.MigrationRevision) (domain.MigrationRevision, error)
}

type MigrationCreator interface {
	CreateMigration(ctx context.Context, in explorer.MigrationInput) (explorer.Migration, error)
}

type ExecutionHistoryLister interface {
	ListExecutionHistory(ctx context.Context, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error)
}

type ReviewLister interface {
	ListReviewRequests(ctx context.Context, databaseID int64) ([]domain.ReviewRequest, error)
}

type DecisionLister interface {
	ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error)
}

// QueryService provides authorized read-only access to migration revisions and execution history.
type QueryService struct {
	access    AccessChecker
	revisions RevisionLister
	history   ExecutionHistoryLister
}

func NewQueryService(access AccessChecker, revisions RevisionLister, history ExecutionHistoryLister) *QueryService {
	return &QueryService{access: access, revisions: revisions, history: history}
}

func (s *QueryService) ListMigrationRevisions(ctx context.Context, actor domain.User, databaseID int64) ([]domain.MigrationRevision, error) {
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, databaseID); err != nil {
		return nil, err
	}
	return s.revisions.ListRevisions(ctx, databaseID)
}

func (s *QueryService) ListExecutionHistory(ctx context.Context, actor domain.User, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, databaseID); err != nil {
		return nil, err
	}
	return s.history.ListExecutionHistory(ctx, databaseID, hideRollback)
}

// JobService combines revision creation, review, submission, and job polling behind the HTTP workflow contract.
type JobService struct {
	access     AccessChecker
	reviews    *ReviewService
	lister     ReviewLister
	decisions  DecisionLister
	submit     *SubmitService
	jobs       JobStore
	revisions  RevisionCreator
	migrations MigrationCreator
	executions ExecutionReader
}

func NewJobService(access AccessChecker, reviews *ReviewService, lister ReviewLister, submit *SubmitService, jobs JobStore) *JobService {
	return &JobService{access: access, reviews: reviews, lister: lister, submit: submit, jobs: jobs}
}

func (s *JobService) WithRevisions(revisions RevisionCreator, migrations MigrationCreator) *JobService {
	s.revisions = revisions
	s.migrations = migrations
	return s
}

func (s *JobService) WithDecisions(decisions DecisionLister) *JobService {
	s.decisions = decisions
	return s
}

func (s *JobService) WithExecutions(executions ExecutionReader) *JobService {
	s.executions = executions
	return s
}

func (s *JobService) CreateRevision(ctx context.Context, actor domain.User, rev domain.MigrationRevision) (domain.MigrationRevision, error) {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, rev.DatabaseID)
	if err != nil {
		return domain.MigrationRevision{}, err
	}
	if !level.Allows(authorization.AccessViewEdit) {
		return domain.MigrationRevision{}, admin.ErrForbidden
	}
	if s.revisions == nil {
		return domain.MigrationRevision{}, admin.ErrInvalidInput
	}
	rev.Title = strings.TrimSpace(rev.Title)
	rev.UpSQL = strings.TrimSpace(rev.UpSQL)
	rev.DownSQL = strings.TrimSpace(rev.DownSQL)
	rev.IrreversibleReason = strings.TrimSpace(rev.IrreversibleReason)
	if rev.UpSQL == "" {
		return domain.MigrationRevision{}, admin.ErrInvalidInput
	}
	if rev.Reversible {
		if rev.DownSQL == "" {
			return domain.MigrationRevision{}, admin.ErrInvalidInput
		}
		rev.IrreversibleReason = ""
	} else {
		rev.DownSQL = ""
		if rev.IrreversibleReason == "" {
			return domain.MigrationRevision{}, admin.ErrInvalidInput
		}
	}
	if rev.MigrationID <= 0 {
		if s.migrations == nil {
			return domain.MigrationRevision{}, admin.ErrInvalidInput
		}
		m, err := s.migrations.CreateMigration(ctx, explorer.MigrationInput{
			DatabaseID: rev.DatabaseID,
			UserID:     actor.ID,
			Title:      rev.Title,
			SQL:        rev.UpSQL,
		})
		if err != nil {
			return domain.MigrationRevision{}, err
		}
		rev.MigrationID = m.ID
		if rev.Title == "" {
			rev.Title = m.Title
		}
	}
	rev.CreatedBy = actor.ID
	return s.revisions.CreateRevision(ctx, rev)
}

func (s *JobService) SubmitForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64) (domain.ReviewRequest, error) {
	return s.SubmitActionForReview(ctx, actor, databaseID, revisionID, domain.MigrationActionApply, nil)
}

func (s *JobService) SubmitActionForReview(ctx context.Context, actor domain.User, databaseID, revisionID int64, action domain.MigrationAction, targetExecutionID *string) (domain.ReviewRequest, error) {
	if s.reviews == nil {
		return domain.ReviewRequest{}, admin.ErrInvalidInput
	}
	return s.reviews.SubmitActionForReview(ctx, actor, databaseID, revisionID, action, targetExecutionID)
}

func (s *JobService) GetExecution(ctx context.Context, actor domain.User, executionID string) (domain.ExecutionRequest, domain.ExecutionResult, error) {
	if executionID == "" || s.executions == nil {
		return domain.ExecutionRequest{}, domain.ExecutionResult{}, admin.ErrNotFound
	}
	req, res, err := s.executions.GetExecution(ctx, executionID)
	if err != nil {
		return domain.ExecutionRequest{}, domain.ExecutionResult{}, err
	}
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, req.DatabaseID); err != nil {
		return domain.ExecutionRequest{}, domain.ExecutionResult{}, err
	}
	return req, res, nil
}

func (s *JobService) ListReviewRequests(ctx context.Context, actor domain.User, databaseID int64) ([]domain.ReviewRequest, error) {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, databaseID)
	if err != nil {
		return nil, err
	}
	if !level.Allows(authorization.AccessViewOnly) {
		return nil, admin.ErrForbidden
	}
	return s.lister.ListReviewRequests(ctx, databaseID)
}

func (s *JobService) ListReviewDecisions(ctx context.Context, actor domain.User, databaseID, reviewID int64) ([]domain.ApprovalDecision, error) {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, databaseID)
	if err != nil {
		return nil, err
	}
	if !level.Allows(authorization.AccessViewOnly) {
		return nil, admin.ErrForbidden
	}
	if s.decisions == nil {
		return nil, nil
	}
	return s.decisions.ListDecisions(ctx, reviewID)
}

func (s *JobService) DecideApproval(ctx context.Context, actor domain.User, reviewID int64, status domain.ApprovalStatus, comment string) (domain.ReviewRequest, error) {
	return s.reviews.DecideApproval(ctx, actor, reviewID, status, comment)
}

func (s *JobService) SubmitExecution(ctx context.Context, actor domain.User, req domain.ExecutionRequest) (domain.Job, error) {
	return s.submit.SubmitExecution(ctx, actor, req)
}

func (s *JobService) GetJob(ctx context.Context, actor domain.User, jobID string) (domain.Job, error) {
	if jobID == "" {
		return domain.Job{}, admin.ErrInvalidInput
	}
	job, err := s.jobs.GetJob(ctx, jobID)
	if err != nil {
		return domain.Job{}, err
	}
	if _, _, err := s.access.ConnectionWithAccess(ctx, actor, job.DatabaseID); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

type JobActions struct {
	CanResume   bool `json:"can_resume"`
	CanRollback bool `json:"can_rollback"`
	CanForward  bool `json:"can_forward"`
}

func AvailableActions(state SubmitState, execution domain.ExecutionRequest, canEdit bool) JobActions {
	if !canEdit || execution.ExecutionID == "" {
		return JobActions{}
	}
	actions := JobActions{CanResume: CanSubmit(state, execution) == nil}
	fresh := execution
	fresh.ExecutionID, fresh.JobID = "", ""
	if execution.Action == domain.MigrationActionRollback {
		forward := fresh
		forward.Action, forward.TargetExecutionID = domain.MigrationActionForward, nil
		actions.CanForward = CanSubmit(stateForAction(state, domain.MigrationActionForward, nil), forward) == nil
		return actions
	}
	rollback := fresh
	target := execution.ExecutionID
	rollback.Action, rollback.TargetExecutionID = domain.MigrationActionRollback, &target
	actions.CanRollback = CanSubmit(stateForAction(state, domain.MigrationActionRollback, &target), rollback) == nil
	return actions
}

func stateForAction(state SubmitState, action domain.MigrationAction, target *string) SubmitState {
	if state.Review == nil {
		return state
	}
	revCopy := *state.Review
	revCopy.Action = action
	revCopy.TargetExecutionID = target
	state.Review = &revCopy
	return state
}

type ExecutionReader interface {
	GetExecution(ctx context.Context, executionID string) (domain.ExecutionRequest, domain.ExecutionResult, error)
}

type ActionAdvisor struct {
	access     AccessChecker
	executions ExecutionReader
	states     RunStateReader
}

func NewActionAdvisor(access AccessChecker, executions ExecutionReader, states RunStateReader) *ActionAdvisor {
	return &ActionAdvisor{access: access, executions: executions, states: states}
}

func (a *ActionAdvisor) Actions(ctx context.Context, actor domain.User, job domain.Job) (JobActions, error) {
	_, level, err := a.access.ConnectionWithAccess(ctx, actor, job.DatabaseID)
	if err != nil || !level.Allows(authorization.AccessViewEdit) {
		return JobActions{}, nil
	}
	execution, _, err := a.executions.GetExecution(ctx, job.ExecutionID)
	if err != nil {
		return JobActions{}, err
	}
	execution.ActorID = actor.ID
	_, state, err := a.states.LoadRunState(ctx, execution)
	if err != nil {
		return JobActions{}, err
	}
	return AvailableActions(state, execution, true), nil
}

// ApprovalStore menyimpan review request dan keputusan setiap leader wajib.
type ApprovalStore interface {
	// CreateReview menyimpan review baru yang terikat pada revision tertentu dan
	// mengembalikan data lengkapnya, termasuk ID yang dibuat storage.
	CreateReview(ctx context.Context, review domain.ReviewRequest) (domain.ReviewRequest, error)

	// ListDecisions mengambil keputusan seluruh leader untuk satu review agar
	// service dapat menentukan apakah approval sudah lengkap.
	ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error)

	// SaveDecision menyimpan keputusan satu leader. Pemeriksaan apakah actor
	// memang leader yang berhak tetap dilakukan oleh service workflow.
	SaveDecision(ctx context.Context, decision domain.ApprovalDecision) (domain.ApprovalDecision, error)
}

// JobStore adalah sumber status job persisten untuk workflow dan polling UI.
type JobStore interface {
	// CreateJob menyimpan job sebelum pesan dipublish agar kegagalan publish dapat
	// dipulihkan dan status job tetap dapat diaudit.
	CreateJob(ctx context.Context, job domain.Job) error

	// GetJob mengambil status job terbaru berdasarkan job ID untuk polling dan
	// proses recovery.
	GetJob(ctx context.Context, jobID string) (domain.Job, error)
}

// JobPublisher adalah batas minimal workflow menuju message queue.
type JobPublisher interface {
	// Publish mengirim referensi job yang sudah tersimpan. Implementasi nantinya
	// tidak boleh memasukkan SQL, password, cookie, atau secret ke pesan.
	Publish(ctx context.Context, message domain.MigrationJobMessage) error
}
