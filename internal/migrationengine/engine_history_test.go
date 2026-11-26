package migrationengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestEngineRecordsExactlyOneAttemptPerRun(t *testing.T) {
	cases := []struct {
		name       string
		action     domain.MigrationAction
		setup      func(*engineHarness)
		wantStatus domain.ExecutionStatus
		wantCode   string
		wantSQL    func(domain.MigrationRevision) string
	}{
		{"success", domain.MigrationActionApply, nil, domain.ExecutionStatusSucceeded, "",
			func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"rollback snapshots the down SQL", domain.MigrationActionRollback, nil, domain.ExecutionStatusSucceeded, "",
			func(r domain.MigrationRevision) string { return r.DownSQL }},
		{"sql failure", domain.MigrationActionApply, func(h *engineHarness) {
			h.targetRes = domain.ExecutionResult{Status: domain.ExecutionStatusFailed, ErrorCode: "42P01", ErrorMessage: "missing"}
		}, domain.ExecutionStatusFailed, "42P01", func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"partial batch", domain.MigrationActionApply, func(h *engineHarness) {
			h.targetRes = domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: "22012", AppliedBatches: 6}
		}, domain.ExecutionStatusPartial, "22012", func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"changed definition", domain.MigrationActionApply, func(h *engineHarness) { h.rev.UpSQL = "DROP TABLE t" },
			domain.ExecutionStatusFailed, ErrorCodeDefinitionChanged, func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"busy target is a transient failure", domain.MigrationActionApply, func(h *engineHarness) { h.targetErr = ErrTargetBusy },
			domain.ExecutionStatusFailed, ErrorCodeTransientError, func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"permanent target error", domain.MigrationActionApply, func(h *engineHarness) {
			h.targetErr = fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"})
		}, domain.ExecutionStatusFailed, ErrorCodeTargetError, func(r domain.MigrationRevision) string { return r.UpSQL }},
		{"uncertain commit", domain.MigrationActionApply, func(h *engineHarness) {
			h.targetErr = fmt.Errorf("%w: eof", ErrCommitUncertain)
		}, domain.ExecutionStatusUnknown, ErrorCodeCommitUncertain, func(r domain.MigrationRevision) string { return r.UpSQL }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newEngineHarness()
			if c.setup != nil {
				c.setup(h)
			}
			_, err := h.engine.Execute(context.Background(), engineRequest(c.action))
			if (err != nil) != (h.targetErr != nil) {
				t.Fatalf("err = %v", err)
			}
			if len(h.attempts) != 1 {
				t.Fatalf("%d attempts recorded, want exactly 1", len(h.attempts))
			}
			a := h.attempts[0]
			if a.ExecutionID != "exec-1" || a.Status != c.wantStatus || a.ErrorCode != c.wantCode || a.SQLSnapshot != c.wantSQL(h.rev) ||
				a.StartedAt.IsZero() || a.FinishedAt == nil || a.FinishedAt.Before(a.StartedAt) {
				t.Fatalf("attempt = %+v", a)
			}
			if h.targetErr != nil && a.ErrorMessage == "" {
				t.Fatal("the target error message was not recorded")
			}
		})
	}
}

func TestEngineRetriesRecordOneAttemptEach(t *testing.T) {
	h := newEngineHarness()
	h.targetErr = ErrTargetBusy
	_, _ = h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply))
	h.targetErr = nil
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); err != nil {
		t.Fatal(err)
	}
	if len(h.attempts) != 2 || h.attempts[0].Status != domain.ExecutionStatusFailed || h.attempts[1].Status != domain.ExecutionStatusSucceeded ||
		h.requests[0].ExecutionID != h.requests[1].ExecutionID {
		t.Fatalf("attempts = %+v", h.attempts)
	}
}

func TestEngineRecordsTheAttemptEvenWhenCancelled(t *testing.T) {
	h := newEngineHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var seen context.Context
	h.engine.history = &mockHistoryStore{appendAttempt: func(c context.Context, a domain.ExecutionAttempt) error {
		seen = c
		h.attempts = append(h.attempts, a)
		return nil
	}}
	h.engine.target = &mockTargetExecutor{execute: func(context.Context, domain.ExecutionRequest, domain.MigrationRevision) (domain.ExecutionResult, error) {
		cancel()
		return domain.ExecutionResult{Status: domain.ExecutionStatusPartial, ErrorCode: "INTERRUPTED", AppliedBatches: 2}, nil
	}}
	if _, err := h.engine.Execute(ctx, engineRequest(domain.MigrationActionApply)); err != nil {
		t.Fatal(err)
	}
	if len(h.attempts) != 1 || seen.Err() != nil {
		t.Fatal("a shutdown must not drop the attempt record")
	}
}

func TestEngineReportsAnUnrecordedAttempt(t *testing.T) {
	h := newEngineHarness()
	h.appendErr = errors.New("platform down")
	res, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply))
	if !errors.Is(err, ErrAttemptNotRecorded) || !errors.Is(err, h.appendErr) || res.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("result = %+v, err = %v; want the result plus an attempt error", res, err)
	}
	h = newEngineHarness()
	h.appendErr, h.targetErr = errors.New("platform down"), ErrTargetBusy
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); !errors.Is(err, ErrTargetBusy) || !errors.Is(err, ErrAttemptNotRecorded) {
		t.Fatalf("both errors must survive: %v", err)
	}
	h = newEngineHarness()
	h.revErr = errors.New("platform down")
	_, _ = h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply))
	if len(h.attempts) != 0 {
		t.Fatal("nothing ran, so nothing is recorded when the revision cannot be loaded")
	}
}
