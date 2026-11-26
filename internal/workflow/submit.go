package workflow

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type BatchScope struct {
	Min int64
	Max int64
}

type Submission struct {
	Request      domain.ExecutionRequest
	JobID        string
	Scope        *BatchScope
	TraceContext map[string]string
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
	result, err := s.submitter.SubmitExecution(ctx, Submission{
		Request:      req,
		JobID:        jobID,
		Scope:        scope,
		TraceContext: observability.InjectTraceContext(ctx),
	}, func(state SubmitState) error {
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
	ctx, span := observability.Tracer().Start(ctx, "queue.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("job.id", result.Job.ID),
			attribute.String("execution.id", result.Message.ExecutionID),
			attribute.Int64("database.id", result.Message.DatabaseID),
			attribute.Int64("migration.id", result.Message.MigrationID),
			attribute.Int64("revision.id", result.Message.RevisionID),
			attribute.String("migration.action", string(result.Message.Action)),
			attribute.String("migration.mode", string(result.Message.Mode)),
		),
	)
	defer span.End()

	msg := result.Message
	msg.TraceContext = observability.InjectTraceContext(ctx)

	if err := s.publisher.Publish(ctx, msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "publish migration job failed; outbox relay will retry",
			slog.String("event", "queue.publish"),
			slog.String("status", "error"),
			slog.String("job_id", result.Job.ID),
			slog.String("execution_id", msg.ExecutionID),
			slog.Int64("database_id", msg.DatabaseID),
			slog.String("error", err.Error()),
		)
		return
	}
	if err := s.marker.MarkPublished(ctx, result.Job.ID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "mark migration job published failed",
			slog.String("event", "queue.mark_published"),
			slog.String("status", "error"),
			slog.String("job_id", result.Job.ID),
			slog.String("execution_id", msg.ExecutionID),
			slog.String("error", err.Error()),
		)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "migration job published",
		slog.String("event", "queue.publish"),
		slog.String("status", "ok"),
		slog.String("job_id", result.Job.ID),
		slog.String("execution_id", msg.ExecutionID),
		slog.Int64("database_id", msg.DatabaseID),
	)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
