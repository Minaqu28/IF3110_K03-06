package workflow

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type BatchScope struct {
	Min int64
	Max int64
}

type Submission struct {
	Request domain.ExecutionRequest
	JobID   string
	Scope   *BatchScope
}

type SubmitResult struct {
	Job     domain.Job
	Message domain.MigrationJobMessage
	Created bool
}

type ExecutionSubmitter interface {
	SubmitExecution(ctx context.Context, submission Submission, check func(SubmitState) error) (SubmitResult, error)
}

type RevisionGetter interface {
	GetRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error)
}

type ScopeReader interface {
	BatchScope(ctx context.Context, databaseID int64, batch domain.BatchConfig) (BatchScope, error)
}

type PublishMarker interface {
	MarkPublished(ctx context.Context, jobID string) error
}

type SubmitService struct {
	access    AccessChecker
	revisions RevisionGetter
	scopes    ScopeReader
	submitter ExecutionSubmitter
	publisher JobPublisher
	marker    PublishMarker
	newID     func() string
}

func NewSubmitService(access AccessChecker, revisions RevisionGetter, scopes ScopeReader, submitter ExecutionSubmitter, publisher JobPublisher, marker PublishMarker) *SubmitService {
	return &SubmitService{access: access, revisions: revisions, scopes: scopes, submitter: submitter, publisher: publisher, marker: marker, newID: newUUID}
}

func (s *SubmitService) SubmitExecution(ctx context.Context, actor domain.User, req domain.ExecutionRequest) (domain.Job, error) {
	_, level, err := s.access.ConnectionWithAccess(ctx, actor, req.DatabaseID)
	if err != nil {
		return domain.Job{}, err
	}
	if !level.Allows(authorization.AccessViewEdit) {
		return domain.Job{}, admin.ErrForbidden
	}
	req.ActorID = actor.ID
	jobID := req.JobID
	if jobID == "" {
		jobID = s.newID()
	}
	req.JobID = ""
	scope, err := s.scopeFor(ctx, req)
	if err != nil {
		return domain.Job{}, err
	}
	result, err := s.submitter.SubmitExecution(ctx, Submission{Request: req, JobID: jobID, Scope: scope}, func(state SubmitState) error {
		return CanSubmit(state, req)
	})
	if err != nil {
		return domain.Job{}, err
	}
	if result.Created {
		s.publish(ctx, result)
	}
	return result.Job, nil
}

func (s *SubmitService) scopeFor(ctx context.Context, req domain.ExecutionRequest) (*BatchScope, error) {
	if req.ExecutionID != "" || req.Action == domain.MigrationActionRollback || req.Mode != domain.ExecutionModeBatch {
		return nil, nil
	}
	rev, err := s.revisions.GetRevision(ctx, req.DatabaseID, req.RevisionID)
	if err != nil {
		return nil, err
	}
	if rev.Batch == nil {
		return nil, admin.ErrInvalidInput
	}
	scope, err := s.scopes.BatchScope(ctx, req.DatabaseID, *rev.Batch)
	if err != nil {
		return nil, err
	}
	return &scope, nil
}

func (s *SubmitService) publish(ctx context.Context, result SubmitResult) {
	if err := s.publisher.Publish(ctx, result.Message); err != nil {
		log.Printf("publish migration job %s: %v (the outbox relay will retry)", result.Job.ID, err)
		return
	}
	if err := s.marker.MarkPublished(ctx, result.Job.ID); err != nil {
		log.Printf("mark migration job %s published: %v", result.Job.ID, err)
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
