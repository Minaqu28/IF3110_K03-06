package httpserver

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

var errUnexpectedMigrationMockCall = errors.New("unexpected migration service mock call")

type mockMigrationQueryService struct {
	listRevisions func(context.Context, domain.User, int64) ([]domain.MigrationRevision, error)
	listHistory   func(context.Context, domain.User, int64, bool) ([]domain.ExecutionAttempt, error)
}

var _ MigrationQueryService = (*mockMigrationQueryService)(nil)

func (m *mockMigrationQueryService) ListMigrationRevisions(ctx context.Context, actor domain.User, databaseID int64) ([]domain.MigrationRevision, error) {
	if m.listRevisions == nil {
		return nil, errUnexpectedMigrationMockCall
	}
	return m.listRevisions(ctx, actor, databaseID)
}

func (m *mockMigrationQueryService) ListExecutionHistory(ctx context.Context, actor domain.User, databaseID int64, hideRollback bool) ([]domain.ExecutionAttempt, error) {
	if m.listHistory == nil {
		return nil, errUnexpectedMigrationMockCall
	}
	return m.listHistory(ctx, actor, databaseID, hideRollback)
}
