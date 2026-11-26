package migrationengine

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type ProgressSink func(ctx context.Context, executionID string, progress domain.BatchProgress) error

func (s ProgressSink) Report(ctx context.Context, executionID string, progress domain.BatchProgress) error {
	if s == nil {
		return nil
	}
	return s(ctx, executionID, progress)
}
