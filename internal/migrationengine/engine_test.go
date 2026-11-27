package migrationengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

type engineHarness struct {
	rev       domain.MigrationRevision
	revErr    error
	targetRes domain.ExecutionResult
	targetErr error
	executed  []domain.MigrationRevision
	requests  []domain.ExecutionRequest
	attempts  []domain.ExecutionAttempt
	appendErr error
	engine    *Engine
}

func hashed(rev domain.MigrationRevision) domain.MigrationRevision {
	rev.DefinitionHash = domain.ComputeDefinitionHash(rev)
	return rev
}

func newEngineHarness() *engineHarness {
	h := &engineHarness{
		rev: hashed(domain.MigrationRevision{
			ID: 7, MigrationID: 3, DatabaseID: 1, UpSQL: "CREATE TABLE t (id int)", DownSQL: "DROP TABLE t", Reversible: true,
		}),
		targetRes: domain.ExecutionResult{ExecutionID: "exec-1", Status: domain.ExecutionStatusSucceeded},
	}
	revisions := &mockRevisionStore{getRevision: func(_ context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error) {
		if databaseID != h.rev.DatabaseID || revisionID != h.rev.ID {
			return domain.MigrationRevision{}, admin.ErrNotFound
		}
		return h.rev, h.revErr
	}}
	history := &mockHistoryStore{appendAttempt: func(_ context.Context, a domain.ExecutionAttempt) error {
		h.attempts = append(h.attempts, a)
		return h.appendErr
	}}
	target := &mockTargetExecutor{execute: func(_ context.Context, req domain.ExecutionRequest, rev domain.MigrationRevision) (domain.ExecutionResult, error) {
		h.requests, h.executed = append(h.requests, req), append(h.executed, rev)
		return h.targetRes, h.targetErr
	}}
	h.engine = NewEngine(revisions, history, target)
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h.engine.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	return h
}

func engineRequest(action domain.MigrationAction) domain.ExecutionRequest {
	req := domain.ExecutionRequest{ExecutionID: "exec-1", DatabaseID: 1, MigrationID: 3, RevisionID: 7, ActorID: 5, Action: action, Mode: domain.ExecutionModeNormal}
	if action == domain.MigrationActionRollback {
		target := "exec-0"
		req.TargetExecutionID = &target
	}
	return req
}

func TestEngineRunsTheStoredRevision(t *testing.T) {
	h := newEngineHarness()
	req := engineRequest(domain.MigrationActionApply)
	res, err := h.engine.Execute(context.Background(), req)
	if err != nil || res.Status != domain.ExecutionStatusSucceeded {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if len(h.executed) != 1 || h.executed[0].UpSQL != h.rev.UpSQL || h.requests[0] != req {
		t.Fatalf("target got %+v / %+v", h.executed, h.requests)
	}
}

func TestEngineRefusesAChangedDefinition(t *testing.T) {
	for name, mutate := range map[string]func(*domain.MigrationRevision){
		"up SQL edited":   func(r *domain.MigrationRevision) { r.UpSQL = "DROP TABLE everything" },
		"down SQL edited": func(r *domain.MigrationRevision) { r.DownSQL = "SELECT 1" },
		"batch size edited": func(r *domain.MigrationRevision) {
			r.Batch = &domain.BatchConfig{Table: "t", CursorColumn: "id", BatchSize: 1}
		},
		"no hash": func(r *domain.MigrationRevision) { r.DefinitionHash = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness()
			mutate(&h.rev)
			res, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionRollback))
			if err != nil || res.Status != domain.ExecutionStatusFailed || res.ErrorCode != ErrorCodeDefinitionChanged ||
				res.ExecutionID != "exec-1" || res.RevertsExecutionID == nil || res.FinishedAt == nil || !res.FinishedAt.After(res.StartedAt) {
				t.Fatalf("result = %+v, err = %v", res, err)
			}
			if len(h.executed) != 0 {
				t.Fatal("a changed definition reached the target")
			}
		})
	}
}

func TestEngineRefusesAnInvalidBatch(t *testing.T) {
	h := newEngineHarness()
	h.rev.UpSQL = "UPDATE t SET a = 1"
	h.rev.DownSQL = "UPDATE t SET a = 0 WHERE id > $1 AND id <= $2"
	h.rev.Batch = &domain.BatchConfig{Table: "t", CursorColumn: "id", BatchSize: 10}
	h.rev = hashed(h.rev)
	req := engineRequest(domain.MigrationActionApply)
	req.Mode = domain.ExecutionModeBatch
	res, err := h.engine.Execute(context.Background(), req)
	if err != nil || res.ErrorCode != ErrorCodeInvalidBatch || len(h.executed) != 0 {
		t.Fatalf("result = %+v, err = %v, executed %d", res, err, len(h.executed))
	}
}

func TestEnginePassesErrorsThrough(t *testing.T) {
	h := newEngineHarness()
	h.revErr = errors.New("platform down")
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); !errors.Is(err, h.revErr) {
		t.Fatalf("revision load error = %v", err)
	}
	h = newEngineHarness()
	req := engineRequest(domain.MigrationActionApply)
	req.RevisionID = 99
	if _, err := h.engine.Execute(context.Background(), req); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown revision = %v", err)
	}
	h = newEngineHarness()
	h.targetErr = ErrTargetBusy
	if _, err := h.engine.Execute(context.Background(), engineRequest(domain.MigrationActionApply)); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("target error = %v", err)
	}
}

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

func TestProgressSinkReport(t *testing.T) {
	var gotID string
	var gotProgress domain.BatchProgress
	sink := ProgressSink(func(_ context.Context, id string, p domain.BatchProgress) error {
		gotID, gotProgress = id, p
		return nil
	})
	want := domain.BatchProgress{Status: domain.BatchStatusCommitted, Completed: 3, LastCursor: "3000", AffectedRows: 3000}

	if err := sink.Report(context.Background(), "exec-1", want); err != nil {
		t.Fatal(err)
	}
	if gotID != "exec-1" || gotProgress != want {
		t.Fatalf("sink got (%q, %+v), want (%q, %+v)", gotID, gotProgress, "exec-1", want)
	}
}

func TestProgressSinkReportReturnsError(t *testing.T) {
	boom := errors.New("boom")
	sink := ProgressSink(func(context.Context, string, domain.BatchProgress) error { return boom })
	if err := sink.Report(context.Background(), "exec-1", domain.BatchProgress{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestNilProgressSinkIsNoop(t *testing.T) {
	var sink ProgressSink
	if err := sink.Report(context.Background(), "exec-1", domain.BatchProgress{}); err != nil {
		t.Fatalf("nil sink returned %v", err)
	}
}

func TestClassifySQLState(t *testing.T) {
	cases := map[string]ErrorClass{
		"40001": ErrorTransient,
		"40P01": ErrorTransient,
		"55P03": ErrorTransient,
		"57P01": ErrorTransient,
		"57P02": ErrorTransient,
		"57P03": ErrorTransient,
		"53300": ErrorTransient,
		"08000": ErrorTransient,
		"08003": ErrorTransient,
		"08006": ErrorTransient,
		"08P01": ErrorTransient,
		"42P01": ErrorPermanent,
		"42601": ErrorPermanent,
		"23505": ErrorPermanent,
		"23503": ErrorPermanent,
		"22P02": ErrorPermanent,
		"57014": ErrorPermanent,
		"28P01": ErrorPermanent,
		"53100": ErrorPermanent,
		"0800":  ErrorPermanent,
		"":      ErrorPermanent,
	}
	for code, want := range cases {
		if got := ClassifySQLState(code); got != want {
			t.Errorf("ClassifySQLState(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrorPermanent},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, ErrorTransient},
		{"wrapped lock timeout", fmt.Errorf("batch 3: %w", &pgconn.PgError{Code: "55P03"}), ErrorTransient},
		{"syntax error", &pgconn.PgError{Code: "42601"}, ErrorPermanent},
		{"bad password on connect", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"}), ErrorPermanent},
		{"uncertain commit", fmt.Errorf("%w: %w", ErrCommitUncertain, io.EOF), ErrorUncertain},
		{"uncertain beats sqlstate", fmt.Errorf("%w: %w", ErrCommitUncertain, &pgconn.PgError{Code: "42601"}), ErrorUncertain},
		{"target busy", fmt.Errorf("batch run: %w", ErrTargetBusy), ErrorTransient},
		{"cancelled", context.Canceled, ErrorTransient},
		{"deadline", fmt.Errorf("run: %w", context.DeadlineExceeded), ErrorTransient},
		{"eof", io.EOF, ErrorTransient},
		{"unexpected eof", io.ErrUnexpectedEOF, ErrorTransient},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ErrorTransient},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), ErrorTransient},
		{"dns", &net.DNSError{Err: "no such host", Name: "db"}, ErrorTransient},
		{"not found", admin.ErrNotFound, ErrorPermanent},
		{"invalid input", admin.ErrInvalidInput, ErrorPermanent},
		{"plain", errors.New("boom"), ErrorPermanent},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("%s: ClassifyError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
