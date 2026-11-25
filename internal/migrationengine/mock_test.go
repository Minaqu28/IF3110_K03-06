package migrationengine

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

var errUnexpectedMigrationEngineMockCall = errors.New("unexpected migration engine mock call")

type mockRevisionStore struct {
	getRevision   func(context.Context, int64, int64) (domain.MigrationRevision, error)
	listRevisions func(context.Context, int64) ([]domain.MigrationRevision, error)
}

var _ RevisionStore = (*mockRevisionStore)(nil)

func (m *mockRevisionStore) GetRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error) {
	if m.getRevision == nil {
		return domain.MigrationRevision{}, errUnexpectedMigrationEngineMockCall
	}
	return m.getRevision(ctx, databaseID, revisionID)
}

func (m *mockRevisionStore) ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error) {
	if m.listRevisions == nil {
		return nil, errUnexpectedMigrationEngineMockCall
	}
	return m.listRevisions(ctx, databaseID)
}

type mockHistoryStore struct {
	appendAttempt func(context.Context, domain.ExecutionAttempt) error
	listAttempts  func(context.Context, int64) ([]domain.ExecutionAttempt, error)
}

var _ HistoryStore = (*mockHistoryStore)(nil)

func (m *mockHistoryStore) AppendAttempt(ctx context.Context, attempt domain.ExecutionAttempt) error {
	if m.appendAttempt == nil {
		return errUnexpectedMigrationEngineMockCall
	}
	return m.appendAttempt(ctx, attempt)
}

func (m *mockHistoryStore) ListAttempts(ctx context.Context, databaseID int64) ([]domain.ExecutionAttempt, error) {
	if m.listAttempts == nil {
		return nil, errUnexpectedMigrationEngineMockCall
	}
	return m.listAttempts(ctx, databaseID)
}

type mockTargetExecutor struct {
	execute func(context.Context, domain.ExecutionRequest, domain.MigrationRevision) (domain.ExecutionResult, error)
}

var _ TargetExecutor = (*mockTargetExecutor)(nil)

func (m *mockTargetExecutor) Execute(ctx context.Context, request domain.ExecutionRequest, revision domain.MigrationRevision) (domain.ExecutionResult, error) {
	if m.execute == nil {
		return domain.ExecutionResult{}, errUnexpectedMigrationEngineMockCall
	}
	return m.execute(ctx, request, revision)
}
