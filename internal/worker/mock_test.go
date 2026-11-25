package worker

import (
	"context"
	"errors"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

var errUnexpectedWorkerMockCall = errors.New("unexpected worker mock call")

type mockExecutionAuthorizer struct {
	authorize func(context.Context, domain.ExecutionRequest) error
}

var _ ExecutionAuthorizer = (*mockExecutionAuthorizer)(nil)

func (m *mockExecutionAuthorizer) AuthorizeExecution(ctx context.Context, request domain.ExecutionRequest) error {
	if m.authorize == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.authorize(ctx, request)
}

type mockMigrationExecutor struct {
	execute func(context.Context, domain.ExecutionRequest) (domain.ExecutionResult, error)
}

var _ MigrationExecutor = (*mockMigrationExecutor)(nil)

func (m *mockMigrationExecutor) Execute(ctx context.Context, request domain.ExecutionRequest) (domain.ExecutionResult, error) {
	if m.execute == nil {
		return domain.ExecutionResult{}, errUnexpectedWorkerMockCall
	}
	return m.execute(ctx, request)
}

type mockJobStateStore struct {
	getJob  func(context.Context, string) (domain.Job, error)
	saveJob func(context.Context, domain.Job) error
}

var _ JobStateStore = (*mockJobStateStore)(nil)

func (m *mockJobStateStore) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	if m.getJob == nil {
		return domain.Job{}, errUnexpectedWorkerMockCall
	}
	return m.getJob(ctx, jobID)
}

func (m *mockJobStateStore) SaveJob(ctx context.Context, job domain.Job) error {
	if m.saveJob == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.saveJob(ctx, job)
}

type mockJobDelivery struct {
	message     domain.MigrationJobMessage
	redelivered bool
	ack         func() error
	requeue     func() error
	reject      func() error
}

var _ JobDelivery = (*mockJobDelivery)(nil)

func (m *mockJobDelivery) Message() domain.MigrationJobMessage { return m.message }
func (m *mockJobDelivery) Redelivered() bool                   { return m.redelivered }

func (m *mockJobDelivery) Ack() error {
	if m.ack == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.ack()
}

func (m *mockJobDelivery) Requeue() error {
	if m.requeue == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.requeue()
}

func (m *mockJobDelivery) Reject() error {
	if m.reject == nil {
		return errUnexpectedWorkerMockCall
	}
	return m.reject()
}

type mockJobConsumer struct {
	receive func(context.Context) (JobDelivery, error)
}

var _ JobConsumer = (*mockJobConsumer)(nil)

func (m *mockJobConsumer) Receive(ctx context.Context) (JobDelivery, error) {
	if m.receive == nil {
		return nil, errUnexpectedWorkerMockCall
	}
	return m.receive(ctx)
}
